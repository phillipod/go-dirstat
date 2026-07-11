package scan

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

func BenchmarkMillionEntryWideDirectory(b *testing.B) {
	const entries = 1_000_000
	for _, benchmark := range []struct {
		name      string
		batchSize int
	}{
		{name: fmt.Sprintf("batched-%d", defaultDirectoryBatchSize), batchSize: defaultDirectoryBatchSize},
		{name: "single-batch", batchSize: entries},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			var elapsedTotal time.Duration
			var peakTotal, retainedTotal uint64
			for b.Loop() {
				root, elapsed, peak, retained := runSyntheticWideScan(b, entries, benchmark.batchSize)
				if len(root.Children) != entries || root.FileCount != entries || root.Apparent != entries {
					b.Fatalf("wide scan = %d children/%d files/%d bytes, want %d", len(root.Children), root.FileCount, root.Apparent, entries)
				}
				elapsedTotal += elapsed
				peakTotal += peak
				retainedTotal += retained
				runtime.KeepAlive(root)
			}
			b.ReportMetric(float64(entries)/(elapsedTotal.Seconds()/float64(b.N)), "entries/s")
			b.ReportMetric(float64(peakTotal)/float64(b.N), "peak-heap-bytes/op")
			b.ReportMetric(float64(retainedTotal)/float64(b.N), "retained-heap-bytes/op")
			transient := uint64(0)
			if peakTotal > retainedTotal {
				transient = peakTotal - retainedTotal
			}
			b.ReportMetric(float64(transient)/float64(b.N), "transient-heap-bytes/op")
		})
	}
}

func runSyntheticWideScan(tb testing.TB, entries, batchSize int) (*tree.Node, time.Duration, uint64, uint64) {
	tb.Helper()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var peak atomic.Uint64
	peak.Store(before.HeapAlloc)
	done := make(chan struct{})
	var sampleWG sync.WaitGroup
	sampleWG.Add(1)
	go func() {
		defer sampleWG.Done()
		ticker := time.NewTicker(200 * time.Microsecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				var current runtime.MemStats
				runtime.ReadMemStats(&current)
				updatePeak(&peak, current.HeapAlloc)
			case <-done:
				return
			}
		}
	}()

	ctx := context.Background()
	workers := max(1, runtime.GOMAXPROCS(0))
	s := &scanner{
		ctx:          ctx,
		opts:         Options{Policy: scope.New(), Concurrency: workers},
		sem:          make(chan struct{}, workers),
		dirSlots:     make(chan struct{}, max(0, workers-1)),
		statJobs:     make(chan statTask, workers),
		fileGroups:   make(map[fileKey]*fileGroup),
		dirBatchSize: batchSize,
		openDir: func(string) (directoryReader, error) {
			return &syntheticDirectoryReader{total: entries}, nil
		},
	}
	root := &tree.Node{Name: "synthetic", IsDir: true}
	lt := &liveTree{root: root, files: &s.files, dirs: &s.dirs, errors: &s.errors}
	s.startStatWorkers(workers)
	start := time.Now()
	s.scanDir("/synthetic", "", syntheticFileInfo{name: "synthetic", mode: fs.ModeDir | 0o755}, 0, "", 0, root, nil, lt)
	elapsed := time.Since(start)
	s.stopStatWorkers()

	var atEnd runtime.MemStats
	runtime.ReadMemStats(&atEnd)
	updatePeak(&peak, atEnd.HeapAlloc)
	close(done)
	sampleWG.Wait()

	runtime.GC()
	var retained runtime.MemStats
	runtime.ReadMemStats(&retained)
	runtime.KeepAlive(root)
	return root, elapsed, subtractFloor(peak.Load(), before.HeapAlloc), subtractFloor(retained.HeapAlloc, before.HeapAlloc)
}

func updatePeak(peak *atomic.Uint64, value uint64) {
	for current := peak.Load(); value > current; current = peak.Load() {
		if peak.CompareAndSwap(current, value) {
			return
		}
	}
}

func subtractFloor(value, baseline uint64) uint64 {
	if value <= baseline {
		return 0
	}
	return value - baseline
}

type syntheticDirectoryReader struct {
	next, total int
}

func (r *syntheticDirectoryReader) ReadDir(n int) ([]os.DirEntry, error) {
	if r.next >= r.total {
		return nil, io.EOF
	}
	remaining := r.total - r.next
	if n <= 0 || n > remaining {
		n = remaining
	}
	entries := make([]os.DirEntry, n)
	for i := range n {
		name := fmt.Sprintf("file-%07d", r.next+i)
		entries[i] = syntheticDirEntry{name: name}
	}
	r.next += n
	return entries, nil
}

func (*syntheticDirectoryReader) Close() error { return nil }

type syntheticDirEntry struct {
	name string
}

func (e syntheticDirEntry) Name() string    { return e.name }
func (syntheticDirEntry) IsDir() bool       { return false }
func (syntheticDirEntry) Type() fs.FileMode { return 0 }
func (e syntheticDirEntry) Info() (fs.FileInfo, error) {
	return syntheticFileInfo{name: e.name, size: 1}, nil
}

type syntheticFileInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (i syntheticFileInfo) Name() string      { return i.name }
func (i syntheticFileInfo) Size() int64       { return i.size }
func (i syntheticFileInfo) Mode() fs.FileMode { return i.mode }
func (syntheticFileInfo) ModTime() time.Time  { return time.Unix(1, 0) }
func (i syntheticFileInfo) IsDir() bool       { return i.mode.IsDir() }
func (syntheticFileInfo) Sys() any            { return nil }
