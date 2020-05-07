// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package integration

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/downloader"
	"github.com/NVIDIA/aistore/tutils"
	"github.com/NVIDIA/aistore/tutils/readers"
	"github.com/NVIDIA/aistore/tutils/tassert"
)

const (
	downloadDescAllPrefix = "downloader-test-integration"
	downloadDescAllRegex  = "^" + downloadDescAllPrefix
)

var (
	downloadDescCurPrefix = fmt.Sprintf("%s-%d-", downloadDescAllPrefix, os.Getpid())
)

func generateDownloadDesc() string {
	return downloadDescCurPrefix + time.Now().Format(time.RFC3339Nano)
}

func clearDownloadList(t *testing.T) {
	var (
		httpErr = &cmn.HTTPError{}
	)
while503:
	listDownload, err := api.DownloadGetList(tutils.BaseAPIParams(), downloadDescAllRegex)
	if err != nil && errors.As(err, &httpErr) && httpErr.Status == http.StatusServiceUnavailable {
		tutils.Logln("waiting for the cluster to start up...")
		time.Sleep(time.Second)
		goto while503
	}
	tassert.CheckFatal(t, err)

	for _, v := range listDownload {
		if v.JobRunning() {
			tutils.Logf("Canceling: %v...\n", v.ID)
			err := api.DownloadAbort(tutils.BaseAPIParams(), v.ID)
			tassert.CheckFatal(t, err)
		}
	}

	time.Sleep(time.Millisecond * 300)

	for _, v := range listDownload {
		tutils.Logf("Removing: %v...\n", v.ID)
		err := api.DownloadRemove(tutils.BaseAPIParams(), v.ID)
		tassert.CheckFatal(t, err)
	}
}

func checkDownloadList(t *testing.T, expNumEntries ...int) {
	defer clearDownloadList(t)

	expNumEntriesVal := 1
	if len(expNumEntries) > 0 {
		expNumEntriesVal = expNumEntries[0]
	}

	listDownload, err := api.DownloadGetList(tutils.BaseAPIParams(), downloadDescAllRegex)
	tassert.CheckFatal(t, err)
	actEntries := len(listDownload)

	if expNumEntriesVal != actEntries {
		t.Fatalf("Incorrect # of downloader entries: expected %d, actual %d", expNumEntriesVal, actEntries)
	}
}

