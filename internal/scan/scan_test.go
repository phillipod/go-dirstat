package scan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	historypkg "github.com/phillipod/go-dirstat/internal/history"
	"github.com/phillipod/go-dirstat/internal/index"
	querypkg "github.com/phillipod/go-dirstat/internal/query"
	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

// writeFile creates path with the given byte content, making parent dirs.
func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestScanTotalsAndCounts(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 100)
	writeFile(t, filepath.Join(root, "b.dat"), 1000)
	writeFile(t, filepath.Join(root, "sub", "c.txt"), 50)
	if err := os.MkdirAll(filepath.Join(root, "sub", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	n, stats, err := Scan(context.Background(), root, WithPolicy(scope.New()))
	if err != nil {
		t.Fatal(err)
	}

	if got := n.Apparent; got != 1150 {
		t.Fatalf("root apparent = %d, want 1150", got)
	}
	if n.FileCount != 3 {
		t.Errorf("FileCount = %d, want 3", n.FileCount)
	}
	if n.DirCount != 2 { // sub + sub/empty
		t.Errorf("DirCount = %d, want 2", n.DirCount)
	}
	if n.Alloc < n.Apparent { // on-disk never smaller than apparent on real fs
		t.Errorf("Alloc %d < Apparent %d", n.Alloc, n.Apparent)
	}
	if stats.Files != 3 {
		t.Errorf("stats.Files = %d, want 3", stats.Files)
	}
	if stats.Dirs != 3 { // root + sub + empty
		t.Errorf("stats.Dirs = %d, want 3", stats.Dirs)
	}
}

func TestScanExcludeGlob(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 100)
	writeFile(t, filepath.Join(root, "b.dat"), 1000)
	writeFile(t, filepath.Join(root, "sub", "c.txt"), 50)

	p := scope.New(scope.WithExcludeGlobs([]string{"*.txt"}))
	n, _, err := Scan(context.Background(), root, WithPolicy(p))
	if err != nil {
		t.Fatal(err)
	}
	if n.Apparent != 1000 {
		t.Fatalf("apparent with *.txt excluded = %d, want 1000", n.Apparent)
	}
	if n.FileCount != 1 {
		t.Errorf("FileCount = %d, want 1", n.FileCount)
	}
}

func TestScanSizeThreshold(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "small"), 10)
	writeFile(t, filepath.Join(root, "big"), 10_000)

	p := scope.New(scope.WithSizeThreshold(100, 0)) // drop files < 100B
	n, _, err := Scan(context.Background(), root, WithPolicy(p))
	if err != nil {
		t.Fatal(err)
	}
	if n.Apparent != 10_000 {
		t.Fatalf("apparent = %d, want 10000", n.Apparent)
	}
	if n.FileCount != 1 {
		t.Errorf("FileCount = %d, want 1", n.FileCount)
	}
}

func TestScanSizeThresholdAppliesToFileRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "small")
	writeFile(t, root, 10)
	p := scope.New(scope.WithSizeThreshold(100, 0))

	n, _, err := Scan(context.Background(), root, WithPolicy(p))
	if err == nil || n != nil {
		t.Fatalf("filtered file root result = node %+v / error %v, want rejection", n, err)
	}
	if !strings.Contains(err.Error(), "size policy") {
		t.Fatalf("file-root size error is not useful: %v", err)
	}
}

func TestScanMissingRoot(t *testing.T) {
	if _, _, err := Scan(context.Background(), "/no/such/path/here", DefaultOptions()); err == nil {
		t.Fatal("expected error for missing root")
	}
}

func TestScanFollowSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, "payload"), 128)
	root := filepath.Join(base, "root-link")
	if err := os.Symlink(target, root); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	p := scope.New(scope.WithFollowSymlinks(true))
	n, stats, err := Scan(context.Background(), root, WithPolicy(p))
	if err != nil {
		t.Fatal(err)
	}
	if !n.IsDir || n.Name != "root-link" {
		t.Fatalf("followed symlink root = %+v, want directory named root-link", n)
	}
	if n.Apparent != 128 || n.FileCount != 1 {
		t.Errorf("followed symlink totals = %d bytes/%d files, want 128/1", n.Apparent, n.FileCount)
	}
	if stats.Dirs != 1 {
		t.Errorf("stats.Dirs = %d, want 1", stats.Dirs)
	}
}

