// Copyright 2017 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"math"
	"sync"
	"time"

	"github.com/keybase/client/go/logger"
	"github.com/keybase/kbfs/kbfssync"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

// backpressureTracker keeps track of the variables used to calculate
// backpressure. It keeps track of a generic resource (which can be
// either bytes or files).
//
// Let U be the (approximate) resource usage of the journal and F be
// the free resources. Then we want to enforce
//
//   U <= min(k(U+F), L),
//
// where 0 < k <= 1 is some fraction, and L > 0 is the absolute
// resource usage limit. But in addition to that, we want to set
// thresholds 0 <= m <= M <= 1 such that we apply proportional
// backpressure (with a given maximum delay) when
//
//   m <= max(U/(k(U+F)), U/L) <= M,
//
// which is equivalent to
//
//   m <= U/min(k(U+F), L) <= M.
//
// Note that this type doesn't do any locking, so it's the caller's
// responsibility to do so.
type backpressureTracker struct {
	// minThreshold is m in the above.
	minThreshold float64
	// maxThreshold is M in the above.
	maxThreshold float64
	// limitFrac is k in the above.
	limitFrac float64
	// limit is L in the above.
	limit int64

	// used is U in the above.
	used int64
	// free is F in the above.
	free int64

	// semaphoreMax is the last calculated value of currLimit(),
	// which is min(k(U+F), L).
	semaphoreMax int64
	// The count of the semaphore is semaphoreMax - U - I, where I
	// is the resource count that is currently "in-flight",
	// i.e. between beforeBlockPut() and afterBlockPut() calls.
	semaphore *kbfssync.Semaphore
}

func newBackpressureTracker(minThreshold, maxThreshold, limitFrac float64,
	limit, initialFree int64) (*backpressureTracker, error) {
	if minThreshold < 0.0 {
		return nil, errors.Errorf("minThreshold=%f < 0.0",
			minThreshold)
	}
	if maxThreshold < minThreshold {
		return nil, errors.Errorf(
			"maxThreshold=%f < minThreshold=%f",
			maxThreshold, minThreshold)
	}
	if 1.0 < maxThreshold {
		return nil, errors.Errorf("1.0 < maxThreshold=%f",
			maxThreshold)
	}
	if limitFrac < 0.01 {
		return nil, errors.Errorf("limitFrac=%f < 0.01", limitFrac)
	}
	if limitFrac > 1.0 {
		return nil, errors.Errorf("limitFrac=%f > 1.0", limitFrac)
	}
	if limit < 0 {
		return nil, errors.Errorf("limit=%d < 0", limit)
	}
	if initialFree < 0 {
		return nil, errors.Errorf("initialFree=%d < 0", initialFree)
	}
	bt := &backpressureTracker{
		minThreshold, maxThreshold, limitFrac, limit,
		0, initialFree, 0, kbfssync.NewSemaphore(),
	}
	bt.updateSemaphoreMax()
	return bt, nil
}

// currLimit returns the resource limit, taking into account the
// amount of free resources left. This is min(k(U+F), L).
func (bt backpressureTracker) currLimit() float64 {
	// Calculate k(U+F), converting to float64 first to avoid
	// overflow, although losing some precision in the process.
	usedFloat := float64(bt.used)
	freeFloat := float64(bt.free)
	limit := bt.limitFrac * (usedFloat + freeFloat)
	return math.Min(limit, float64(bt.limit))
}

func (bt backpressureTracker) usedFrac() float64 {
	return float64(bt.used) / bt.currLimit()
}

// delayScale returns a number between 0 and 1, which should be
// multiplied with the maximum delay to get the backpressure delay to
// apply.
func (bt backpressureTracker) delayScale() float64 {
	usedFrac := bt.usedFrac()

	// We want the delay to be 0 if usedFrac <= m and the max
	// delay if usedFrac >= M, so linearly interpolate the delay
	// scale.
	m := bt.minThreshold
	M := bt.maxThreshold
	return math.Min(1.0, math.Max(0.0, (usedFrac-m)/(M-m)))
}

// updateSemaphoreMax must be called whenever bt.used or bt.free
// changes.
func (bt *backpressureTracker) updateSemaphoreMax() {
	newMax := int64(bt.currLimit())
	delta := newMax - bt.semaphoreMax
	// These operations are adjusting the *maximum* value of
	// bt.semaphore.
	if delta > 0 {
		bt.semaphore.Release(delta)
	} else if delta < 0 {
		bt.semaphore.ForceAcquire(-delta)
	}
	bt.semaphoreMax = newMax
}

