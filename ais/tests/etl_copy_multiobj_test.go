// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2021-2022, NVIDIA CORPORATION. All rights reserved.
 */
package integration

import (
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/etl"
	"github.com/NVIDIA/aistore/tools"
	"github.com/NVIDIA/aistore/tools/readers"
	"github.com/NVIDIA/aistore/tools/tassert"
	"github.com/NVIDIA/aistore/tools/tetl"
	"github.com/NVIDIA/aistore/tools/tlog"
	"github.com/NVIDIA/aistore/tools/trand"
)

// TODO -- FIXME: randomize range, check prefix for `xs.iteratePrefix`
// NOTE:          from is a cloud bucket, if exists
func TestCopyMultiObjSimple(t *testing.T) {
	const (
		copyCnt   = 20
		objSize   = 128
		cksumType = cos.ChecksumXXHash
	)
	var (
		objCnt     = 2345
		proxyURL   = tools.RandomProxyURL(t)
		bckFrom    cmn.Bck
		bckTo      = cmn.Bck{Name: "cp-range-to", Provider: apc.AIS}
		baseParams = tools.BaseAPIParams(proxyURL)
		xactID     string
		err        error
		exists     bool
	)
	if cliBck.IsRemote() {
		if exists, _ = tools.BucketExists(nil, proxyURL, cliBck); exists {
			bckFrom = cliBck
			objCnt = 40
		}
	}
	if !exists {
		bckFrom = cmn.Bck{Name: "cp-range-from", Provider: apc.AIS}
		tools.CreateBucketWithCleanup(t, proxyURL, bckFrom, nil)
	}
	objList := make([]string, 0, objCnt)
	tlog.Logf("exists = %t\n", exists)

	tools.CreateBucketWithCleanup(t, proxyURL, bckTo, nil)
	for i := 0; i < objCnt; i++ {
		objList = append(objList, fmt.Sprintf("test/a-%04d", i))
	}
	for i := 0; i < 5; i++ {
		tlog.Logf("PUT %d => %s\n", len(objList), bckFrom.DisplayName())
		for _, objName := range objList {
			r, _ := readers.NewRandReader(objSize, cksumType)
			err := api.PutObject(api.PutObjectArgs{
				BaseParams: baseParams,
				Bck:        bckFrom,
				Object:     objName,
				Reader:     r,
				Size:       objSize,
			})
			tassert.CheckFatal(t, err)
		}

		rangeStart := 10 // rand.Intn(objCnt - copyCnt - 1)
		template := "test/a-" + fmt.Sprintf("{%04d..%04d}", rangeStart, rangeStart+copyCnt-1)
		tlog.Logf("[%s] %s => %s\n", template, bckFrom.DisplayName(), bckTo.DisplayName())
		msg := cmn.TCObjsMsg{SelectObjsMsg: cmn.SelectObjsMsg{Template: template}, ToBck: bckTo}
		xactID, err = api.CopyMultiObj(baseParams, bckFrom, msg)
		tassert.CheckFatal(t, err)
	}

	wargs := api.XactReqArgs{ID: xactID, Kind: apc.ActCopyObjects}
	api.WaitForXactionIdle(baseParams, wargs)

	tlog.Logln("prefix: test/")
	msg := &apc.LsoMsg{Prefix: "test/"}
	lst, err := api.ListObjects(baseParams, bckTo, msg, 0)
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(lst.Entries) == copyCnt, "%d != %d", copyCnt, len(lst.Entries))
	rangeStart := 10 // rand.Intn(objCnt - copyCnt - 1)
	for i := rangeStart; i < rangeStart+copyCnt; i++ {
		objName := fmt.Sprintf("test/a-%04d", i)
		err := api.DeleteObject(baseParams, bckTo, objName)
		tassert.CheckError(t, err)
		tlog.Logf("%s/%s\n", bckTo.DisplayName(), objName)
	}
}

func TestCopyMultiObj(t *testing.T) {
	runProviderTests(t, func(t *testing.T, bck *cluster.Bck) {
		testCopyMobj(t, bck)
	})
}

