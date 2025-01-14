// Package xs is a collection of eXtended actions (xactions), including multi-object
// operations, list-objects, (cluster) rebalance and (target) resilver, ETL, and more.
/*
 * Copyright (c) 2021-2023, NVIDIA CORPORATION. All rights reserved.
 */
package xs

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cluster/meta"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/archive"
	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/xact"
	"github.com/NVIDIA/aistore/xact/xreg"
)

type (
	archFactory struct {
		streamingF
	}
	archwi struct { // archival work item; implements lrwi
		writer    archive.Writer
		r         *XactArch
		msg       *cmn.ArchiveMsg
		tsi       *meta.Snode
		lom       *cluster.LOM // of the archive
		fqn       string       // workFQN --/--
		fh        *os.File     // --/--
		cksum     cos.CksumHashSize
		appendPos int64 // append to existing archive
		// finishing
		refc       atomic.Int32
		finalizing atomic.Bool
	}
	XactArch struct {
		streamingX
		workCh  chan *cmn.ArchiveMsg
		config  *cmn.Config
		bckTo   *meta.Bck
		pending struct {
			m map[string]*archwi
			sync.RWMutex
		}
	}
)

// interface guard
var (
	_ cluster.Xact   = (*XactArch)(nil)
	_ xreg.Renewable = (*archFactory)(nil)
	_ lrwi           = (*archwi)(nil)
)

/////////////////
// archFactory //
/////////////////

func (*archFactory) New(args xreg.Args, bck *meta.Bck) xreg.Renewable {
	p := &archFactory{streamingF: streamingF{RenewBase: xreg.RenewBase{Args: args, Bck: bck}, kind: apc.ActArchive}}
	return p
}

func (p *archFactory) Start() error {
	workCh := make(chan *cmn.ArchiveMsg, maxNumInParallel)
	r := &XactArch{streamingX: streamingX{p: &p.streamingF}, workCh: workCh, config: cmn.GCO.Get()}
	r.pending.m = make(map[string]*archwi, maxNumInParallel)
	p.xctn = r
	r.DemandBase.Init(p.UUID(), apc.ActArchive, p.Bck /*from*/, 0 /*use default*/)

	bmd := p.Args.T.Bowner().Get()
	trname := fmt.Sprintf("arch-%s%s-%s-%d", p.Bck.Provider, p.Bck.Ns, p.Bck.Name, bmd.Version) // NOTE: (bmd.Version)
	if err := p.newDM(trname, r.recv, 0 /*pdu*/); err != nil {
		return err
	}
	r.p.dm.SetXact(r)
	r.p.dm.Open()

	xact.GoRunW(r)
	return nil
}

//////////////
// XactArch //
//////////////

func (r *XactArch) Begin(msg *cmn.ArchiveMsg) (err error) {
	lom := cluster.AllocLOM(msg.ArchName)
	if err = lom.InitBck(&msg.ToBck); err != nil {
		r.raiseErr(err, msg.ContinueOnError)
		return
	}
	debug.Assert(lom.Cname() == msg.Cname()) // relying on it

	wi := &archwi{r: r, msg: msg, lom: lom}
	wi.fqn = fs.CSM.Gen(wi.lom, fs.WorkfileType, fs.WorkfileCreateArch)
	wi.cksum.Init(lom.CksumType())

	smap := r.p.T.Sowner().Get()

	// TODO -- FIXME: assuming _this_ target is active; check TCO as well
	wi.refc.Store(int32(smap.CountActiveTs() - 1))

	wi.tsi, err = cluster.HrwTarget(msg.ToBck.MakeUname(msg.ArchName), smap)
	if err != nil {
		r.raiseErr(err, msg.ContinueOnError)
		return
	}

	// NOTE: creating archive at BEGIN time (see cleanup)
	if r.p.T.SID() == wi.tsi.ID() {
		var (
			lmfh        *os.File
			finfo, errX = os.Stat(wi.lom.FQN)
			exists      = errX == nil
		)
		if wi.msg.AppendToExisting {
			if !exists {
				return fmt.Errorf("%s: %s doesn't exist - cannot APPEND", r.p.T, msg.Cname())
			}
			lmfh, err = wi.beginAppend()
		} else {
			if exists {
				// PUT (new multi-object-arch version) semantics
				glog.Infof("%s: %s exists - proceeding to overwrite w/ new version", r.p.T, msg.Cname())
			}
			wi.fh, err = wi.lom.CreateFile(wi.fqn)
		}
		if err != nil {
			return
		}

		// construct format-specific writer
		wi.writer = archive.NewWriter(msg.Mime, wi.fh, &wi.cksum, true /*serialize*/)

		// append
		if lmfh != nil {
			err = wi.writer.Copy(lmfh, finfo.Size())
		}
	}

	// most of the time there'll be a single dst bucket for the lifetime
	// TODO: extend `cluster.Xact` for one-source-to-many-destination buckets
	if r.bckTo == nil {
		if from := r.Bck().Bucket(); !from.Equal(&wi.msg.ToBck) {
			r.bckTo = meta.CloneBck(&wi.msg.ToBck)
		}
	}

	r.pending.Lock()
	r.pending.m[msg.TxnUUID] = wi
	r.wiCnt.Inc()
	r.pending.Unlock()
	return
}