func (bt *backpressureTracker) onEnable(usedResources int64) (
	availableResources int64) {
	bt.used += usedResources
	bt.updateSemaphoreMax()
	if usedResources == 0 {
		return bt.semaphore.Count()
	}
	return bt.semaphore.ForceAcquire(usedResources)
}

func (bt *backpressureTracker) onDisable(usedResources int64) {
	bt.used -= usedResources
	bt.updateSemaphoreMax()
	if usedResources > 0 {
		bt.semaphore.Release(usedResources)
	}
}

func (bt *backpressureTracker) updateFree(freeResources int64) {
	bt.free = freeResources
	bt.updateSemaphoreMax()
}

func (bt *backpressureTracker) beforeBlockPut(
	ctx context.Context, blockResources int64) (
	availableResources int64, err error) {
	return bt.semaphore.Acquire(ctx, blockResources)
}

func (bt *backpressureTracker) afterBlockPut(
	blockResources int64, putData bool) {
	if putData {
		bt.used += blockResources
		bt.updateSemaphoreMax()
	} else {
		bt.semaphore.Release(blockResources)
	}
}

func (bt *backpressureTracker) onBlocksDelete(blockResources int64) {
	if blockResources == 0 {
		return
	}

	bt.semaphore.Release(blockResources)

	bt.used -= blockResources
	bt.updateSemaphoreMax()
}

func (bt *backpressureTracker) beforeDiskBlockCachePut(blockResources int64) (
	availableResources int64) {
	// TODO: Implement TryAcquire that automatically rolls back if it would go
	// negative.
	availableResources = bt.semaphore.ForceAcquire(blockResources)
	if availableResources < 0 {
		// We must roll back the acquisition of resources. We should still
		// return the negative number, however, so the disk block cache
		// knows how much to evict.
		bt.afterBlockPut(blockResources, false)
	}
	return availableResources
}

type backpressureTrackerStatus struct {
	// Derived numbers.
	UsedFrac   float64
	DelayScale float64

	// Constants.
	MinThreshold float64
	MaxThreshold float64
	LimitFrac    float64
	Limit        int64

	// Raw numbers.
	Used  int64
	Free  int64
	Max   int64
	Count int64
}

func (bt *backpressureTracker) getStatus() backpressureTrackerStatus {
	return backpressureTrackerStatus{
		UsedFrac:   bt.usedFrac(),
		DelayScale: bt.delayScale(),

		MinThreshold: bt.minThreshold,
		MaxThreshold: bt.maxThreshold,
		LimitFrac:    bt.limitFrac,
		Limit:        bt.limit,

		Used:  bt.used,
		Free:  bt.free,
		Max:   bt.semaphoreMax,
		Count: bt.semaphore.Count(),
	}
}

// backpressureDiskLimiter is an implementation of diskLimiter that
// uses backpressure to slow down block puts before they hit the disk
// limits.
type backpressureDiskLimiter struct {
	log logger.Logger

	maxDelay            time.Duration
	delayFn             func(context.Context, time.Duration) error
	freeBytesAndFilesFn func() (int64, int64, error)

	// lock protects everything in the trackers, including the
	// (implicit) maximum values of the semaphores, but not the
	// actual semaphore itself.
	lock                                   sync.RWMutex
	journalByteTracker, journalFileTracker *backpressureTracker
	diskCacheByteTracker                   *backpressureTracker
}

var _ DiskLimiter = (*backpressureDiskLimiter)(nil)

type backpressureDiskLimiterParams struct {
	// minThreshold is the fraction of the free bytes/files at
	// which we start to apply backpressure.
	minThreshold float64
	// maxThreshold is the fraction of the free bytes/files at
	// which we max out on backpressure.
	maxThreshold float64
	// journalFrac is fraction of the free bytes/files that the
	// journal is allowed to use.
	journalFrac float64
	// diskCacheFrac is the fraction of the free bytes that the
	// disk cache is allowed to use. The disk cache doesn't store
	// individual files.
	diskCacheFrac float64
	// byteLimit is the total cap for free bytes. The journal will
	// be allowed to use at most journalFrac*byteLimit, and the
	// disk cache will be allowed to use at most
	// diskCacheFrac*byteLimit.
	byteLimit int64
	// maxFreeFiles is the cap for free files. The journal will be
	// allowed to use at most journalFrac*fileLimit. This limit
	// doesn't apply to the disk cache, since it doesn't store
	// individual files.
	fileLimit int64
	// maxDelay is the maximum delay used for backpressure.
	maxDelay time.Duration
	// delayFn is a function that takes a context and a duration
	// and returns after sleeping for that duration, or if the
	// context is cancelled. Overridable for testing.
	delayFn func(context.Context, time.Duration) error
	// freeBytesAndFilesFn is a function that returns the current
	// free bytes and files on the disk containing the
	// journal/disk cache directory. Overridable for testing.
	freeBytesAndFilesFn func() (int64, int64, error)
}