func waitForDownload(t *testing.T, id string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)

	for {
		if time.Now().After(deadline) {
			t.Errorf("Timed out waiting for download %s.", id)
			return
		}

		all := true
		if resp, err := api.DownloadStatus(tutils.BaseAPIParams(), id); err == nil {
			if !resp.JobFinished() {
				all = false
			}
		}

		if all {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func downloaderCompleted(t *testing.T, targetID string, targetsStats api.NodesXactStats) bool {
	downloaderStat, exists := targetsStats[targetID]
	for _, xaction := range downloaderStat {
		if xaction.Running() {
			tutils.Logf("%s in progress for %s\n", xaction.Kind(), targetID)
			return false
		}
	}

	tassert.Fatalf(t, exists, "target %s not found in downloader stats", targetID)
	return true
}

func waitForDownloaderToFinish(t *testing.T, baseParams api.BaseParams, targetID string, timeouts ...time.Duration) {
	start := time.Now()
	timeout := time.Duration(0)
	if len(timeouts) > 0 {
		timeout = timeouts[0]
	}

	tutils.Logf("waiting %s for downloader to finish\n", timeout)
	time.Sleep(time.Second * 2)

	xactArgs := api.XactReqArgs{Kind: cmn.ActDownload}
	for {
		time.Sleep(time.Second)
		downloaderStats, err := api.GetXactionStats(baseParams, xactArgs)
		tassert.CheckFatal(t, err)

		if downloaderCompleted(t, targetID, downloaderStats) {
			tutils.Logf("downloader has finished\n")
			return
		}

		if timeout.Nanoseconds() != 0 && time.Since(start) > timeout {
			tassert.Fatalf(t, false, "downloader has not finished before %s", timeout)
			return
		}
	}
}

func downloadObject(t *testing.T, bck cmn.Bck, objName, link string) { // nolint:unparam // it's better to keep link as parameter
	id, err := api.DownloadSingle(tutils.BaseAPIParams(), generateDownloadDesc(), bck, objName, link)
	tassert.CheckError(t, err)
	waitForDownload(t, id, 20*time.Second)
}

func verifyProps(t *testing.T, bck cmn.Bck, objName string, size int64, version string) *cmn.ObjectProps {
	objProps, err := api.HeadObject(tutils.BaseAPIParams(), bck, objName)
	tassert.CheckError(t, err)

	tassert.Errorf(
		t, objProps.Size == size,
		"size mismatch (%d vs %d)", objProps.Size, size,
	)
	tassert.Errorf(
		t, objProps.Version == version,
		"version mismatch (%s vs %s)", objProps.Version, version,
	)
	return objProps
}

func TestDownloadSingle(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		proxyURL      = tutils.RandomProxyURL()
		baseParams    = tutils.BaseAPIParams(proxyURL)
		objName       = "object"
		objNameSecond = "object-second"

		// links below don't contain protocols to test that no error occurs
		// in case they are missing.
		link      = "storage.googleapis.com/lpr-vision/imagenet/imagenet_train-000001.tgz"
		linkSmall = "github.com/NVIDIA/aistore"
	)

	clearDownloadList(t)

	// Create ais bucket
	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadSingle(baseParams, generateDownloadDesc(), bck, objName, link)
	tassert.CheckError(t, err)

	time.Sleep(time.Second)

	// Schedule second object
	idSecond, err := api.DownloadSingle(baseParams, generateDownloadDesc(), bck, objNameSecond, link)
	tassert.CheckError(t, err)

	// Cancel second object
	err = api.DownloadAbort(baseParams, idSecond)
	tassert.CheckError(t, err)

	resp, err := api.DownloadStatus(baseParams, id)
	tassert.CheckError(t, err)

	err = api.DownloadAbort(baseParams, id)
	tassert.CheckError(t, err)

	time.Sleep(time.Second)

	if resp, err = api.DownloadStatus(baseParams, id); err != nil {
		t.Errorf("got error when getting status for link that is not being downloaded: %v", err)
	} else if !resp.Aborted {
		t.Errorf("canceled link not marked: %v", resp)
	}

	if err = api.DownloadAbort(baseParams, id); err != nil {
		t.Errorf("got error when canceling second time: %v", err)
	}

	if err = api.DownloadRemove(baseParams, id); err != nil {
		t.Errorf("got error when removing task: %v", err)
	}

	if err = api.DownloadRemove(baseParams, id); err == nil {
		t.Errorf("expected error when removing non-existent task")
	}

	id, err = api.DownloadSingle(baseParams, generateDownloadDesc(), bck, objName, linkSmall)
	tassert.CheckError(t, err)

	waitForDownload(t, id, 30*time.Second)

	objs, err := tutils.ListObjects(proxyURL, bck, "", 0)
	tassert.CheckError(t, err)
	if len(objs) != 1 || objs[0] != objName {
		t.Errorf("expected single object (%s), got: %s", objName, objs)
	}

	// If the file was successfully downloaded, it means that its checksum was correct

	checkDownloadList(t, 2)
}

func TestDownloadRange(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)

		template = "storage.googleapis.com/lpr-vision/imagenet/imagenet_train-{000000..000007}.tgz"
	)

	clearDownloadList(t)

	// Create ais bucket
	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadRange(baseParams, generateDownloadDesc(), bck, template)
	tassert.CheckFatal(t, err)

	time.Sleep(3 * time.Second)

	err = api.DownloadAbort(baseParams, id)
	tassert.CheckFatal(t, err)

	checkDownloadList(t)
}

func TestDownloadMultiRange(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)

		template = "storage.googleapis.com/lpr-imagenet-augmented/imagenet_train-{0000..0007}-{001..009}.tgz"
	)

	clearDownloadList(t)

	// Create ais bucket
	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadRange(baseParams, generateDownloadDesc(), bck, template)
	tassert.CheckFatal(t, err)

	time.Sleep(3 * time.Second)

	err = api.DownloadAbort(baseParams, id)
	tassert.CheckFatal(t, err)

	checkDownloadList(t)
}

