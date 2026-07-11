package index

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phillipod/go-dirstat/internal/tree"
)

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := &Store{dir: dir}
	root := filepath.Join(t.TempDir(), "scan-root")
	snap := FromTree(&tree.Node{Name: "scan-root", IsDir: true, Apparent: 42, Alloc: 512}, "scope-fp", "ext4", 1, 0, 0, time.Now())
	snap.Root = root

	if err := store.Save(snap); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Load(root, snap.Fingerprint)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Root != root || got.Fingerprint != snap.Fingerprint || got.Nodes[0].Alloc != 512 {
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
	store := &Store{dir: t.TempDir()}
	_, err := store.Load("/missing", "scope-fp")
	if !IsMissing(err) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load() error = %v, want fs.ErrNotExist", err)
	}
}

func TestStoreSaveRejectsIncompleteSnapshot(t *testing.T) {
	store := &Store{dir: t.TempDir()}
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
	store := &Store{dir: dir}
	snap := FromTree(&tree.Node{Name: "root", IsDir: true}, "../../outside", "", 0, 1, 0, time.Now())
	snap.Root = "/tmp/root"

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
	store := &Store{dir: dir}
	wantRoot := "/tmp/wanted"
	fingerprint := "scope-fp"
	snap := FromTree(&tree.Node{Name: "other", IsDir: true}, fingerprint, "", 0, 1, 0, time.Now())
	snap.Root = "/tmp/other"
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