// defaultDiskLimitMaxDelay is the maximum amount to delay a block
// put. Exposed as a constant as it is used by
// tlfJournalConfigAdapter.
const defaultDiskLimitMaxDelay = 10 * time.Second

func makeDefaultBackpressureDiskLimiterParams(
	storageRoot string) backpressureDiskLimiterParams {
	return backpressureDiskLimiterParams{
		// Start backpressure when 50% of free bytes or files
		// are used...
		minThreshold: 0.5,
		// ...and max it out at 95% (slightly less than 100%
		// to allow for inaccuracies in estimates).
		maxThreshold: 0.95,
		// Cap journal usage to 15% of free bytes and files...
		journalFrac: 0.15,
		// ...and cap disk cache usage to 10% of free
		// bytes. The disk cache doesn't store individual
		// files.
		diskCacheFrac: 0.10,
		// Set the byte limit to 200 GiB, which translates to
		// having the journal take up at most 30 GiB, and the
		// disk cache to take up at most 20 GiB.
		byteLimit: 200 * 1024 * 1024 * 1024,
		// Set the file limit to 6 million files, which
		// translates to having the journal take up at most
		// 900k files.
		fileLimit: 6000000,
		maxDelay:  defaultDiskLimitMaxDelay,
		delayFn:   defaultDoDelay,
		freeBytesAndFilesFn: func() (int64, int64, error) {
			return defaultGetFreeBytesAndFiles(storageRoot)
		},
	}
}

// newBackpressureDiskLimiter constructs a new backpressureDiskLimiter
// with the given params.
func newBackpressureDiskLimiter(
	log logger.Logger, params backpressureDiskLimiterParams) (
	*backpressureDiskLimiter, error) {
	freeBytes, freeFiles, err := params.freeBytesAndFilesFn()
	if err != nil {
		return nil, err
	}
	// byteLimit and fileLimit must be scaled by the proportion of
	// the limit that the journal should consume.
	journalByteLimit := int64((float64(params.byteLimit) * params.journalFrac) + 0.5)
	byteTracker, err := newBackpressureTracker(
		params.minThreshold, params.maxThreshold,
		params.journalFrac, journalByteLimit, freeBytes)
	if err != nil {
		return nil, err
	}
	// the fileLimit is only used here, but in the interest of consistency with
	// how we treat the byteLimit, we multiply it by the journalFrac.
	journalFileLimit := int64((float64(params.fileLimit) * params.journalFrac) + 0.5)
	fileTracker, err := newBackpressureTracker(
		params.minThreshold, params.maxThreshold,
		params.journalFrac, journalFileLimit, freeFiles)
	if err != nil {
		return nil, err
	}
	diskCacheByteLimit := int64((float64(params.byteLimit) * params.diskCacheFrac) + 0.5)
	diskCacheByteTracker, err := newBackpressureTracker(
		1.0, 1.0, params.diskCacheFrac, diskCacheByteLimit, freeBytes)
	bdl := &backpressureDiskLimiter{
		log, params.maxDelay, params.delayFn, params.freeBytesAndFilesFn, sync.RWMutex{},
		byteTracker, fileTracker, diskCacheByteTracker,
	}
	return bdl, nil
}

// defaultDoDelay uses a timer to delay by the given duration.
func defaultDoDelay(ctx context.Context, delay time.Duration) error {
	if delay == 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		timer.Stop()
		return errors.WithStack(ctx.Err())
	}
}

func defaultGetFreeBytesAndFiles(path string) (int64, int64, error) {
	// getDiskLimits returns availableBytes and availableFiles,
	// but we want to avoid confusing that with availBytes and
	// availFiles in the sense of the semaphore value.
	freeBytes, freeFiles, err := getDiskLimits(path)
	if err != nil {
		return 0, 0, err
	}

	if freeBytes > uint64(math.MaxInt64) {
		freeBytes = math.MaxInt64
	}
	if freeFiles > uint64(math.MaxInt64) {
		freeFiles = math.MaxInt64
	}
	return int64(freeBytes), int64(freeFiles), nil
}