func TestDownloadMultiMap(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		m = map[string]string{
			"ais": "https://raw.githubusercontent.com/NVIDIA/aistore/master/README.md",
			"k8s": "https://raw.githubusercontent.com/kubernetes/kubernetes/master/README.md",
		}
		proxyURL = tutils.RandomProxyURL()
	)

	clearDownloadList(t)

	// Create ais bucket
	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadMulti(tutils.BaseAPIParams(), generateDownloadDesc(), bck, m)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, 10*time.Second)

	objs, err := tutils.ListObjects(proxyURL, bck, "", 0)
	tassert.CheckFatal(t, err)
	if len(objs) != len(m) {
		t.Errorf("expected objects (%s), got: %s", m, objs)
	}

	checkDownloadList(t)
}

func TestDownloadMultiList(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		l = []string{
			"https://raw.githubusercontent.com/NVIDIA/aistore/master/README.md",
			"https://raw.githubusercontent.com/kubernetes/kubernetes/master/LICENSE?query=values",
		}
		expectedObjs = []string{"LICENSE", "README.md"}
		proxyURL     = tutils.RandomProxyURL()
		baseParams   = tutils.BaseAPIParams(proxyURL)
	)

	clearDownloadList(t)

	// Create ais bucket
	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadMulti(baseParams, generateDownloadDesc(), bck, l)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, 10*time.Second)

	objs, err := tutils.ListObjects(proxyURL, bck, "", 0)
	tassert.CheckFatal(t, err)
	if !reflect.DeepEqual(objs, expectedObjs) {
		t.Errorf("expected objs: %s, got: %s", expectedObjs, objs)
	}

	checkDownloadList(t)
}

func TestDownloadTimeout(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		objName    = "object"
		link       = "https://storage.googleapis.com/lpr-vision/imagenet/imagenet_train-000001.tgz"
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
	)

	clearDownloadList(t)

	// Create ais bucket
	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	body := downloader.DlSingleBody{
		DlSingleObj: downloader.DlSingleObj{
			ObjName: objName,
			Link:    link,
		},
	}
	body.Bck = bck
	body.Description = generateDownloadDesc()
	body.Timeout = "1ms" // super small timeout to see if the request will be canceled

	id, err := api.DownloadSingleWithParam(baseParams, body)
	tassert.CheckFatal(t, err)

	time.Sleep(time.Second)

	if _, err := api.DownloadStatus(baseParams, id); err == nil {
		// TODO: we should get response that some files has been canceled or not finished.
		// For now we cannot do that since we don't collect information about
		// task being canceled.
		// t.Errorf("expected error when getting status for link that is not being downloaded: %s", string(resp))
		tutils.Logf("%v\n", err)
	}

	objs, err := tutils.ListObjects(proxyURL, bck, "", 0)
	tassert.CheckFatal(t, err)
	if len(objs) != 0 {
		t.Errorf("expected 0 objects, got: %s", objs)
	}

	checkDownloadList(t)
}

func TestDownloadCloud(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     clibucket,
			Provider: cmn.AnyCloud,
		}

		fileCnt = 5
		prefix  = "imagenet/imagenet_train-"
		suffix  = ".tgz"
	)

	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true, Cloud: true, Bck: bck})

	clearDownloadList(t)

	tutils.CleanCloudBucket(t, proxyURL, bck, prefix)
	defer tutils.CleanCloudBucket(t, proxyURL, bck, prefix)

	expectedObjs := make([]string, 0, fileCnt)
	for i := 0; i < fileCnt; i++ {
		reader, err := readers.NewRandReader(cmn.MiB, false /* withHash */)
		tassert.CheckFatal(t, err)

		objName := fmt.Sprintf("%s%0*d%s", prefix, 5, i, suffix)
		err = api.PutObject(api.PutObjectArgs{
			BaseParams: baseParams,
			Bck:        bck,
			Object:     objName,
			Reader:     reader,
		})
		tassert.CheckFatal(t, err)

		expectedObjs = append(expectedObjs, objName)
	}

	// Test download
	err := api.EvictList(baseParams, bck, expectedObjs)
	tassert.CheckFatal(t, err)
	xactArgs := api.XactReqArgs{Kind: cmn.ActEvictObjects, Bck: bck, Timeout: rebalanceTimeout}
	err = api.WaitForXaction(baseParams, xactArgs)
	tassert.CheckFatal(t, err)

	id, err := api.DownloadCloud(baseParams, generateDownloadDesc(), bck, prefix, suffix)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, time.Minute)

	objs, err := tutils.ListObjects(proxyURL, bck, prefix, 0)
	tassert.CheckFatal(t, err)
	if !reflect.DeepEqual(objs, expectedObjs) {
		t.Errorf("expected objs: %s, got: %s", expectedObjs, objs)
	}

	// Test cancellation
	err = api.EvictList(baseParams, bck, expectedObjs)
	tassert.CheckFatal(t, err)
	err = api.WaitForXaction(baseParams, xactArgs)
	tassert.CheckFatal(t, err)

	id, err = api.DownloadCloud(baseParams, generateDownloadDesc(), bck, prefix, suffix)
	tassert.CheckFatal(t, err)

	time.Sleep(200 * time.Millisecond)

	err = api.DownloadAbort(baseParams, id)
	tassert.CheckFatal(t, err)

	resp, err := api.DownloadStatus(baseParams, id)
	tassert.CheckFatal(t, err)
	if !resp.Aborted {
		t.Errorf("canceled cloud download %v not marked", id)
	}

	checkDownloadList(t, 2)
}

