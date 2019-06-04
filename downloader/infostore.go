// Package downloader implements functionality to download resources into AIS cluster from external source.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package downloader

import (
	"fmt"
	"regexp"
	"sync"

	"github.com/NVIDIA/aistore/3rdparty/atomic"

	"github.com/NVIDIA/aistore/3rdparty/glog"
)

type (
	infoStore struct {
		*downloaderDB
		jobInfo map[string]*DownloadJobInfo
		sync.RWMutex
	}
)

func newInfoStore() (*infoStore, error) {
	db, err := newDownloadDB()
	if err != nil {
		return nil, err
	}

	return &infoStore{
		downloaderDB: db,
		jobInfo:      make(map[string]*DownloadJobInfo),
	}, nil
}

func (is *infoStore) getJob(id string) (*DownloadJobInfo, error) {
	is.RLock()
	defer is.RUnlock()

	if ji, ok := is.jobInfo[id]; ok {
		return ji, nil
	}

	return nil, fmt.Errorf("job %s not found", id)
}

func (is *infoStore) getList(descRegex *regexp.Regexp) []*DownloadJobInfo {
	jobsInfo := make([]*DownloadJobInfo, 0)

	is.RLock()
	for _, dji := range is.jobInfo {
		if descRegex == nil || descRegex.MatchString(dji.Description) {
			jobsInfo = append(jobsInfo, dji)
		}
	}
	is.RUnlock()

	return jobsInfo
}

func (is *infoStore) setJob(id string, job DownloadJob) {
	jInfo := &DownloadJobInfo{
		ID:            job.ID(),
		Bucket:        job.Bucket(),
		BckProvider:   job.BckProvider(),
		Total:         job.Len(),
		Description:   job.Description(),
		Aborted:       atomic.NewBool(false),
		AllDispatched: atomic.NewBool(false),
		ScheduledCnt:  atomic.NewInt32(0),
		FinishedCnt:   atomic.NewInt32(0),
	}

	is.Lock()
	is.jobInfo[id] = jInfo
	is.Unlock()
}

func (is *infoStore) incFinished(id string) error {
	jInfo, err := is.getJob(id)
	if err != nil {
		glog.Error(err)
		return err
	}

	jInfo.FinishedCnt.Inc()
	return nil
}

func (is *infoStore) incScheduled(id string) error {
	jInfo, err := is.getJob(id)
	if err != nil {
		glog.Error(err)
		return err
	}

	jInfo.ScheduledCnt.Inc()
	is.jobInfo[id] = jInfo
	return nil
}

func (is *infoStore) setAllDispatched(id string, dispatched bool) error {
	jInfo, err := is.getJob(id)
	if err != nil {
		glog.Error(err)
		return err
	}

	jInfo.AllDispatched.Store(dispatched)
	is.jobInfo[id] = jInfo
	return nil
}

func (is *infoStore) setAborted(id string) error {
	jInfo, err := is.getJob(id)
	if err != nil {
		glog.Error(err)
		return err
	}

	jInfo.Aborted.Store(true)
	return nil
}

func (is *infoStore) delJob(id string) {
	delete(is.jobInfo, id)
}
