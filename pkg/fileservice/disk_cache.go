// Copyright 2022 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fileservice

import (
	"bytes"
	"context"
	"fmt"
	"hash/maphash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/matrixorigin/matrixone/pkg/fileservice/fifocache"
	"github.com/matrixorigin/matrixone/pkg/fileservice/fscache"
	"github.com/matrixorigin/matrixone/pkg/logutil"
	"github.com/matrixorigin/matrixone/pkg/perfcounter"
	metric "github.com/matrixorigin/matrixone/pkg/util/metric/v2"
)

type DiskCache struct {
	path               string
	cacheDataAllocator CacheDataAllocator
	perfCounterSets    []*perfcounter.CounterSet

	updatingPaths struct {
		*sync.Cond
		m map[string]bool
	}

	cache        *fifocache.Cache[string, struct{}]
	capacityFunc fscache.CapacityFunc
}

func NewDiskCache(
	ctx context.Context,
	path string,
	capacity fscache.CapacityFunc,
	perfCounterSets []*perfcounter.CounterSet,
	asyncLoad bool,
	cacheDataAllocator CacheDataAllocator,
	name string,
) (ret *DiskCache, err error) {

	err = os.MkdirAll(path, 0755)
	if err != nil {
		return nil, err
	}

	if cacheDataAllocator == nil {
		cacheDataAllocator = DefaultCacheDataAllocator()
	}

	seed := maphash.MakeSeed()

	inuseBytes, capacityBytes := metric.GetFsCacheBytesGauge(name, "disk")
	capacityBytes.Set(float64(capacity()))

	capacityFunc := func() int64 {
		// read from global size hint
		if n := GlobalDiskCacheSizeHint.Load(); n > 0 {
			return n
		}
		// fallback
		return capacity()
	}

	ret = &DiskCache{
		path:               path,
		cacheDataAllocator: cacheDataAllocator,
		perfCounterSets:    perfCounterSets,

		capacityFunc: capacityFunc,
		cache: fifocache.New(

			capacityFunc,

			func(key string) uint64 {
				return maphash.String(seed, key)
			},

			func(_ context.Context, _ string, _ struct{}, size int64) { // postSet
				inuseBytes.Add(float64(size))
				capacityBytes.Set(float64(capacityFunc()))
			},

			nil,
			func(ctx context.Context, path string, _ struct{}, size int64) {
				inuseBytes.Add(float64(-size))
				capacityBytes.Set(float64(capacityFunc()))
				err := os.Remove(path)
				if err == nil {
					perfcounter.Update(ctx, func(set *perfcounter.CounterSet) {
						set.FileService.Cache.Disk.Evict.Add(1)
					}, perfCounterSets...)
				} else if !os.IsNotExist(err) {
					logutil.Error("delete disk cache file",
						zap.Any("error", err),
					)
				}
			},
		),
	}
	ret.updatingPaths.Cond = sync.NewCond(new(sync.Mutex))
	ret.updatingPaths.m = make(map[string]bool)

	if asyncLoad {
		go ret.loadCache(ctx)
	} else {
		ret.loadCache(ctx)
	}

	if name != "" {
		allDiskCaches.Store(ret, name)
	}

	return ret, nil
}

func (d *DiskCache) loadCache(ctx context.Context) {
	t0 := time.Now()

	type Info struct {
		Path  string
		Entry os.DirEntry
	}
	works := make(chan Info)

	numWorkers := runtime.NumCPU()
	wg := new(sync.WaitGroup)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for work := range works {

				info, err := work.Entry.Info()
				if err != nil {
					continue // ignore
				}

				d.cache.Set(ctx, work.Path, struct{}{}, int64(fileSize(info)))
			}
		}()
	}

	var numFiles, numCacheFiles, numTempFiles, numDeleted int

	_ = filepath.WalkDir(d.path, func(path string, entry os.DirEntry, err error) error {
		numFiles++
		if err != nil {
			return nil //ignore
		}

		if entry.IsDir() {
			// try remove if empty. for cleaning old structure
			if path != d.path {
				// os.Remove will not delete non-empty directory
				_ = os.Remove(path)
			}
			return nil

		} else {
			// plain files
			if !strings.HasSuffix(entry.Name(), cacheFileSuffix) {
				// not cache file
				if strings.HasSuffix(entry.Name(), cacheFileTempSuffix) {
					numTempFiles++
					// temp file
					info, err := entry.Info()
					if err == nil && time.Since(info.ModTime()) > time.Hour*8 {
						// old temp file
						_ = os.Remove(path)
						numDeleted++
					}
				} else {
					// unknown file
					_ = os.Remove(path)
					numDeleted++
				}
				return nil
			}
		}

		numCacheFiles++
		works <- Info{
			Path:  path,
			Entry: entry,
		}

		return nil
	})

	close(works)
	wg.Wait()

	logutil.Info("disk cache info loaded",
		zap.Any("all files", numFiles),
		zap.Any("cache files", numCacheFiles),
		zap.Any("temp files", numTempFiles),
		zap.Any("deleted files", numDeleted),
		zap.Any("time", time.Since(t0)),
	)

	done := make(chan int64, 1)
	d.cache.Evict(ctx, done, 0)
	target := <-done
	logutil.Info("disk cache evict done",
		zap.Any("target", target),
	)

}