type bdlSnapshot struct {
	used  int64
	free  int64
	max   int64
	count int64
}

func (bdl *backpressureDiskLimiter) getSnapshotsForTest() (
	byteSnapshot, fileSnapshot bdlSnapshot) {
	bdl.lock.RLock()
	defer bdl.lock.RUnlock()
	return bdlSnapshot{bdl.journalByteTracker.used, bdl.journalByteTracker.free,
			bdl.journalByteTracker.semaphoreMax,
			bdl.journalByteTracker.semaphore.Count()},
		bdlSnapshot{bdl.journalFileTracker.used, bdl.journalFileTracker.free,
			bdl.journalFileTracker.semaphoreMax,
			bdl.journalFileTracker.semaphore.Count()}
}

func (bdl *backpressureDiskLimiter) onJournalEnable(
	ctx context.Context, journalBytes, journalFiles int64) (
	availableBytes, availableFiles int64) {
	bdl.lock.Lock()
	defer bdl.lock.Unlock()
	availableBytes = bdl.journalByteTracker.onEnable(journalBytes)
	availableFiles = bdl.journalFileTracker.onEnable(journalFiles)
	return availableBytes, availableFiles
}

func (bdl *backpressureDiskLimiter) onJournalDisable(
	ctx context.Context, journalBytes, journalFiles int64) {
	bdl.lock.Lock()
	defer bdl.lock.Unlock()
	bdl.journalByteTracker.onDisable(journalBytes)
	bdl.journalFileTracker.onDisable(journalFiles)
}

func (bdl *backpressureDiskLimiter) onDiskBlockCacheEnable(ctx context.Context,
	diskCacheBytes int64) {
	bdl.lock.Lock()
	defer bdl.lock.Unlock()
	bdl.diskCacheByteTracker.onEnable(diskCacheBytes)
}

func (bdl *backpressureDiskLimiter) onDiskBlockCacheDisable(ctx context.Context,
	diskCacheBytes int64) {
	bdl.lock.Lock()
	defer bdl.lock.Unlock()
	bdl.diskCacheByteTracker.onDisable(diskCacheBytes)
}

func (bdl *backpressureDiskLimiter) getDelayLocked(
	ctx context.Context, now time.Time) time.Duration {
	byteDelayScale := bdl.journalByteTracker.delayScale()
	fileDelayScale := bdl.journalFileTracker.delayScale()
	delayScale := math.Max(byteDelayScale, fileDelayScale)

	// Set maxDelay to min(bdl.maxDelay, time until deadline - 1s).
	maxDelay := bdl.maxDelay
	if deadline, ok := ctx.Deadline(); ok {
		// Subtract a second to allow for some slack.
		remainingTime := deadline.Sub(now) - time.Second
		if remainingTime < maxDelay {
			maxDelay = remainingTime
		}
	}

	return time.Duration(delayScale * float64(maxDelay))
}

func (bdl *backpressureDiskLimiter) updateFreeLocked() (
	freeBytes, freeFiles int64, err error) {
	// Call this under lock to avoid problems with its
	// return values going stale while blocking on
	// bdl.lock.
	freeBytes, freeFiles, err = bdl.freeBytesAndFilesFn()
	if err != nil {
		return 0, 0, err
	}

	bdl.journalFileTracker.updateFree(freeFiles)
	bdl.journalByteTracker.updateFree(freeBytes + bdl.diskCacheByteTracker.used)
	bdl.diskCacheByteTracker.updateFree(freeBytes + bdl.journalByteTracker.used)
	return freeBytes, freeFiles, nil
}

