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

func TestDirectoryLoopFallbackUsesCanonicalPath(t *testing.T) {
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
	s := &scanner{
		loopMu:       &sync.Mutex{},
		visited:      make(map[devIno]struct{}),
		visitedPaths: make(map[string]struct{}),
	}
	info := noIdentityFileInfo{}
	if s.seenDirectory(first, info) {
		t.Fatal("first canonical directory was already marked visited")
	}
	if !s.seenDirectory(second, info) {
		t.Fatal("second alias to the same canonical directory was not detected")
	}
}

type noIdentityFileInfo struct{}

func (noIdentityFileInfo) Name() string       { return "directory" }
func (noIdentityFileInfo) Size() int64        { return 0 }
func (noIdentityFileInfo) Mode() os.FileMode  { return os.ModeDir }
func (noIdentityFileInfo) ModTime() time.Time { return time.Time{} }
func (noIdentityFileInfo) IsDir() bool        { return true }
func (noIdentityFileInfo) Sys() any           { return nil }

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
	copy_ := filepath.Join(root, "copy.bin")
	if err := os.Link(orig, copy_); err != nil {
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