func TestDownloadStatus(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		baseParams    = tutils.BaseAPIParams()
		shortFileName = "shortFile"
		m             = ioContext{t: t}
	)

	m.saveClusterState()
	if m.originalTargetCount < 2 {
		t.Errorf("At least 2 targets are required.")
		return
	}

	longFileName := tutils.GenerateNotConflictingObjectName(shortFileName, "longFile", bck, m.smap)

	files := map[string]string{
		shortFileName: "https://raw.githubusercontent.com/NVIDIA/aistore/master/README.md",
		longFileName:  "https://storage.googleapis.com/lpr-vision/imagenet/imagenet_train-000001.tgz",
	}

	clearDownloadList(t)

	// Create ais bucket
	tutils.CreateFreshBucket(t, m.proxyURL, bck)
	defer tutils.DestroyBucket(t, m.proxyURL, bck)

	id, err := api.DownloadMulti(baseParams, generateDownloadDesc(), bck, files)
	tassert.CheckFatal(t, err)

	// Wait for the short file to be downloaded
	err = tutils.WaitForObjectToBeDowloaded(baseParams, bck, shortFileName, 5*time.Second)
	tassert.CheckFatal(t, err)

	resp, err := api.DownloadStatus(baseParams, id)
	tassert.CheckFatal(t, err)

	if resp.Total != 2 {
		t.Errorf("expected %d objects, got %d", 2, resp.Total)
	}
	if resp.FinishedCnt != 1 {
		t.Errorf("expected the short file to be downloaded")
	}
	if len(resp.CurrentTasks) != 1 {
		t.Fatal("did not expect the long file to be already downloaded")
	}
	if resp.CurrentTasks[0].Name != longFileName {
		t.Errorf("invalid file name in status message, expected: %s, got: %s", longFileName, resp.CurrentTasks[0].Name)
	}

	checkDownloadList(t)
}

func TestDownloadStatusError(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		files = map[string]string{
			"invalidURL":   "http://some.invalid.url",
			"notFoundFile": "https://google.com/404.tar",
		}

		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
	)

	clearDownloadList(t)

	// Create ais bucket
	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadMulti(baseParams, generateDownloadDesc(), bck, files)
	tassert.CheckFatal(t, err)

	// Wait to make sure both files were processed by downloader
	waitForDownload(t, id, 10*time.Second)

	resp, err := api.DownloadStatus(baseParams, id)
	tassert.CheckFatal(t, err)

	if resp.Total != len(files) {
		t.Errorf("expected %d objects, got %d", len(files), resp.Total)
	}
	if resp.FinishedCnt != 0 {
		t.Errorf("expected 0 files to be finished")
	}
	if resp.ErrorCnt != len(files) {
		t.Fatalf("expected 2 downloading errors, but got: %d errors: %v", len(resp.Errs), resp.Errs)
	}

	invalidAddressCausedError := resp.Errs[0].Name == "invalidURL" || resp.Errs[1].Name == "invalidURL"
	notFoundFileCausedError := resp.Errs[0].Name == "notFoundFile" || resp.Errs[1].Name == "notFoundFile"

	if !(invalidAddressCausedError && notFoundFileCausedError) {
		t.Errorf("expected objects that cause errors to be (%s, %s), but got: (%s, %s)",
			"invalidURL", "notFoundFile", resp.Errs[0].Name, resp.Errs[1].Name)
	}

	checkDownloadList(t)
}