func (bdl *backpressureDiskLimiter) beforeBlockPut(
	ctx context.Context, blockBytes, blockFiles int64) (
	availableBytes, availableFiles int64, err error) {
	if blockBytes == 0 {
		// Better to return an error than to panic in Acquire.
		return bdl.journalByteTracker.semaphore.Count(),
			bdl.journalFileTracker.semaphore.Count(), errors.New(
				"backpressureDiskLimiter.beforeBlockPut called with 0 blockBytes")
	}
	if blockFiles == 0 {
		// Better to return an error than to panic in Acquire.
		return bdl.journalByteTracker.semaphore.Count(),
			bdl.journalFileTracker.semaphore.Count(), errors.New(
				"backpressureDiskLimiter.beforeBlockPut called with 0 blockFiles")
	}

	delay, err := func() (time.Duration, error) {
		bdl.lock.Lock()
		defer bdl.lock.Unlock()

		freeBytes, freeFiles, err := bdl.updateFreeLocked()
		if err != nil {
			return 0, err
		}

		delay := bdl.getDelayLocked(ctx, time.Now())
		if delay > 0 {
			bdl.log.CDebugf(ctx, "Delaying block put of %d bytes and %d files by %f s ("+
				"journalBytes=%d, freeBytes=%d, "+
				"journalFiles=%d, freeFiles=%d)",
				blockBytes, blockFiles, delay.Seconds(),
				bdl.journalByteTracker.used, freeBytes,
				bdl.journalFileTracker.used, freeFiles)
		}

		return delay, nil
	}()
	if err != nil {
		return bdl.journalByteTracker.semaphore.Count(),
			bdl.journalFileTracker.semaphore.Count(), err
	}

	// TODO: Update delay if any variables change (i.e., we
	// suddenly free up a lot of space).
	err = bdl.delayFn(ctx, delay)
	if err != nil {
		return bdl.journalByteTracker.semaphore.Count(),
			bdl.journalFileTracker.semaphore.Count(), err
	}

	availableBytes, err = bdl.journalByteTracker.beforeBlockPut(ctx, blockBytes)
	if err != nil {
		return availableFiles, bdl.journalFileTracker.semaphore.Count(), err
	}
	defer func() {
		if err != nil {
			bdl.journalByteTracker.afterBlockPut(blockBytes, false)
			availableBytes = bdl.journalByteTracker.semaphore.Count()
		}
	}()

	availableFiles, err = bdl.journalFileTracker.beforeBlockPut(ctx, blockFiles)
	return availableBytes, availableFiles, err
}

func (bdl *backpressureDiskLimiter) afterBlockPut(
	ctx context.Context, blockBytes, blockFiles int64, putData bool) {
	bdl.lock.Lock()
	defer bdl.lock.Unlock()
	bdl.journalByteTracker.afterBlockPut(blockBytes, putData)
	bdl.journalFileTracker.afterBlockPut(blockFiles, putData)
}

func (bdl *backpressureDiskLimiter) onBlocksDelete(
	ctx context.Context, blockBytes, blockFiles int64) {
	bdl.lock.Lock()
	defer bdl.lock.Unlock()
	bdl.journalByteTracker.onBlocksDelete(blockBytes)
	bdl.journalFileTracker.onBlocksDelete(blockFiles)
}

func (bdl *backpressureDiskLimiter) onDiskBlockCacheDelete(
	ctx context.Context, blockBytes int64) {
	if blockBytes == 0 {
		return
	}
	bdl.lock.Lock()
	defer bdl.lock.Unlock()
	bdl.diskCacheByteTracker.onBlocksDelete(blockBytes)
}

func (bdl *backpressureDiskLimiter) beforeDiskBlockCachePut(
	ctx context.Context, blockBytes int64) (
	availableBytes int64, err error) {
	if blockBytes == 0 {
		// Better to return an error than to panic in ForceAcquire.
		return 0, errors.New("backpressureDiskLimiter.beforeDiskBlockCachePut" +
			" called with 0 blockBytes")
	}
	bdl.lock.Lock()
	defer bdl.lock.Unlock()
	_, _, err = bdl.updateFreeLocked()
	if err != nil {
		return 0, err
	}

	return bdl.diskCacheByteTracker.beforeDiskBlockCachePut(blockBytes), nil
}

func (bdl *backpressureDiskLimiter) afterDiskBlockCachePut(
	ctx context.Context, blockBytes int64, putData bool) {
	bdl.lock.Lock()
	defer bdl.lock.Unlock()
	bdl.diskCacheByteTracker.afterBlockPut(blockBytes, putData)
}

type backpressureDiskLimiterStatus struct {
	Type string

	// Derived numbers.
	CurrentDelaySec float64

	ByteTrackerStatus backpressureTrackerStatus
	FileTrackerStatus backpressureTrackerStatus
}

func (bdl *backpressureDiskLimiter) getStatus() interface{} {
	bdl.lock.RLock()
	defer bdl.lock.RUnlock()

	currentDelay := bdl.getDelayLocked(context.Background(), time.Now())

	return backpressureDiskLimiterStatus{
		Type: "BackpressureDiskLimiter",

		CurrentDelaySec: currentDelay.Seconds(),

		ByteTrackerStatus: bdl.journalByteTracker.getStatus(),
		FileTrackerStatus: bdl.journalFileTracker.getStatus(),
	}
}