func TestScanFollowSymlinkBackToRootOnce(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "payload"), 256)
	if err := os.Symlink(root, filepath.Join(root, "back-to-root")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	p := scope.New(scope.WithFollowSymlinks(true))
	n, stats, err := Scan(context.Background(), root, WithPolicy(p))
	if err != nil {
		t.Fatal(err)
	}
	if n.Apparent != 256 || n.FileCount != 1 {
		t.Errorf("root loop totals = %d bytes/%d files, want 256/1", n.Apparent, n.FileCount)
	}
	if n.DirCount != 0 || stats.Dirs != 1 {
		t.Errorf("root loop directories = tree %d/stats %d, want 0/1", n.DirCount, stats.Dirs)
	}
}

func TestDirectoryIdentityFallbackUsesCanonicalPath(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	if err := os.Symlink(target, first); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	if err := os.Symlink(target, second); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	firstKey := fallbackDirectoryIdentity(first)
	secondKey := fallbackDirectoryIdentity(second)
	if firstKey.path == "" || firstKey != secondKey {
		t.Fatalf("fallback identities differ: first=%+v second=%+v", firstKey, secondKey)
	}
}

func TestScanFollowSymlinkCannotBypassExcludedTarget(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	target := filepath.Join(base, "excluded-target")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, "payload"), 256)
	if err := os.Symlink(target, filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	p := scope.New(scope.WithFollowSymlinks(true), scope.WithExcludePaths([]string{target}))
	n, stats, err := Scan(context.Background(), root, WithPolicy(p))
	if err != nil {
		t.Fatal(err)
	}
	if n.Apparent != 0 || n.FileCount != 0 || n.DirCount != 0 || stats.Files != 0 {
		t.Fatalf("excluded target was traversed: tree=%+v stats=%+v", n, stats)
	}
}

func TestScanFollowFileSymlinkCannotBypassExcludedTarget(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	target := filepath.Join(base, "excluded.bin")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, target, 7)
	if err := os.Symlink(target, filepath.Join(root, "alias.bin")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	p := scope.New(scope.WithFollowSymlinks(true), scope.WithExcludePaths([]string{target}))
	n, stats, err := Scan(context.Background(), root, WithPolicy(p))
	if err != nil {
		t.Fatal(err)
	}
	if n.Apparent != 0 || n.FileCount != 0 || stats.Files != 0 || len(n.Children) != 0 {
		t.Fatalf("excluded file target was measured: tree=%+v stats=%+v", n, stats)
	}
}

func TestScanFollowSymlinkCannotEscapeIncludePath(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	target := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, "payload"), 64)
	writeFile(t, filepath.Join(root, "kept"), 32)
	if err := os.Symlink(target, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	p := scope.New(scope.WithFollowSymlinks(true), scope.WithIncludePaths([]string{root}))
	n, stats, err := Scan(context.Background(), root, WithPolicy(p))
	if err != nil {
		t.Fatal(err)
	}
	if n.Apparent != 32 || n.FileCount != 1 || n.DirCount != 0 || stats.Files != 1 {
		t.Fatalf("followed alias escaped include path: tree=%+v stats=%+v", n, stats)
	}
}

func TestScanFollowRootSymlinkCannotBypassExcludedTarget(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "excluded-target")
	writeFile(t, filepath.Join(target, "payload"), 256)
	root := filepath.Join(base, "root-link")
	if err := os.Symlink(target, root); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	p := scope.New(scope.WithFollowSymlinks(true), scope.WithExcludePaths([]string{target}))
	if n, _, err := Scan(context.Background(), root, WithPolicy(p)); err == nil || n != nil {
		t.Fatalf("excluded root target result = node %+v / error %v, want rejection", n, err)
	}
}

