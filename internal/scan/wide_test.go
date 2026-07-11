package scan

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

func TestBatchedDirectoryMatchesSingleBatchWithAliasesAndHardlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "m-target")
	writeFile(t, filepath.Join(target, "nested", "payload.bin"), 257)
	writeFile(t, filepath.Join(root, "plain-z.bin"), 31)
	if err := os.Link(filepath.Join(target, "nested", "payload.bin"), filepath.Join(root, "hardlink-a.bin")); err != nil {
		t.Skipf("hardlinks not supported: %v", err)
	}
	for _, name := range []string{"a-alias", "z-alias"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("directory symlinks not supported: %v", err)
		}
	}

	policy := scope.New(scope.WithFollowSymlinks(true))
	singleBatch := Options{Policy: policy, Concurrency: 8, directoryBatchSize: 1 << 20}
	want, wantStats, err := Scan(context.Background(), root, singleBatch)
	if err != nil {
		t.Fatal(err)
	}
	batched := Options{Policy: policy, Concurrency: 8, directoryBatchSize: 1}
	got, gotStats, err := Scan(context.Background(), root, batched)
	if err != nil {
		t.Fatal(err)
	}
	if !treeEqual(got, want) {
		t.Fatalf("one-entry batches changed the authoritative tree\nwant paths: %v\ngot paths:  %v", relativeTreePaths(want), relativeTreePaths(got))
	}
	if !scanStatsEqual(gotStats, wantStats) {
		t.Fatalf("one-entry batch stats = %+v, single-batch stats = %+v", gotStats, wantStats)
	}
	assertLexicalChildren(t, got)
}

func TestBatchedDirectoryRetainsCompletedEntriesOnReadAndStatErrors(t *testing.T) {
	readFailure := errors.New("synthetic readdir failure")
	statFailure := errors.New("synthetic stat failure")
	reader := &scriptedDirectoryReader{
		entries: []os.DirEntry{
			scriptedDirEntry{name: "z-good"},
			scriptedDirEntry{name: "a-bad", err: statFailure},
			scriptedDirEntry{name: "m-good"},
		},
		terminalErr: readFailure,
	}
	root, scanner := runScriptedDirectoryScan(t, context.Background(), reader, 2, 3, nil)
	if !errors.Is(root.Err, readFailure) {
		t.Fatalf("directory error = %v, want %v", root.Err, readFailure)
	}
	if scanner.errors != 2 || scanner.files != 2 || root.FileCount != 2 || root.Apparent != 2 {
		t.Fatalf("partial error accounting = tree %+v / files %d errors %d", root, scanner.files, scanner.errors)
	}
	if len(root.Children) != 3 || root.Children[0].Name != "a-bad" || !errors.Is(root.Children[0].Err, statFailure) {
		t.Fatalf("completed/error entries were not retained and sorted: %+v", root.Children)
	}
	if !reader.closed || reader.calls != 3 || reader.maxRequest != 2 {
		t.Fatalf("reader lifecycle = closed %t / calls %d / max request %d", reader.closed, reader.calls, reader.maxRequest)
	}
}

func TestBatchedDirectoryCancellationStopsBeforeNextRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entries := make([]os.DirEntry, 10)
	for i := range entries {
		entries[i] = scriptedDirEntry{name: string(rune('z' - i))}
	}
	reader := &scriptedDirectoryReader{entries: entries}
	root, scanner := runScriptedDirectoryScan(t, ctx, reader, 3, 2, cancel)
	if !errors.Is(root.Err, context.Canceled) {
		t.Fatalf("directory error = %v, want context cancellation", root.Err)
	}
	if scanner.errors != 0 || scanner.files != 3 || len(root.Children) != 3 {
		t.Fatalf("canceled batch accounting = tree %+v / files %d errors %d", root, scanner.files, scanner.errors)
	}
	if reader.calls != 1 || !reader.closed {
		t.Fatalf("canceled scan read %d batches and closed=%t, want one closed batch", reader.calls, reader.closed)
	}
	assertLexicalChildren(t, root)
}