var _ IOVectorCache = new(DiskCache)

func (d *DiskCache) Read(
	ctx context.Context,
	vector *IOVector,
) (
	err error,
) {

	if vector.Policy.Any(SkipDiskCacheReads) {
		return nil
	}

	var numHit, numRead, numOpenIOEntry, numOpenFull, numError int64
	defer func() {
		LogEvent(ctx, str_update_metrics_begin)

		metric.FSReadHitDiskCounter.Add(float64(numHit))
		metric.FSReadReadDiskCounter.Add(float64(numRead))
		perfcounter.Update(ctx, func(c *perfcounter.CounterSet) {
			c.FileService.Cache.Read.Add(numRead)
			c.FileService.Cache.Hit.Add(numHit)
			c.FileService.Cache.Disk.Read.Add(numRead)
			c.FileService.Cache.Disk.Hit.Add(numHit)
			c.FileService.Cache.Disk.Error.Add(numError)
			c.FileService.Cache.Disk.OpenIOEntryFile.Add(numOpenIOEntry)
			c.FileService.Cache.Disk.OpenFullFile.Add(numOpenFull)
		}, d.perfCounterSets...)

		LogEvent(ctx, str_update_metrics_end)
	}()

	path, err := ParsePath(vector.FilePath)
	if err != nil {
		return err
	}

	openedFiles := make(map[string]*os.File)
	defer func() {
		LogEvent(ctx, str_close_disk_files_begin)
		for _, file := range openedFiles {
			_ = file.Close()
		}
		LogEvent(ctx, str_close_disk_files_end)
	}()

	fillEntry := func(entry *IOEntry) error {
		LogEvent(ctx, str_disk_cache_fill_entry_begin)
		defer LogEvent(ctx, str_disk_cache_fill_entry_end)

		if entry.done {
			return nil
		}
		if entry.Size < 0 {
			// ignore size unknown entry
			return nil
		}

		numRead++

		var file *os.File

		// entry file
		diskPath := d.pathForIOEntry(path.File, *entry)
		if f, ok := openedFiles[diskPath]; ok {
			// use opened file
			LogEvent(ctx, str_disk_cache_file_seek_begin)
			_, err = file.Seek(entry.Offset, io.SeekStart)
			LogEvent(ctx, str_disk_cache_file_seek_end)
			if err == nil {
				file = f
			}
		} else {
			// open file
			d.waitUpdateComplete(ctx, diskPath)
			LogEvent(ctx, str_disk_cache_file_open_begin)
			diskFile, err := os.Open(diskPath)
			LogEvent(ctx, str_disk_cache_file_open_end)
			if err == nil {
				file = diskFile
				defer func() {
					openedFiles[diskPath] = diskFile
				}()
				numOpenIOEntry++
			}
		}

		if file == nil {
			// try full file
			diskPath = d.pathForFile(path.File)
			if f, ok := openedFiles[diskPath]; ok {
				// use opened file
				LogEvent(ctx, str_disk_cache_file_seek_begin)
				_, err = f.Seek(entry.Offset, io.SeekStart)
				LogEvent(ctx, str_disk_cache_file_seek_end)
				if err == nil {
					file = f
				}
			} else {
				// open file
				d.waitUpdateComplete(ctx, diskPath)
				LogEvent(ctx, str_disk_cache_file_open_begin)
				diskFile, err := os.Open(diskPath)
				LogEvent(ctx, str_disk_cache_file_open_end)
				if err == nil {
					defer func() {
						openedFiles[diskPath] = diskFile
					}()
					numOpenFull++
					// seek
					LogEvent(ctx, str_disk_cache_file_seek_begin)
					_, err = diskFile.Seek(entry.Offset, io.SeekStart)
					LogEvent(ctx, str_disk_cache_file_seek_end)
					if err == nil {
						file = diskFile
					}
				}
			}
		}

		if file == nil {
			// no file available
			return nil
		}

		LogEvent(ctx, str_disk_cache_update_states_begin)
		if _, ok := d.cache.Get(ctx, diskPath); !ok {
			// set cache
			LogEvent(ctx, str_disk_cache_file_stat_begin)
			stat, err := file.Stat()
			LogEvent(ctx, str_disk_cache_file_stat_end)
			if err != nil {
				return err
			}
			d.cache.Set(ctx, diskPath, struct{}{}, fileSize(stat))
		}
		LogEvent(ctx, str_disk_cache_update_states_end)

		if err := entry.ReadFromOSFile(ctx, file, d.cacheDataAllocator); err != nil {
			return err
		}

		entry.done = true
		entry.fromCache = d
		numHit++

		return nil
	}

	for i := range vector.Entries {
		if err := fillEntry(&vector.Entries[i]); err != nil {
			// ignore error
			numError++
			logutil.Warn(
				"read disk cache error",
				zap.Any("error", err),
				zap.Any("path", vector.FilePath),
				zap.Any("entry", vector.Entries[i]),
			)
		}
	}

	return nil
}