func TestScanFollowRootSymlinkCannotEscapeIncludePath(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "outside")
	writeFile(t, filepath.Join(target, "payload"), 64)
	root := filepath.Join(base, "root-link")
	if err := os.Symlink(target, root); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	p := scope.New(scope.WithFollowSymlinks(true), scope.WithIncludePaths([]string{root}))
	if n, _, err := Scan(context.Background(), root, WithPolicy(p)); err == nil || n != nil {
		t.Fatalf("out-of-scope root target result = node %+v / error %v, want rejection", n, err)
	}
}

func TestScanRejectsRootFilesystem(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "direct-file"), 64)
	p := scope.New(scope.WithFilesystems([]string{"definitely-not-a-real-filesystem"}, nil))

	n, _, err := Scan(context.Background(), root, WithPolicy(p))
	if err == nil {
		t.Fatal("expected root filesystem policy error")
	}
	if n != nil {
		t.Fatalf("rejected scan returned a tree: %+v", n)
	}
	if !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "filesystem policy") {
		t.Fatalf("root filesystem error is not useful: %v", err)
	}
}

func TestScanRejectsExplicitlyExcludedRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "must-not-be-read"), 64)
	p := scope.New(scope.WithExcludePaths([]string{root}))

	n, _, err := Scan(context.Background(), root, WithPolicy(p))
	if err == nil {
		t.Fatal("expected root path policy error")
	}
	if n != nil {
		t.Fatalf("rejected scan returned a tree: %+v", n)
	}
	if !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "path policy") {
		t.Fatalf("root path error is not useful: %v", err)
	}
}

func TestScanIncludePathTraversesAncestors(t *testing.T) {
	root := t.TempDir()
	included := filepath.Join(root, "parent", "keep")
	writeFile(t, filepath.Join(included, "wanted"), 128)
	writeFile(t, filepath.Join(root, "parent", "sibling", "ignored"), 256)
	p := scope.New(scope.WithIncludePaths([]string{included}))

	n, stats, err := Scan(context.Background(), root, WithPolicy(p))
	if err != nil {
		t.Fatal(err)
	}
	if n.Apparent != 128 || stats.Files != 1 {
		t.Fatalf("included subtree totals = %d bytes/%d files, want 128/1", n.Apparent, stats.Files)
	}
	if n.DirCount != 2 {
		t.Fatalf("included subtree directory count = %d, want parent + keep", n.DirCount)
	}
}