func testCopyMobj(t *testing.T, bck *cluster.Bck) {
	const objCnt = 200
	var (
		proxyURL   = tools.RandomProxyURL(t)
		baseParams = tools.BaseAPIParams(proxyURL)

		m = ioContext{
			t:       t,
			bck:     bck.Clone(),
			num:     objCnt,
			prefix:  "copy-multiobj/",
			ordered: true,
		}
		toBck     = cmn.Bck{Name: trand.String(10), Provider: apc.AIS}
		numToCopy = cos.Min(m.num/2, 13)
		fmtRange  = "%s{%d..%d}"
		subtests  = []struct {
			list bool
		}{
			{true}, {false},
		}
	)
	for _, test := range subtests {
		tname := "list"
		if !test.list {
			tname = "range"
		}
		t.Run(tname, func(t *testing.T) {
			if m.bck.IsRemote() {
				m.num = objCnt / 3
			}
			m.initWithCleanup()
			m.puts()
			if m.bck.IsRemote() {
				defer m.del()
			}
			if !toBck.Equal(&m.bck) && toBck.IsAIS() {
				tools.CreateBucketWithCleanup(t, proxyURL, toBck, nil)
			}
			var erv atomic.Value
			if test.list {
				for i := 0; i < numToCopy && erv.Load() == nil; i++ {
					list := make([]string, 0, numToCopy)
					for j := 0; j < numToCopy; j++ {
						list = append(list, m.objNames[rand.Intn(m.num)])
					}
					go func(list []string) {
						msg := cmn.TCObjsMsg{SelectObjsMsg: cmn.SelectObjsMsg{ObjNames: list}, ToBck: toBck}
						if _, err := api.CopyMultiObj(baseParams, m.bck, msg); err != nil {
							erv.Store(err)
						}
					}(list)
				}
			} else {
				for i := 0; i < numToCopy && erv.Load() == nil; i++ {
					start := rand.Intn(m.num - numToCopy)
					go func(start int) {
						template := fmt.Sprintf(fmtRange, m.prefix, start, start+numToCopy-1)
						msg := cmn.TCObjsMsg{SelectObjsMsg: cmn.SelectObjsMsg{Template: template}, ToBck: toBck}
						if _, err := api.CopyMultiObj(baseParams, m.bck, msg); err != nil {
							erv.Store(err)
						}
					}(start)
				}
			}
			if erv.Load() != nil {
				tassert.CheckFatal(t, erv.Load().(error))
			}
			wargs := api.XactReqArgs{Kind: apc.ActCopyObjects, Bck: m.bck}
			api.WaitForXactionIdle(baseParams, wargs)

			msg := &apc.LsoMsg{Prefix: m.prefix}
			msg.AddProps(apc.GetPropsName, apc.GetPropsSize)
			objList, err := api.ListObjects(baseParams, toBck, msg, 0)
			tassert.CheckFatal(t, err)
			tlog.Logf("%s => %s: copied %d objects\n", m.bck, toBck, len(objList.Entries))
		})
	}
}

func TestETLMultiObj(t *testing.T) {
	tools.CheckSkip(t, tools.SkipTestArgs{RequiredDeployment: tools.ClusterTypeK8s})
	tetl.CheckNoRunningETLContainers(t, baseParams)

	const (
		objCnt      = 50
		copyCnt     = 20
		rangeStart  = 10
		transformer = tetl.MD5
		etlCommType = etl.Hpush
		objSize     = cos.KiB
		cksumType   = cos.ChecksumMD5
	)
	var (
		proxyURL   = tools.RandomProxyURL(t)
		baseParams = tools.BaseAPIParams(proxyURL)

		bck   = cmn.Bck{Name: "etloffline", Provider: apc.AIS}
		toBck = cmn.Bck{Name: "etloffline-out-" + trand.String(5), Provider: apc.AIS}
	)

	tools.CreateBucketWithCleanup(t, proxyURL, bck, nil)
	tools.CreateBucketWithCleanup(t, proxyURL, toBck, nil)

	for i := 0; i < objCnt; i++ {
		r, _ := readers.NewRandReader(objSize, cksumType)
		err := api.PutObject(api.PutObjectArgs{
			BaseParams: baseParams,
			Bck:        bck,
			Object:     fmt.Sprintf("test/a-%04d", i),
			Reader:     r,
			Size:       objSize,
		})
		tassert.CheckFatal(t, err)
	}

	uuid := tetl.Init(t, baseParams, transformer, etlCommType)
	t.Cleanup(func() { tetl.StopAndDeleteETL(t, baseParams, uuid) })

	for _, ty := range []string{"range", "list"} {
		t.Run(ty, func(t *testing.T) {
			testETLMultiObj(t, uuid, bck, toBck, "test/a-"+fmt.Sprintf("{%04d..%04d}", rangeStart, rangeStart+copyCnt-1), ty)
		})
	}
}

func testETLMultiObj(t *testing.T, uuid string, fromBck, toBck cmn.Bck, fileRange, opType string) {
	pt, err := cos.ParseBashTemplate(fileRange)
	tassert.CheckFatal(t, err)

	var (
		proxyURL   = tools.RandomProxyURL(t)
		baseParams = tools.BaseAPIParams(proxyURL)

		objList        = pt.ToSlice()
		objCnt         = len(objList)
		requestTimeout = 30 * time.Second
		tcoMsg         = cmn.TCObjsMsg{
			TCBMsg: apc.TCBMsg{
				ID:             uuid,
				RequestTimeout: cos.Duration(requestTimeout),
			},
			ToBck: toBck,
		}
	)
	if opType == "list" {
		tcoMsg.SelectObjsMsg.ObjNames = objList
	} else {
		tcoMsg.SelectObjsMsg.Template = fileRange
	}

	tlog.Logf("Start offline ETL %q\n", uuid)
	xactID, err := api.ETLMultiObj(baseParams, fromBck, tcoMsg)
	tassert.CheckFatal(t, err)

	wargs := api.XactReqArgs{ID: xactID, Kind: apc.ActETLObjects}
	err = api.WaitForXactionIdle(baseParams, wargs)
	tassert.CheckFatal(t, err)

	list, err := api.ListObjects(baseParams, toBck, nil, 0)
	tassert.CheckFatal(t, err)
	tassert.Errorf(t, len(list.Entries) == objCnt, "expected %d objects from offline ETL, got %d", objCnt, len(list.Entries))
	for _, objName := range objList {
		err := api.DeleteObject(baseParams, toBck, objName)
		tassert.CheckError(t, err)
		tlog.Logf("%s/%s\n", toBck.Name, objName)
	}
}