func (d *DiskCache) Update(
	ctx context.Context,
	vector *IOVector,
	async bool,
) (
	err error,
) {

	if vector.Policy.Any(SkipDiskCacheWrites) {
		return nil
	}

	path, err := ParsePath(vector.FilePath)
	if err != nil {
		return err
	}

	// callback
	var onWritten []OnDiskCacheWrittenFunc
	if v := ctx.Value(CtxKeyDiskCacheCallbacks); v != nil {
		onWritten = v.(*DiskCacheCallbacks).OnWritten
	}

	for _, entry := range vector.Entries {
		if len(entry.Data) == 0 {
			// no data
			continue
		}
		if entry.Size < 0 {
			// ignore size unknown entry
			continue
		}
		if entry.fromCache == d {
			// no need to update
			continue
		}

		diskPath := d.pathForIOEntry(path.File, entry)
		written, err := d.writeFile(ctx, diskPath, func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(entry.Data)), nil
		})
		if err != nil {
			return err
		}
		if written {
			for _, fn := range onWritten {
				fn(vector.FilePath, entry)
			}
		}

	}

	return nil
}

func (d *DiskCache) writeFile(
	ctx context.Context,
	diskPath string,
	openReader func(context.Context) (io.ReadCloser, error),
) (written bool, err error) {

	var numCreate, numStat, numError, numWrite int64
	defer func() {
		perfcounter.Update(ctx, func(set *perfcounter.CounterSet) {
			set.FileService.Cache.Disk.CreateFile.Add(numCreate)
			set.FileService.Cache.Disk.StatFile.Add(numStat)
			set.FileService.Cache.Disk.WriteFile.Add(numWrite)
			set.FileService.Cache.Disk.Error.Add(numError)
		})
	}()

	defer func() {
		if err != nil {
			// ignore errors
			numError++
			logutil.Warn(
				"write disk cache error",
				zap.Any("error", err),
				zap.Any("path", diskPath),
			)
			err = nil
		}
	}()

	// evict if disk is full
	defer func() {
		if isDiskFull(err) {
			d.cache.ForceEvict(ctx, d.capacityFunc()/10)
		}
	}()

	doneUpdate := d.startUpdate(diskPath)
	defer doneUpdate()

	if _, ok := d.cache.Get(ctx, diskPath); ok {
		// already exists
		return false, nil
	}
	stat, err := os.Stat(diskPath)
	if err == nil {
		// file exists
		d.cache.Set(ctx, diskPath, struct{}{}, fileSize(stat))
		numStat++
		return false, nil
	}

	// write data
	dir := filepath.Dir(diskPath)
	err = os.MkdirAll(dir, 0755)
	if err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, "*"+cacheFileTempSuffix)
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()

	numCreate++
	from, err := openReader(ctx)
	if err != nil {
		return false, err
	}
	defer from.Close()

	// do eviction before write
	forceEvict := int64(0)
	if file, ok := from.(*os.File); ok {
		// get file size
		info, err := file.Stat()
		if err == nil {
			forceEvict = fileSize(info)
		}
	}
	d.cache.Evict(ctx, nil, forceEvict)

	var buf []byte
	put := ioBufferPool.Get(&buf)
	defer put.Put()
	_, err = io.CopyBuffer(f, from, buf)
	if err != nil {
		return false, err
	}

	if err := f.Sync(); err != nil {
		return false, err
	}

	stat, err = f.Stat()
	if err != nil {
		return false, err
	}
	size := fileSize(stat)

	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(f.Name(), diskPath); err != nil {
		return false, err
	}
	logutil.Debug("disk cache file written",
		zap.Any("path", diskPath),
	)

	d.cache.Set(ctx, diskPath, struct{}{}, size)

	numWrite++

	return true, nil
}