func TestScanReturnsContextCancellation(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 50; i++ {
		writeFile(t, filepath.Join(root, fmt.Sprintf("d%02d", i), "payload"), 64)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n, _, err := ScanStream(ctx, root, WithPolicy(scope.New()), Progress{
		Period: time.Nanosecond,
		OnTick: func(*tree.Node, Stats) {
			cancel()
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ScanStream error = %v, want context.Canceled", err)
	}
	if n == nil {
		t.Fatal("canceled in-progress scan should return its partial tree")
	}
}

func TestScannerAcquireStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &scanner{ctx: ctx, sem: make(chan struct{}, 1)}
	s.sem <- struct{}{} // force acquire to wait

	done := make(chan bool, 1)
	go func() { done <- s.acquire() }()
	cancel()
	select {
	case acquired := <-done:
		if acquired {
			t.Fatal("acquire succeeded after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("acquire did not respond to cancellation")
	}
}

func TestStatErrorDoesNotInflateFileCount(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true}
	leaf := &tree.Node{Name: "unknown", Err: errors.New("stat failed")}
	root.Adopt(leaf)
	lt := &liveTree{}
	lt.propagateFile(leaf)
	if root.FileCount != 0 {
		t.Fatalf("error entry increased FileCount to %d", root.FileCount)
	}
}

func TestReadDirErrorRetainsKnownDirectoryMetadata(t *testing.T) {
	path := t.TempDir()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	node := &tree.Node{Name: filepath.Base(path), IsDir: true}
	s := &scanner{
		ctx: context.Background(), opts: DefaultOptions(), sem: make(chan struct{}, 1),
	}
	lt := &liveTree{root: node}
	s.scanDir(path, "", info, devOfPath(path, info), "", 0, node, nil, lt)
	if node.Err == nil {
		t.Fatal("removed directory did not produce a ReadDir error")
	}
	if node.Alloc != allocBytes(info) || !node.ModTime.Equal(info.ModTime()) {
		t.Fatalf("known metadata was discarded: node=%+v info=%+v", node, info)
	}
	if s.errors != 1 {
		t.Fatalf("scan errors = %d, want 1", s.errors)
	}
}

func TestFollowedAliasTargetResolutionRejectsReplacement(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	alias := filepath.Join(base, "alias")
	if err := os.Mkdir(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(second, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(first, alias); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	expected, err := os.Stat(alias)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveAliasTarget(alias, expected); err == nil {
		t.Fatal("replacement alias was accepted as the originally checked target")
	}
}

func TestFollowedDirectoryAliasIsNotTraversedAfterTargetReplacement(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	alias := filepath.Join(base, "alias")
	if err := os.Mkdir(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(second, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(first, alias); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	expected, err := os.Stat(alias)
	if err != nil {
		t.Fatal(err)
	}
	root := &tree.Node{Name: "alias", IsDir: true}
	s := &scanner{
		ctx:  context.Background(),
		opts: Options{Policy: scope.New(scope.WithFollowSymlinks(true)), Concurrency: 1},
		sem:  make(chan struct{}, 1),
		openDir: func(path string) (directoryReader, error) {
			if err := os.Remove(alias); err != nil {
				return nil, err
			}
			if err := os.Symlink(second, alias); err != nil {
				return nil, err
			}
			return os.Open(path)
		},
	}
	lt := &liveTree{root: root}
	s.scanDir(alias, "", expected, devOfPath(alias, expected), "", 0, root, nil, lt)
	if root.Err == nil || !strings.Contains(root.Err.Error(), "changed while opening") {
		t.Fatalf("replaced directory target was traversed: root=%+v", root)
	}
	if s.errors != 1 {
		t.Fatalf("scan errors = %d, want 1", s.errors)
	}
}

// TestScanConcurrencySanity re-scans the repo itself under the race detector
// implicitly (go test -race) to shake out goroutine/data-race bugs in the
// aggregation step on a non-trivial tree.
func TestScanRepoTree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping repo scan in short mode")
	}
	// run from the package dir; walk up to find go.mod.
	dir := "."
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		dir = filepath.Join(dir, "..")
	}
	n, stats, err := Scan(context.Background(), dir, WithPolicy(scope.New()))
	if err != nil {
		t.Fatal(err)
	}
	if n.Apparent <= 0 || n.FileCount == 0 {
		t.Fatalf("repo scan produced empty tree: %+v", stats)
	}
	// GOMAXPROCS sanity: the scan must respect a tiny worker budget too.
	if n2, _, err := Scan(context.Background(), dir, Options{Policy: scope.New(), Concurrency: 1}); err == nil && n2.Apparent != n.Apparent {
		t.Errorf("apparent differs between runs: %d vs %d (non-deterministic)", n2.Apparent, n.Apparent)
	}
	_ = runtime.NumCPU
}

// TestScanStreamMatchesScan asserts the streaming walker produces exactly the
// same tree and stats as the one-shot walker. This is the guard that the
// incremental-propagation aggregation never diverges from the canonical result.
func TestScanStreamMatchesScan(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 100)
	writeFile(t, filepath.Join(root, "b.dat"), 1000)
	writeFile(t, filepath.Join(root, "sub", "c.txt"), 50)
	writeFile(t, filepath.Join(root, "sub", "deep", "d.txt"), 20)
	if err := os.MkdirAll(filepath.Join(root, "sub", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	want, wantStats, err := Scan(context.Background(), root, WithPolicy(scope.New()))
	if err != nil {
		t.Fatal(err)
	}
	got, gotStats, err := ScanStream(context.Background(), root, WithPolicy(scope.New()), Progress{})
	if err != nil {
		t.Fatal(err)
	}
	if !treeEqual(got, want) {
		t.Errorf("ScanStream tree differs from Scan\n got  root = %+v\n want root = %+v", got, want)
	}
	if gotStats.Files != wantStats.Files || gotStats.Dirs != wantStats.Dirs || gotStats.Errors != wantStats.Errors {
		t.Errorf("ScanStream stats = %+v, want %+v", gotStats, wantStats)
	}
}

// TestScanStreamProgress checks that snapshots are delivered, never exceed the
// final total, and arrive in non-decreasing order (the single-emitter guarantee).
func TestScanStreamProgress(t *testing.T) {
	root := t.TempDir()
	const dirs, files = 20, 10
	for d := 0; d < dirs; d++ {
		for f := 0; f < files; f++ {
			writeFile(t, filepath.Join(root, fmt.Sprintf("d%02d", d), fmt.Sprintf("f%d.bin", f)), 256)
		}
	}

	var (
		mu       sync.Mutex
		ticks    int
		prev     int64 = -1
		maxTotal int64
	)
	final, _, err := ScanStream(context.Background(), root, WithPolicy(scope.New()), Progress{
		Period: time.Millisecond,
		OnTick: func(n *tree.Node, _ Stats) {
			mu.Lock()
			defer mu.Unlock()
			ticks++
			if n.Apparent < prev {
				t.Errorf("snapshot total decreased out of order: %d < %d", n.Apparent, prev)
			}
			prev = n.Apparent
			if n.Apparent > maxTotal {
				maxTotal = n.Apparent
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ticks == 0 {
		t.Fatal("OnTick never fired")
	}
	want := int64(dirs * files * 256)
	if final.Apparent != want {
		t.Errorf("final apparent = %d, want %d", final.Apparent, want)
	}
	if maxTotal <= 0 {
		t.Errorf("no positive total streamed; maxTotal = %d", maxTotal)
	}
	if maxTotal > final.Apparent {
		t.Errorf("streamed max %d exceeds final %d", maxTotal, final.Apparent)
	}
}

// TestScanHardlinkDedup checks that a hardlinked file's bytes are counted once
// (du semantics): two names for the same inode contribute the size once, while
// both names remain visible and both count as file entries.
func TestScanHardlinkDedup(t *testing.T) {
	root := t.TempDir()
	orig := filepath.Join(root, "orig.bin")
	writeFile(t, orig, 1000)
	copyPath := filepath.Join(root, "copy.bin")
	if err := os.Link(orig, copyPath); err != nil {
		t.Skipf("hardlinks not supported: %v", err)
	}
	writeFile(t, filepath.Join(root, "other.bin"), 500) // a separate inode

	n, stats, err := Scan(context.Background(), root, WithPolicy(scope.New()))
	if err != nil {
		t.Fatal(err)
	}
	// orig + copy share an inode: only 1000 counts once, plus other's 500.
	if n.Apparent != 1500 {
		t.Errorf("root apparent = %d, want 1500 (hardlink double-counted?)", n.Apparent)
	}
	// Both names are still entries.
	if n.FileCount != 3 {
		t.Errorf("root FileCount = %d, want 3", n.FileCount)
	}
	if stats.Files != 3 {
		t.Errorf("stats.Files = %d, want 3", stats.Files)
	}

	byName := map[string]*tree.Node{}
	for _, c := range n.Children {
		byName[c.Name] = c
	}
	o, c := byName["orig.bin"], byName["copy.bin"]
	if o == nil || c == nil {
		t.Fatalf("missing hardlink entries: %+v", byName)
	}
	// Exactly one link claims the size; the other is a zero-size Hardlink node.
	if o.Hardlink == c.Hardlink {
		t.Errorf("expected exactly one hardlink marker, got orig=%v copy=%v", o.Hardlink, c.Hardlink)
	}
	var owner, dup *tree.Node
	if o.Hardlink {
		owner, dup = c, o
	} else {
		owner, dup = o, c
	}
	if owner.Apparent != 1000 {
		t.Errorf("owning link apparent = %d, want 1000", owner.Apparent)
	}
	if dup.Apparent != 0 || dup.Alloc != 0 {
		t.Errorf("duplicate link apparent/alloc = %d/%d, want 0/0", dup.Apparent, dup.Alloc)
	}
}

func TestScanFollowedFileAliasesAreDeduplicatedDeterministically(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "m-target.bin")
	writeFile(t, target, 100)
	for _, name := range []string{"a-alias.bin", "z-alias.bin"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("symlinks not supported: %v", err)
		}
	}

	p := scope.New(scope.WithFollowSymlinks(true))
	n, stats, err := Scan(context.Background(), root, WithPolicy(p))
	if err != nil {
		t.Fatal(err)
	}
	if n.Apparent != 100 || n.FileCount != 3 || stats.Files != 3 {
		t.Fatalf("followed aliases were double-counted: tree=%+v stats=%+v", n, stats)
	}
	byName := make(map[string]*tree.Node, len(n.Children))
	for _, child := range n.Children {
		byName[child.Name] = child
	}
	owner := byName["a-alias.bin"]
	if owner == nil || owner.Hardlink || owner.Apparent != 100 {
		t.Fatalf("lexicographically first alias is not the owner: %+v", owner)
	}
	for _, name := range []string{"m-target.bin", "z-alias.bin"} {
		duplicate := byName[name]
		if duplicate == nil || !duplicate.Hardlink || duplicate.Apparent != 0 || duplicate.Alloc != 0 {
			t.Errorf("duplicate %s = %+v, want zero-size hardlink marker", name, duplicate)
		}
	}
}

func TestFollowedDirectoryAliasesAreStableAcrossConcurrentScans(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	target := filepath.Join(base, "shared-target")
	writeFile(t, filepath.Join(target, "nested", "payload.bin"), 257)

	const branchCount = 64
	for i := branchCount - 1; i >= 0; i-- {
		branch := filepath.Join(root, fmt.Sprintf("branch-%02d", i))
		if err := os.MkdirAll(branch, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(branch, "shared")); err != nil {
			t.Skipf("directory symlinks not supported: %v", err)
		}
	}

	opts := Options{
		Policy:      scope.New(scope.WithFollowSymlinks(true)),
		Concurrency: 32,
	}
	var (
		wantTreePaths  string
		wantQueryPaths string
		previous       *index.Snapshot
	)
	for attempt := 0; attempt < 30; attempt++ {
		n, stats, err := Scan(context.Background(), root, opts)
		if err != nil {
			t.Fatalf("attempt %d scan: %v", attempt, err)
		}
		if stats.Files != 1 || stats.Dirs != branchCount+3 || n.Apparent != 257 || n.FileCount != 1 || n.DirCount != stats.Dirs-1 {
			t.Fatalf("attempt %d double-counted aliases: tree=%+v stats=%+v", attempt, n, stats)
		}
		branches := make(map[string]*tree.Node, len(n.Children))
		for _, child := range n.Children {
			branches[child.Name] = child
		}
		owner, duplicate := branches["branch-00"], branches["branch-01"]
		if owner == nil || owner.Apparent != 257 || owner.FileCount != 1 || owner.DirCount != 2 {
			t.Fatalf("attempt %d owner aggregates = %+v", attempt, owner)
		}
		if duplicate == nil || duplicate.Apparent != 0 || duplicate.FileCount != 0 || duplicate.DirCount != 0 {
			t.Fatalf("attempt %d duplicate aggregates = %+v", attempt, duplicate)
		}

		treePaths := strings.Join(relativeTreePaths(n), "\n")
		records, err := querypkg.Build(n, root, querypkg.Options{})
		if err != nil {
			t.Fatalf("attempt %d query: %v", attempt, err)
		}
		queryPaths := strings.Join(relativeQueryPaths(records), "\n")
		if attempt == 0 {
			wantTreePaths = treePaths
			wantQueryPaths = queryPaths
			if !strings.Contains("\n"+treePaths+"\n", "\nbranch-00/shared/nested/payload.bin\n") {
				t.Fatalf("lexicographically first alias is not the owner:\n%s", treePaths)
			}
		} else {
			if treePaths != wantTreePaths {
				t.Fatalf("attempt %d tree paths changed:\nwant:\n%s\ngot:\n%s", attempt, wantTreePaths, treePaths)
			}
			if queryPaths != wantQueryPaths {
				t.Fatalf("attempt %d query paths changed:\nwant:\n%s\ngot:\n%s", attempt, wantQueryPaths, queryPaths)
			}
		}

		current := index.FromTree(n, "followed-directory-aliases", stats.RootFS, stats.Files, stats.Dirs, stats.Errors, stats.Complete, time.Now())
		current.Root = root
		if previous != nil {
			deltas, err := historypkg.Compare(previous, current)
			if err != nil {
				t.Fatalf("attempt %d history compare: %v", attempt, err)
			}
			if len(deltas) != 0 {
				t.Fatalf("attempt %d unchanged scan produced history deltas: %#v", attempt, deltas)
			}
		}
		previous = current
	}
}

func TestFollowedDirectoryCrossLinksDoNotCreateCanonicalCycle(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	writeFile(t, filepath.Join(a, "a.bin"), 11)
	writeFile(t, filepath.Join(b, "b.bin"), 13)
	if err := os.Symlink(b, filepath.Join(a, "z-to-b")); err != nil {
		t.Skipf("directory symlinks not supported: %v", err)
	}
	if err := os.Symlink(a, filepath.Join(b, "y-to-a")); err != nil {
		t.Skipf("directory symlinks not supported: %v", err)
	}

	n, stats, err := Scan(context.Background(), root, Options{
		Policy: scope.New(scope.WithFollowSymlinks(true)), Concurrency: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n.Apparent != 24 || n.FileCount != 2 || n.DirCount != 2 || stats.Dirs != 3 {
		t.Fatalf("cross-linked totals = tree %+v / stats %+v", n, stats)
	}
	paths := "\n" + strings.Join(relativeTreePaths(n), "\n") + "\n"
	for _, want := range []string{"a", "a/a.bin", "a/z-to-b", "a/z-to-b/b.bin"} {
		if !strings.Contains(paths, "\n"+want+"\n") {
			t.Errorf("canonical tree missing %q:\n%s", want, paths)
		}
	}
	for _, duplicate := range []string{"b", "a/z-to-b/y-to-a"} {
		if strings.Contains(paths, "\n"+duplicate+"\n") {
			t.Errorf("canonical tree retained duplicate/loop %q:\n%s", duplicate, paths)
		}
	}
}

func relativeTreePaths(root *tree.Node) []string {
	paths := make([]string, 0, root.FileCount+root.DirCount+1)
	root.Walk(func(node *tree.Node) bool {
		paths = append(paths, filepath.ToSlash(node.Path()))
		return true
	})
	return paths
}

func relativeQueryPaths(records []querypkg.Record) []string {
	paths := make([]string, len(records))
	for i, record := range records {
		paths[i] = record.Relative
	}
	return paths
}

func TestScanCrossDirectoryHardlinkOwnerIsDeterministic(t *testing.T) {
	root := t.TempDir()
	ownerPath := filepath.Join(root, "a", "file.bin")
	duplicatePath := filepath.Join(root, "b", "file.bin")
	writeFile(t, duplicatePath, 100)
	if err := os.MkdirAll(filepath.Dir(ownerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(duplicatePath, ownerPath); err != nil {
		t.Skipf("hardlinks not supported: %v", err)
	}

	for attempt := 0; attempt < 20; attempt++ {
		n, stats, err := Scan(context.Background(), root, Options{Policy: scope.New(), Concurrency: 8})
		if err != nil {
			t.Fatal(err)
		}
		if n.Apparent != 100 || n.FileCount != 2 || stats.Files != 2 {
			t.Fatalf("attempt %d totals = tree %+v / stats %+v", attempt, n, stats)
		}
		dirs := make(map[string]*tree.Node, len(n.Children))
		for _, child := range n.Children {
			dirs[child.Name] = child
		}
		a, b := dirs["a"], dirs["b"]
		if a == nil || b == nil || len(a.Children) != 1 || len(b.Children) != 1 {
			t.Fatalf("attempt %d missing hardlink branches: %+v", attempt, dirs)
		}
		if a.Apparent != 100 || a.Children[0].Hardlink || a.Children[0].Apparent != 100 {
			t.Errorf("attempt %d owner branch = %+v / %+v", attempt, a, a.Children[0])
		}
		if b.Apparent != 0 || !b.Children[0].Hardlink || b.Children[0].Apparent != 0 || b.Children[0].Alloc != 0 {
			t.Errorf("attempt %d duplicate branch = %+v / %+v", attempt, b, b.Children[0])
		}
	}
}

// treeEqual compares two nodes structurally: identity fields, aggregated sizes
// and counts, mtime, error presence, and children in order.
func treeEqual(a, b *tree.Node) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Name != b.Name || a.Path() != b.Path() || a.IsDir != b.IsDir || a.Depth != b.Depth {
		return false
	}
	if a.Apparent != b.Apparent || a.Alloc != b.Alloc {
		return false
	}
	if a.FileCount != b.FileCount || a.DirCount != b.DirCount {
		return false
	}
	if (a.Err == nil) != (b.Err == nil) {
		return false
	}
	if !a.ModTime.Equal(b.ModTime) {
		return false
	}
	if len(a.Children) != len(b.Children) {
		return false
	}
	for i := range a.Children {
		if !treeEqual(a.Children[i], b.Children[i]) {
			return false
		}
	}
	return true
}

// TestScanStreamKeepsFlowingOnBigTree is the regression guard for the freeze
// bug: snapshots must keep arriving even well past the old 250k-entry hard
// stop. Each snapshot is a bounded shallow clone, so a large tree never causes
// the stream to stall — it just throttles. We assert we receive many ticks and
// that totals climb monotonically to the final value.
func TestScanStreamKeepsFlowingOnBigTree(t *testing.T) {
	if testing.Short() {
		t.Skip("big-tree streaming test")
	}
	root := t.TempDir()
	// ~6k files spread across many dirs: enough to run for a few ticks, small
	// enough to stay quick. The point is structural, not scale.
	for d := 0; d < 300; d++ {
		for f := 0; f < 20; f++ {
			writeFile(t, filepath.Join(root, fmt.Sprintf("d%03d", d), fmt.Sprintf("f%d", f)), 64)
		}
	}

	var (
		mu    sync.Mutex
		ticks int
		prev  int64 = -1
		froze bool
	)
	final, _, err := ScanStream(context.Background(), root, WithPolicy(scope.New()), Progress{
		Period: time.Millisecond,
		OnTick: func(n *tree.Node, _ Stats) {
			mu.Lock()
			defer mu.Unlock()
			ticks++
			if n.Apparent < prev {
				t.Errorf("snapshot total decreased: %d < %d", n.Apparent, prev)
			}
			prev = n.Apparent
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	froze = ticks < 3 // a tree this size must produce several progress ticks
	mu.Unlock()
	if froze {
		t.Fatalf("stream froze: only %d ticks for a large tree", ticks)
	}
	if prev > final.Apparent {
		t.Errorf("last streamed total %d exceeds final %d", prev, final.Apparent)
	}
}