func TestDownloadSingleValidExternalAndInternalChecksum(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)

		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		objNameFirst  = "object-first"
		objNameSecond = "object-second"

		linkFirst  = "https://storage.googleapis.com/lpr-vision/cifar10_test.tgz"
		linkSecond = "github.com/NVIDIA/aistore"

		expectedObjects = []string{objNameFirst, objNameSecond}
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	err := api.SetBucketProps(baseParams, bck, cmn.BucketPropsToUpdate{
		Cksum: &cmn.CksumConfToUpdate{ValidateWarmGet: api.Bool(true)},
	})
	tassert.CheckFatal(t, err)

	id, err := api.DownloadSingle(baseParams, generateDownloadDesc(), bck, objNameFirst, linkFirst)
	tassert.CheckError(t, err)
	id2, err := api.DownloadSingle(baseParams, generateDownloadDesc(), bck, objNameSecond, linkSecond)
	tassert.CheckError(t, err)

	waitForDownload(t, id, 20*time.Second)
	waitForDownload(t, id2, 5*time.Second)

	// If the file was successfully downloaded, it means that the external checksum was correct. Also because of the
	// ValidateWarmGet property being set to True, if it was downloaded without errors then the internal checksum was
	// also set properly
	tutils.EnsureObjectsExist(t, baseParams, bck, expectedObjects...)
}

func TestDownloadMultiValidExternalAndInternalChecksum(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)

		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		objNameFirst  = "linkFirst"
		objNameSecond = "linkSecond"

		m = map[string]string{
			"linkFirst":  "https://storage.googleapis.com/lpr-vision/cifar10_test.tgz",
			"linkSecond": "github.com/NVIDIA/aistore",
		}

		expectedObjects = []string{objNameFirst, objNameSecond}
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	err := api.SetBucketProps(baseParams, bck, cmn.BucketPropsToUpdate{
		Cksum: &cmn.CksumConfToUpdate{ValidateWarmGet: api.Bool(true)},
	})
	tassert.CheckFatal(t, err)

	id, err := api.DownloadMulti(baseParams, generateDownloadDesc(), bck, m)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, 30*time.Second)

	// If the file was successfully downloaded, it means that the external checksum was correct. Also because of the
	// ValidateWarmGet property being set to True, if it was downloaded without errors then the internal checksum was
	// also set properly
	tutils.EnsureObjectsExist(t, baseParams, bck, expectedObjects...)
}

func TestDownloadRangeValidExternalAndInternalChecksum(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)

		bck = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		template = "storage.googleapis.com/lpr-vision/cifar{10..100..90}_test.tgz"

		expectedObjects = []string{"cifar10_test.tgz", "cifar100_test.tgz"}
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	err := api.SetBucketProps(baseParams, bck, cmn.BucketPropsToUpdate{
		Cksum: &cmn.CksumConfToUpdate{ValidateWarmGet: api.Bool(true)},
	})
	tassert.CheckFatal(t, err)

	id, err := api.DownloadRange(baseParams, generateDownloadDesc(), bck, template)
	tassert.CheckFatal(t, err)

	waitForDownload(t, id, time.Minute)

	// If the file was successfully downloaded, it means that the external checksum was correct. Also because of the
	// ValidateWarmGet property being set to True, if it was downloaded without errors then the internal checksum was
	// also set properly
	tutils.EnsureObjectsExist(t, baseParams, bck, expectedObjects...)
}

func TestDownloadIntoNonexistentBucket(t *testing.T) {
	var (
		baseParams = tutils.BaseAPIParams()
		objName    = "object"
		obj        = "storage.googleapis.com/lpr-vision/imagenet/imagenet_train-000001.tgz"
	)

	bucket, err := tutils.GenerateNonexistentBucketName("download", baseParams)
	tassert.CheckFatal(t, err)

	bck := cmn.Bck{
		Name:     bucket,
		Provider: cmn.ProviderAIS,
	}
	_, err = api.DownloadSingle(baseParams, generateDownloadDesc(), bck, objName, obj)
	if err == nil {
		t.Fatalf("Expected an error, but go no errors.")
	}
	httpErr, ok := err.(*cmn.HTTPError)
	if !ok {
		t.Fatalf("Expected an error of type *cmn.HTTPError, but got: %T.", err)
	}
	if httpErr.Status != http.StatusNotFound {
		t.Errorf("Expected status: %d, got: %d.", http.StatusNotFound, httpErr.Status)
	}
}