func TestBatchedDirectoryRespectsStatConcurrency(t *testing.T) {
	const workers = 3
	var active, peak atomic.Int64
	entries := make([]os.DirEntry, 48)
	for i := range entries {
		entries[i] = scriptedDirEntry{
			name: string(rune('a' + i%26)),
			onInfo: func() {
				current := active.Add(1)
				for observed := peak.Load(); current > observed; observed = peak.Load() {
					if peak.CompareAndSwap(observed, current) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				active.Add(-1)
			},
		}
	}
	reader := &scriptedDirectoryReader{entries: entries}
	root, scanner := runScriptedDirectoryScan(t, context.Background(), reader, 11, workers, nil)
	if scanner.errors != 0 || scanner.files != int64(len(entries)) || len(root.Children) != len(entries) {
		t.Fatalf("concurrency fixture scan = tree %+v / files %d errors %d", root, scanner.files, scanner.errors)
	}
	if got := peak.Load(); got != workers {
		t.Fatalf("peak stat concurrency = %d, want exactly configured worker count %d", got, workers)
	}
}

func runScriptedDirectoryScan(t *testing.T, ctx context.Context, reader *scriptedDirectoryReader, batchSize, workers int, batchDone func()) (*tree.Node, *scanner) {
	t.Helper()
	s := &scanner{
		ctx:          ctx,
		opts:         Options{Policy: scope.New(), Concurrency: workers},
		sem:          make(chan struct{}, workers),
		dirSlots:     make(chan struct{}, max(0, workers-1)),
		statJobs:     make(chan statTask, workers),
		fileGroups:   make(map[fileKey]*fileGroup),
		dirBatchSize: batchSize,
		batchDone:    batchDone,
		openDir: func(string) (directoryReader, error) {
			return reader, nil
		},
	}
	root := &tree.Node{Name: "synthetic", IsDir: true}
	lt := &liveTree{root: root, files: &s.files, dirs: &s.dirs, errors: &s.errors}
	s.startStatWorkers(workers)
	s.scanDir("/synthetic", "", scriptedFileInfo{name: "synthetic", mode: fs.ModeDir | 0o755}, 0, "", 0, root, nil, lt)
	s.stopStatWorkers()
	return root, s
}

func scanStatsEqual(a, b Stats) bool {
	return a.Files == b.Files && a.Dirs == b.Dirs && a.Errors == b.Errors && a.RootFS == b.RootFS && a.Complete == b.Complete
}

func assertLexicalChildren(t *testing.T, node *tree.Node) {
	t.Helper()
	for i := 1; i < len(node.Children); i++ {
		if node.Children[i-1].Name > node.Children[i].Name {
			t.Fatalf("children are not lexical at %q: %q before %q", node.Path(), node.Children[i-1].Name, node.Children[i].Name)
		}
	}
	for _, child := range node.Children {
		if child.IsDir {
			assertLexicalChildren(t, child)
		}
	}
}

type scriptedDirectoryReader struct {
	entries     []os.DirEntry
	next        int
	terminalErr error
	calls       int
	maxRequest  int
	closed      bool
}

func (r *scriptedDirectoryReader) ReadDir(n int) ([]os.DirEntry, error) {
	r.calls++
	if n > r.maxRequest {
		r.maxRequest = n
	}
	if r.next < len(r.entries) {
		remaining := len(r.entries) - r.next
		if n <= 0 || n > remaining {
			n = remaining
		}
		start := r.next
		r.next += n
		return r.entries[start:r.next], nil
	}
	if r.terminalErr != nil {
		err := r.terminalErr
		r.terminalErr = nil
		return nil, err
	}
	return nil, io.EOF
}

func (r *scriptedDirectoryReader) Close() error {
	r.closed = true
	return nil
}

type scriptedDirEntry struct {
	name   string
	err    error
	onInfo func()
}

func (e scriptedDirEntry) Name() string    { return e.name }
func (scriptedDirEntry) IsDir() bool       { return false }
func (scriptedDirEntry) Type() fs.FileMode { return 0 }
func (e scriptedDirEntry) Info() (fs.FileInfo, error) {
	if e.onInfo != nil {
		e.onInfo()
	}
	if e.err != nil {
		return nil, e.err
	}
	return scriptedFileInfo{name: e.name, size: 1}, nil
}

type scriptedFileInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (i scriptedFileInfo) Name() string      { return i.name }
func (i scriptedFileInfo) Size() int64       { return i.size }
func (i scriptedFileInfo) Mode() fs.FileMode { return i.mode }
func (scriptedFileInfo) ModTime() time.Time  { return time.Unix(1, 0) }
func (i scriptedFileInfo) IsDir() bool       { return i.mode.IsDir() }
func (scriptedFileInfo) Sys() any            { return nil }
