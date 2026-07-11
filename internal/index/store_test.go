package index

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/phillipod/go-dirstat/internal/tree"
)

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "scan-root")
	node := &tree.Node{Name: "scan-root", IsDir: true, Apparent: 42, Alloc: 1024, FileCount: 1}
	node.Adopt(&tree.Node{Name: "file", Apparent: 42, Alloc: 512})
	snap := FromTree(node, "scope-fp", "ext4", 1, 1, 0, true, time.Now())
	snap.Root = root

	if err := store.Save(snap); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Load(root, snap.Fingerprint)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Root != root || got.Fingerprint != snap.Fingerprint || got.Nodes[0].Alloc != 1024 {
		t.Errorf("Load() = %+v, want saved snapshot", got)
	}

	temps, err := filepath.Glob(filepath.Join(dir, ".dirstat-*.tmp"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(temps) != 0 {
		t.Errorf("Save() left temporary files: %v", temps)
	}
}

func TestStoreLoadMissing(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Load("/missing", "scope-fp")
	if !IsMissing(err) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load() error = %v, want fs.ErrNotExist", err)
	}
}

func TestStoreSaveRejectsIncompleteSnapshot(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, snap := range map[string]*Snapshot{
		"nil":                 nil,
		"missing root":        {Fingerprint: "scope-fp"},
		"missing fingerprint": {Root: "/tmp/root"},
		"invalid tree":        {Root: "/tmp/root", Fingerprint: "scope-fp"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.Save(snap); err == nil {
				t.Fatal("Save() error = nil, want validation error")
			}
		})
	}
}

func TestStoreCacheKeyCannotEscapeDirectory(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap := FromTree(&tree.Node{Name: "root", IsDir: true}, "../../outside", "", 0, 1, 0, true, time.Now())
	snap.Root = testIndexAbsolutePath("tmp", "root")

	if err := store.Save(snap); err != nil {
		t.Fatal(err)
	}
	path := store.pathFor(snap.Root, snap.Fingerprint)
	if filepath.Dir(path) != dir {
		t.Fatalf("cache path escaped store: %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}
}

func TestStoreLoadRejectsSnapshotForDifferentRoot(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := testIndexAbsolutePath("tmp", "wanted")
	fingerprint := "scope-fp"
	snap := FromTree(&tree.Node{Name: "other", IsDir: true}, fingerprint, "", 0, 1, 0, true, time.Now())
	snap.Root = testIndexAbsolutePath("tmp", "other")
	data, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.pathFor(wantRoot, fingerprint), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Load(wantRoot, fingerprint); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Load() error = %v, want ErrIncompatible", err)
	}
}

func TestStoreSaveReportsWriteFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &Store{dir: file}
	snap := &Snapshot{Root: "/tmp/root", Fingerprint: "scope-fp"}
	if err := store.Save(snap); err == nil {
		t.Fatal("Save() error = nil, want temporary-file creation error")
	}
}

func TestStoreWriteEnforcesGlobalTTL(t *testing.T) {
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	store, err := NewStoreAtWithPolicy(t.TempDir(), Policy{MaxBytes: 1 << 20, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return base }
	old := testStoreSnapshot(filepath.Join(t.TempDir(), "old"), "old-fingerprint", base)
	if err := store.Save(old); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return base.Add(2 * time.Hour) }
	current := testStoreSnapshot(filepath.Join(t.TempDir(), "current"), "current-fingerprint", base.Add(2*time.Hour))
	if err := store.Save(current); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Root != current.Root || !entries[0].Valid {
		t.Fatalf("post-TTL entries = %#v", entries)
	}
	if _, err := os.Lstat(store.pathFor(old.Root, old.Fingerprint)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expired cache entry remains: %v", err)
	}
}

func testStoreSnapshot(root, fingerprint string, at time.Time) *Snapshot {
	node := &tree.Node{Name: filepath.Base(root), IsDir: true}
	snapshot := FromTree(node, fingerprint, "", 0, 1, 0, true, at)
	snapshot.Root = root
	return snapshot
}

func testIndexAbsolutePath(parts ...string) string {
	if runtime.GOOS == windowsOS {
		return filepath.Join(append([]string{`C:\`}, parts...)...)
	}
	return filepath.Join(append([]string{string(filepath.Separator)}, parts...)...)
}

func TestAge(t *testing.T) {
	if got := Age(nil); got != 0 {
		t.Fatalf("Age(nil) = %v, want 0", got)
	}
	want := 2 * time.Minute
	got := Age(&Snapshot{ScannedAt: time.Now().Add(-want)})
	if got < want || got > want+time.Second {
		t.Fatalf("Age() = %v, want about %v", got, want)
	}
	if got := Age(&Snapshot{ScannedAt: time.Now().Add(time.Hour)}); got != 0 {
		t.Fatalf("Age(future snapshot) = %v, want 0", got)
	}
}