func TestDownloadMpathEvents(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     TestBucketName,
			Provider: cmn.ProviderAIS,
		}
		objsCnt = 100

		template = "storage.googleapis.com/lpr-vision/imagenet/imagenet_train-{000000..000050}.tgz"
		m        = make(map[string]string, objsCnt)
	)

	// prepare objects to be downloaded to targets. Multiple objects to make sure that at least
	// one of them gets into target with disabled mountpath
	for i := 0; i < objsCnt; i++ {
		m[strconv.FormatInt(int64(i), 10)] = "https://raw.githubusercontent.com/NVIDIA/aistore/master/README.md"
	}

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	id, err := api.DownloadRange(baseParams, generateDownloadDesc(), bck, template)
	tassert.CheckFatal(t, err)
	tutils.Logf("Started large download job %s, meant to be aborted\n", id)

	smap := tutils.GetClusterMap(t, proxyURL)
	removeTarget := tutils.ExtractTargetNodes(smap)[0]

	mpathList, err := api.GetMountpaths(baseParams, removeTarget)
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(mpathList.Available) >= 2, "%s requires 2 or more mountpaths", t.Name())

	mpathID := cmn.NowRand().Intn(len(mpathList.Available))
	removeMpath := mpathList.Available[mpathID]
	tutils.Logf("Disabling a mountpath %s at target: %s\n", removeMpath, removeTarget.ID())
	err = api.DisableMountpath(baseParams, removeTarget.ID(), removeMpath)
	tassert.CheckFatal(t, err)

	defer func() {
		// Enable mountpah
		tutils.Logf("Enabling mountpath %s at target %s...\n", removeMpath, removeTarget.ID())
		err = api.EnableMountpath(baseParams, removeTarget, removeMpath)
		tassert.CheckFatal(t, err)
	}()

	// wait until downloader is aborted
	waitForDownloaderToFinish(t, baseParams, removeTarget.ID(), time.Second*30)
	// downloader finished on required target, safe to abort the rest
	tutils.Logf("Aborting download job %s\n", id)
	err = api.DownloadAbort(baseParams, id)

	objs, err := tutils.ListObjects(proxyURL, bck, "", 0)
	tassert.CheckError(t, err)
	tassert.Fatalf(t, len(objs) == 0, "objects should not have been downloaded, download should have been aborted\n")

	id, err = api.DownloadMulti(baseParams, generateDownloadDesc(), bck, m)
	tassert.CheckFatal(t, err)
	tutils.Logf("Started download job %s, waiting for it to finish\n", id)

	waitForDownload(t, id, 2*time.Minute)
	objs, err = tutils.ListObjects(proxyURL, bck, "", 0)
	tassert.CheckError(t, err)
	tassert.Fatalf(t, len(objs) == objsCnt, "Expected %d objects to be present, got: %d", objsCnt, len(objs)) // 21: from cifar10.tgz to cifar30.tgz
}

// NOTE: Test may fail if the content (or version) of the link changes
func TestDownloadOverrideObject(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}

		objName = cmn.RandString(10)
		link    = "https://storage.googleapis.com/minikube/iso/minikube-v0.23.2.iso.sha256"

		expectedSize    int64 = 65
		expectedVersion       = "1503349750687573"
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	downloadObject(t, bck, objName, link)
	oldProps := verifyProps(t, bck, objName, expectedSize, expectedVersion)

	// Update the file
	r, _ := readers.NewRandReader(10, true)
	err := api.PutObject(api.PutObjectArgs{
		BaseParams: baseParams,
		Bck:        bck,
		Object:     objName,
		Hash:       r.XXHash(),
		Reader:     r,
	})
	tassert.CheckFatal(t, err)
	verifyProps(t, bck, objName, 10, "1503349750687574")

	downloadObject(t, bck, objName, link)
	newProps := verifyProps(t, bck, objName, expectedSize, expectedVersion)
	tassert.Errorf(
		t, oldProps.Atime != newProps.Atime,
		"atime mismatch (%v vs %v)", oldProps.Atime, newProps.Atime,
	)
}