func (d *DiskCache) Flush(ctx context.Context) {
}

const (
	cacheFileSuffix     = ".mofscache"
	cacheFileTempSuffix = cacheFileSuffix + ".tmp"
)

func (d *DiskCache) pathForIOEntry(path string, entry IOEntry) string {
	if entry.Size < 0 {
		panic("should not cache size -1 entry")
	}
	return filepath.Join(
		d.path,
		fmt.Sprintf("%d-%d%s%s", entry.Offset, entry.Size, toOSPath(path), cacheFileSuffix),
	)
}

func (d *DiskCache) pathForFile(path string) string {
	return filepath.Join(
		d.path,
		fmt.Sprintf("full%s%s", toOSPath(path), cacheFileSuffix),
	)
}

var ErrNotCacheFile = errorStr("not a cache file")

func (d *DiskCache) decodeFilePath(diskPath string) (string, error) {
	path, err := filepath.Rel(d.path, diskPath)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(path, "full") {
		return "", ErrNotCacheFile
	}
	path = strings.TrimPrefix(path, "full")
	path = strings.TrimSuffix(path, cacheFileSuffix)
	return fromOSPath(path), nil
}

func (d *DiskCache) waitUpdateComplete(ctx context.Context, path string) {
	LogEvent(ctx, str_disk_cache_wait_update_complete_begin)
	defer LogEvent(ctx, str_disk_cache_wait_update_complete_end)
	d.updatingPaths.L.Lock()
	for d.updatingPaths.m[path] {
		d.updatingPaths.Wait()
	}
	d.updatingPaths.L.Unlock()
}

func (d *DiskCache) startUpdate(path string) (done func()) {
	d.updatingPaths.L.Lock()
	for d.updatingPaths.m[path] {
		d.updatingPaths.Wait()
	}
	d.updatingPaths.m[path] = true
	d.updatingPaths.L.Unlock()
	done = func() {
		d.updatingPaths.L.Lock()
		delete(d.updatingPaths.m, path)
		d.updatingPaths.Broadcast()
		d.updatingPaths.L.Unlock()
	}
	return
}

var _ FileCache = new(DiskCache)

func (d *DiskCache) SetFile(
	ctx context.Context,
	path string,
	openReader func(context.Context) (io.ReadCloser, error),
) error {
	diskPath := d.pathForFile(path)
	_, err := d.writeFile(ctx, diskPath, openReader)
	if err != nil {
		return err
	}
	return nil
}

func (d *DiskCache) DeletePaths(
	ctx context.Context,
	paths []string,
) (err error) {
	for _, path := range paths {
		//TODO also delete IOEntry files
		if err = d.removeOnePath(ctx, path); err != nil {
			return
		}
	}

	return
}

func (d *DiskCache) removeOnePath(ctx context.Context, path string) (err error) {
	diskPath := d.pathForFile(path)
	doneUpdate := d.startUpdate(diskPath)
	defer doneUpdate()
	if err = os.Remove(diskPath); err != nil {
		if !os.IsNotExist(err) {
			return
		}
		err = nil
	}
	d.cache.Delete(ctx, diskPath)
	return
}

func (d *DiskCache) Evict(ctx context.Context, done chan int64) {
	d.cache.Evict(ctx, done, 0)
}

func fileSize(info fs.FileInfo) int64 {
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(sys.Blocks) * 512 // it's always 512, not sys.Blksize
	}
	return info.Size()
}

func (d *DiskCache) Close(ctx context.Context) {
	allDiskCaches.Delete(d)
}