func (r *XactArch) Do(msg *cmn.ArchiveMsg) {
	r.IncPending()
	r.workCh <- msg
}

func (r *XactArch) Run(wg *sync.WaitGroup) {
	var err error
	glog.Infoln(r.Name())
	wg.Done()
	for {
		select {
		case msg := <-r.workCh:
			r.pending.RLock()
			wi, ok := r.pending.m[msg.TxnUUID]
			r.pending.RUnlock()
			if !ok {
				debug.Assert(!r.err.IsNil()) // see cleanup
				goto fin
			}
			var (
				smap    = r.p.T.Sowner().Get()
				lrit    = &lriterator{}
				freeLOM = false // not delegating to iterator
			)
			lrit.init(r, r.p.T, &msg.ListRange, freeLOM)
			if msg.IsList() {
				err = lrit.iterateList(wi, smap)
			} else {
				err = lrit.iterateRange(wi, smap)
				if err == cos.ErrEmptyTemplate {
					// motivation: archive the entire bucket
					err = lrit.iteratePrefix(smap, "" /*prefix*/, wi)
				}
			}
			if err == nil {
				err = r.AbortErr()
			}
			if err != nil {
				wi.abort()
				goto fin
			}
			if r.p.T.SID() == wi.tsi.ID() {
				wi.finalizing.Store(true)
				go r.finalize(wi) // NOTE async
			} else {
				r.sendTerm(wi.msg.TxnUUID, wi.tsi, nil)
				r.pending.Lock()
				delete(r.pending.m, msg.TxnUUID)
				r.wiCnt.Dec()
				r.pending.Unlock()
				r.DecPending()
			}
		case <-r.IdleTimer():
			goto fin
		case errCause := <-r.ChanAbort():
			if err == nil {
				err = errCause
			}
			goto fin
		}
	}
fin:
	if r.streamingX.fin(err, true /*unreg Rx*/) == nil {
		return
	}

	// [cleanup] close and rm unfinished archives (compare w/ `finalize`)
	r.pending.Lock()
	for uuid, wi := range r.pending.m {
		if wi.finalizing.Load() {
			continue
		}
		wi.abort()
		delete(r.pending.m, uuid)
	}
	r.pending.Unlock()
}

func (r *XactArch) doSend(lom *cluster.LOM, wi *archwi, fh cos.ReadOpenCloser) {
	o := transport.AllocSend()
	hdr := &o.Hdr
	{
		hdr.Bck = wi.msg.ToBck
		hdr.ObjName = lom.ObjName
		hdr.ObjAttrs.CopyFrom(lom.ObjAttrs())
		hdr.Opaque = []byte(wi.msg.TxnUUID)
	}
	o.Callback = func(_ transport.ObjHdr, _ io.ReadCloser, _ any, _ error) {
		cluster.FreeLOM(lom)
	}
	r.p.dm.Send(o, fh, wi.tsi)
}