// NOTE: Test may fail if the content (or version) of the link changes
func TestDownloadSkipObject(t *testing.T) {
	var (
		proxyURL = tutils.RandomProxyURL()
		bck      = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}

		objName = cmn.RandString(10)
		link    = "https://storage.googleapis.com/minikube/iso/minikube-v0.23.2.iso.sha256"

		expectedSize    int64 = 65
		expectedVersion       = "1503349750687573"
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	downloadObject(t, bck, objName, link)
	oldProps := verifyProps(t, bck, objName, expectedSize, expectedVersion)

	downloadObject(t, bck, objName, link)
	newProps := verifyProps(t, bck, objName, expectedSize, expectedVersion)
	tassert.Errorf(
		t, oldProps.Atime == newProps.Atime,
		"atime mismatch (%v vs %v)", oldProps.Atime, newProps.Atime,
	)
}

func TestDownloadJobLimitConnections(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: true})

	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}

		template = "https://storage.googleapis.com/lpr-vision/imagenet/imagenet_train-{000001..0000140}.tgz"
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	smap, err := api.GetClusterMap(baseParams)
	tassert.CheckFatal(t, err)

	id, err := api.DownloadRangeWithParam(baseParams, downloader.DlRangeBody{
		DlBase: downloader.DlBase{
			Bck: bck,
			Limits: downloader.DlLimits{
				Connections: 2,
			},
		},
		Template: template,
	})
	tassert.CheckError(t, err)
	defer api.DownloadAbort(baseParams, id)

	time.Sleep(2 * time.Second) // wait for downloader to pick up the job

	resp, err := api.DownloadStatus(baseParams, id)
	tassert.CheckFatal(t, err)

	tassert.Errorf(
		t, len(resp.CurrentTasks) > smap.CountTargets(),
		"number of tasks mismatch (expected at least: %d, got: %d)",
		smap.CountTargets()+1, len(resp.CurrentTasks),
	)
	tassert.Errorf(
		t, len(resp.CurrentTasks) <= 2*smap.CountTargets(),
		"number of tasks mismatch (expected as most: %d, got: %d)",
		2*smap.CountTargets(), len(resp.CurrentTasks),
	)
}

func TestDownloadJobConcurrency(t *testing.T) {
	var (
		proxyURL   = tutils.RandomProxyURL()
		baseParams = tutils.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     cmn.RandString(10),
			Provider: cmn.ProviderAIS,
		}

		template = "https://storage.googleapis.com/lpr-vision/imagenet/imagenet_train-{000001..0000140}.tgz"
	)

	tutils.CreateFreshBucket(t, proxyURL, bck)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	smap, err := api.GetClusterMap(baseParams)
	tassert.CheckFatal(t, err)

	id1, err := api.DownloadRangeWithParam(baseParams, downloader.DlRangeBody{
		DlBase: downloader.DlBase{
			Bck: bck,
			Limits: downloader.DlLimits{
				Connections: 1,
			},
		},
		Template: template,
	})
	tassert.CheckError(t, err)
	defer api.DownloadAbort(baseParams, id1)

	time.Sleep(time.Second) // wait for downloader to pick up the first job

	id2, err := api.DownloadRange(baseParams, generateDownloadDesc(), bck, template)
	tassert.CheckError(t, err)
	defer api.DownloadAbort(baseParams, id2)

	time.Sleep(2 * time.Second) // wait for downloader to pick up the second job

	resp1, err := api.DownloadStatus(baseParams, id1)
	tassert.CheckFatal(t, err)

	tassert.Errorf(
		t, len(resp1.CurrentTasks) <= smap.CountTargets(),
		"number of tasks mismatch (expected at most: %d, got: %d)",
		smap.CountTargets(), len(resp1.CurrentTasks),
	)

	resp2, err := api.DownloadStatus(baseParams, id2)
	tassert.CheckFatal(t, err)

	// If downloader didn't start jobs concurrently the number of current
	// tasks would be 0 (as the previous download would clog the downloader).
	tassert.Errorf(
		t, len(resp2.CurrentTasks) > smap.CountTargets(),
		"number of tasks mismatch (expected at least: %d, got: %d)",
		smap.CountTargets()+1, len(resp2.CurrentTasks),
	)
}
