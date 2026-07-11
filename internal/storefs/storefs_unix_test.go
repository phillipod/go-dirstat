//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package storefs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRootCapabilityCannotBeRedirectedByPathReplacement(t *testing.T) {
	base := t.TempDir()
	storePath := filepath.Join(base, "store")
	if err := os.Mkdir(storePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storePath, "record"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	displaced := filepath.Join(base, "displaced")
	if err := os.Rename(storePath, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(storePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storePath, "record"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	assertRootFile(t, root, "record", "original")
	if err := root.AtomicWrite("published", ".published-*.tmp", []byte("unsafe")); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("AtomicWrite() error = %v, want path-replacement rejection", err)
	}
	if err := root.RemoveRegular("record"); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("RemoveRegular() error = %v, want path-replacement rejection", err)
	}
	if _, err := os.Lstat(filepath.Join(storePath, "published")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("replacement root received publication: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(displaced, "record")); err != nil || string(got) != "original" {
		t.Fatalf("displaced record = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(storePath, "record")); err != nil || string(got) != "replacement" {
		t.Fatalf("replacement record = %q, %v", got, err)
	}
}

func TestRemoveEmptyDirRejectsReplacedRoot(t *testing.T) {
	base := t.TempDir()
	storePath := filepath.Join(base, "store")
	if err := os.MkdirAll(filepath.Join(storePath, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	displaced := filepath.Join(base, "displaced")
	if err := os.Rename(storePath, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(storePath, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveEmptyDirContext(context.Background(), "empty"); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("RemoveEmptyDirContext() error = %v, want path-replacement rejection", err)
	}
	for _, path := range []string{filepath.Join(displaced, "empty"), filepath.Join(storePath, "empty")} {
		if info, err := os.Lstat(path); err != nil || !info.IsDir() {
			t.Fatalf("directory %q changed: info=%v err=%v", path, info, err)
		}
	}
}

func TestOwnershipRequiresPrivateCurrentUserDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	if owned, issue := root.Ownership("index"); owned || !strings.Contains(issue, "not private") {
		t.Fatalf("public-directory ownership = %v, %q", owned, issue)
	}
	if err := root.EnsureOwnershipContext(context.Background(), "index", false); err == nil || !strings.Contains(err.Error(), "not private") {
		t.Fatalf("non-adopting marker creation error = %v", err)
	}
	if _, err := root.Lstat(markerName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("marker after rejected private check: %v", err)
	}
	if err := root.EnsureOwnershipContext(context.Background(), "index", true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("adopted directory mode = %04o, want 0700", got)
	}
	if owned, issue := root.Ownership("index"); !owned || issue != "" {
		t.Fatalf("adopted current-root ownership = %v, %q", owned, issue)
	}
}

func TestPrivateStoreChecksModeAndEffectiveOwner(t *testing.T) {
	uid := uint32(os.Geteuid())
	private := fakeFileInfo{mode: fs.ModeDir | 0o700, sys: &syscall.Stat_t{Uid: uid}}
	if !privateStore("", private) {
		t.Fatal("privateStore() rejected private current-user directory")
	}
	public := fakeFileInfo{mode: fs.ModeDir | 0o755, sys: &syscall.Stat_t{Uid: uid}}
	if privateStore("", public) {
		t.Fatal("privateStore() accepted group/world-accessible directory")
	}
	otherUID := uid + 1
	if otherUID == uid {
		otherUID--
	}
	foreign := fakeFileInfo{mode: fs.ModeDir | 0o700, sys: &syscall.Stat_t{Uid: otherUID}}
	if privateStore("", foreign) {
		t.Fatal("privateStore() accepted directory owned by another user")
	}
}

type fakeFileInfo struct {
	mode fs.FileMode
	sys  any
}

func (fakeFileInfo) Name() string        { return "store" }
func (fakeFileInfo) Size() int64         { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode { return f.mode }
func (fakeFileInfo) ModTime() time.Time  { return time.Time{} }
func (f fakeFileInfo) IsDir() bool       { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any          { return f.sys }