func (r *XactArch) recv(hdr transport.ObjHdr, objReader io.Reader, err error) error {
	r.IncPending()
	defer func() {
		r.DecPending()
		transport.DrainAndFreeReader(objReader)
	}()
	if err != nil && !cos.IsEOF(err) {
		glog.Error(err)
		return err
	}

	txnUUID := string(hdr.Opaque)
	r.pending.RLock()
	wi, ok := r.pending.m[txnUUID]
	r.pending.RUnlock()
	if !ok {
		debug.Assert(!r.err.IsNil()) // see cleanup
		return r.err.Err()
	}
	debug.Assert(wi.tsi.ID() == r.p.T.SID() && wi.msg.TxnUUID == txnUUID)

	// NOTE: best-effort via ref-counting
	if hdr.Opcode == opcodeDone {
		refc := wi.refc.Dec()
		debug.Assert(refc >= 0)
		return nil
	}
	debug.Assert(hdr.Opcode == 0)
	err = wi.writer.Write(wi.nameInArch(hdr.ObjName), &hdr.ObjAttrs, objReader)
	if err != nil {
		r.raiseErr(err, wi.msg.ContinueOnError)
	}
	return nil
}

func (r *XactArch) finalize(wi *archwi) {
	if q := wi.quiesce(); q == cluster.QuiAborted {
		r.raiseErr(cmn.NewErrAborted(r.Name(), "", nil), wi.msg.ContinueOnError)
	} else if q == cluster.QuiTimeout {
		r.raiseErr(fmt.Errorf("%s: %v", r, cmn.ErrQuiesceTimeout), wi.msg.ContinueOnError)
	}

	r.pending.Lock()
	delete(r.pending.m, wi.msg.TxnUUID)
	r.wiCnt.Dec()
	r.pending.Unlock()

	errCode, err := r.fini(wi)
	r.DecPending()

	if err != nil {
		wi.abort()
		r.raiseErr(err, wi.msg.ContinueOnError, errCode)
	}
}

func (r *XactArch) fini(wi *archwi) (errCode int, err error) {
	var size int64
	wi.writer.Fini()
	if size, err = wi.finalize(); err != nil {
		return http.StatusInternalServerError, err
	}

	wi.lom.SetSize(size)
	cos.Close(wi.fh)
	wi.fh = nil
	errCode, err = r.p.T.FinalizeObj(wi.lom, wi.fqn, r) // cmn.OwtFinalize
	cluster.FreeLOM(wi.lom)

	r.ObjsAdd(1, size-wi.appendPos)
	return
}

func (r *XactArch) Name() (s string) {
	s = r.streamingX.Name()
	if src, dst := r.FromTo(); src != nil {
		s += " => " + dst.String()
	}
	return
}

func (r *XactArch) String() (s string) {
	s = r.streamingX.String() + " => "
	if r.wiCnt.Load() > 0 && r.bckTo != nil {
		s += r.bckTo.String()
	}
	return
}

func (r *XactArch) FromTo() (src, dst *meta.Bck) {
	if r.bckTo != nil {
		src, dst = r.Bck(), r.bckTo
	}
	return
}

func (r *XactArch) Snap() (snap *cluster.Snap) {
	snap = &cluster.Snap{}
	r.ToSnap(snap)

	snap.IdleX = r.IsIdle()
	if f, t := r.FromTo(); f != nil {
		snap.SrcBck, snap.DstBck = f.Clone(), t.Clone()
	}
	return
}

////////////
// archwi //
////////////

func (wi *archwi) beginAppend() (lmfh *os.File, err error) {
	msg := wi.msg
	if msg.Mime == archive.ExtTar {
		if err = wi.openTarForAppend(); err == nil || err != archive.ErrTarIsEmpty {
			return
		}
	}
	switch msg.Mime {
	case archive.ExtTar, archive.ExtTgz, archive.ExtTarTgz, archive.ExtZip:
		// to copy `lmfh` --> `wi.fh` with subsequent APPEND-ing
		lmfh, err = os.Open(wi.lom.FQN)
		if err != nil {
			return
		}
		if wi.fh, err = wi.lom.CreateFile(wi.fqn); err != nil {
			cos.Close(lmfh)
			lmfh = nil
		}
	default: // TODO -- FIXME: add .msgpack
		err = fmt.Errorf("cannot APPEND to %s - %q not implemented yet", msg.Cname(), msg.Mime)
	}
	return
}

func (wi *archwi) openTarForAppend() (err error) {
	if err = os.Rename(wi.lom.FQN, wi.fqn); err != nil {
		return
	}
	wi.fh, err = archive.OpenTarSeekEnd(wi.lom.ObjName, wi.fqn)
	if err != nil {
		goto roll
	}
	wi.appendPos, err = wi.fh.Seek(0, io.SeekCurrent)
	if err == nil {
		return // ok
	}
	cos.Close(wi.fh)
	wi.fh = nil
roll:
	if errV := wi.lom.RenameFrom(wi.fqn); errV != nil {
		glog.Errorf("%s: nested error: failed to append %s (%v) and rename back from %s (%v)",
			wi.tsi, wi.lom, err, wi.fqn, errV)
	} else {
		wi.fqn = ""
	}
	return
}

func (wi *archwi) do(lom *cluster.LOM, lrit *lriterator) {
	var coldGet bool
	if err := lom.Load(false /*cache it*/, false /*locked*/); err != nil {
		if !cmn.IsObjNotExist(err) {
			wi.r.raiseErr(err, wi.msg.ContinueOnError)
			return
		}
		coldGet = lom.Bck().IsRemote()
		if !coldGet {
			wi.r.raiseErr(err, wi.msg.ContinueOnError)
			return
		}
	}
	t := lrit.t
	if coldGet {
		// cold
		if errCode, err := t.GetCold(lrit.ctx, lom, cmn.OwtGetLock); err != nil {
			if errCode == http.StatusNotFound || cmn.IsObjNotExist(err) {
				return
			}
			wi.r.raiseErr(err, wi.msg.ContinueOnError)
			return
		}
	}

	fh, err := cos.NewFileHandle(lom.FQN)
	debug.AssertNoErr(err)
	if err != nil {
		wi.r.raiseErr(err, wi.msg.ContinueOnError)
		return
	}
	if t.SID() != wi.tsi.ID() {
		wi.r.doSend(lom, wi, fh)
		return
	}
	debug.Assert(wi.fh != nil) // see Begin
	err = wi.writer.Write(wi.nameInArch(lom.ObjName), lom, fh)
	cluster.FreeLOM(lom)
	cos.Close(fh)
	if err != nil {
		wi.r.raiseErr(err, wi.msg.ContinueOnError)
	}
}

func (wi *archwi) quiesce() cluster.QuiRes {
	return wi.r.Quiesce(cmn.Timeout.MaxKeepalive(), func(total time.Duration) cluster.QuiRes {
		return xact.RefcntQuiCB(&wi.refc, wi.r.config.Timeout.SendFile.D()/2, total)
	})
}

func (wi *archwi) nameInArch(objName string) string {
	if !wi.msg.InclSrcBname {
		return objName
	}
	buf := make([]byte, 0, len(wi.msg.FromBckName)+1+len(objName))
	buf = append(buf, wi.msg.FromBckName...)
	buf = append(buf, filepath.Separator)
	buf = append(buf, objName...)
	return cos.UnsafeS(buf)
}

func (wi *archwi) abort() {
	if wi.fh != nil {
		cos.Close(wi.fh)
		wi.fh = nil
	}
	if wi.fqn != "" {
		cos.RemoveFile(wi.fqn)
		wi.fqn = ""
	}
}

func (wi *archwi) finalize() (size int64, err error) {
	var cksum *cos.Cksum
	if wi.appendPos > 0 {
		var st os.FileInfo
		if st, err = os.Stat(wi.fqn); err == nil {
			size = st.Size()
		}
		cksum = cos.NewCksum(cos.ChecksumNone, "")
	} else {
		wi.cksum.Finalize()
		cksum = &wi.cksum.Cksum
		size = wi.cksum.Size
	}
	wi.lom.SetCksum(cksum)
	return size, err
}
