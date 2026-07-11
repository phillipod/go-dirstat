package storefs

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveStoreDirRejectsFinalSymlink(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := ResolveStoreDir(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("ResolveStoreDir() error = %v, want final-symlink rejection", err)
	}
	if _, err := OpenRoot(link); err == nil {
		t.Fatal("OpenRoot() accepted a symlinked store root")
	}
}

func TestRootRejectsFinalSymlinksWithoutTouchingTargets(t *testing.T) {
	root := openTestRoot(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root.Path(), "linked")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, _, err := root.ReadRegular("linked", 1024); err == nil {
		t.Fatal("ReadRegular() followed a final symlink")
	}
	if err := root.RemoveRegular("linked"); err == nil {
		t.Fatal("RemoveRegular() removed a final symlink")
	}
	if err := root.AtomicWrite("linked", ".write-*.tmp", []byte("replacement")); err == nil {
		t.Fatal("AtomicWrite() replaced a final symlink")
	}
	if err := root.AtomicCreateContext(context.Background(), "linked", ".create-*.tmp", []byte("replacement")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("AtomicCreateContext() error = %v, want fs.ErrExist", err)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "outside" {
		t.Fatalf("outside target changed to %q", got)
	}
	if info, err := os.Lstat(linkPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link changed: info=%v err=%v", info, err)
	}
}

func TestAtomicCreateDoesNotOverwriteExistingEntry(t *testing.T) {
	root := openTestRoot(t)
	if err := root.AtomicCreateContext(context.Background(), "record", ".record-*.tmp", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := root.AtomicCreateContext(context.Background(), "record", ".record-*.tmp", []byte("second")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second AtomicCreateContext() error = %v, want fs.ErrExist", err)
	}
	assertRootFile(t, root, "record", "first")
}

func TestConcurrentAtomicCreatePublishesOneWholeValue(t *testing.T) {
	root := openTestRoot(t)
	values := [][]byte{
		bytes.Repeat([]byte("a"), 128<<10),
		bytes.Repeat([]byte("b"), 128<<10),
	}
	start := make(chan struct{})
	errs := make(chan error, len(values))
	for _, value := range values {
		value := value
		go func() {
			<-start
			errs <- root.AtomicCreateContext(context.Background(), "record", ".record-*.tmp", value)
		}()
	}
	close(start)

	var successes, exists int
	for range values {
		err := <-errs
		switch {
		case err == nil:
			successes++
		case errors.Is(err, fs.ErrExist):
			exists++
		default:
			t.Fatalf("AtomicCreateContext() error = %v", err)
		}
	}
	if successes != 1 || exists != 1 {
		t.Fatalf("successes=%d exists=%d, want one of each", successes, exists)
	}
	got, _, err := root.ReadRegular("record", int64(len(values[0])))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, values[0]) && !bytes.Equal(got, values[1]) {
		t.Fatalf("published value is torn or unexpected: length=%d", len(got))
	}
}

func TestAtomicCreateFallbackFailuresRemovePartialDestination(t *testing.T) {
	tests := []struct {
		name      string
		decorate  func(*os.File, error) atomicCreateFile
		syncError bool
	}{
		{
			name: "write",
			decorate: func(file *os.File, failure error) atomicCreateFile {
				return &failingAtomicCreateFile{File: file, writeErr: failure, writePartial: true}
			},
		},
		{
			name: "chmod",
			decorate: func(file *os.File, failure error) atomicCreateFile {
				return &failingAtomicCreateFile{File: file, chmodErr: failure}
			},
		},
		{
			name: "file sync",
			decorate: func(file *os.File, failure error) atomicCreateFile {
				return &failingAtomicCreateFile{File: file, syncErr: failure}
			},
		},
		{
			name: "close",
			decorate: func(file *os.File, failure error) atomicCreateFile {
				return &failingAtomicCreateFile{File: file, closeErr: failure}
			},
		},
		{name: "directory sync", syncError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := openTestRoot(t)
			failure := errors.New("injected " + test.name + " failure")
			unsupportedLink := errors.New("test hard links unsupported")
			var syncCalls int
			ops := atomicCreateOps{
				link: func(_, _ string) error { return unsupportedLink },
				openExclusive: func(name string) (atomicCreateFile, error) {
					file, err := root.handle.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
					if err != nil || test.decorate == nil {
						return file, err
					}
					return test.decorate(file, failure), nil
				},
				remove: root.handle.Remove,
				syncDir: func() error {
					syncCalls++
					if test.syncError && syncCalls == 1 {
						return failure
					}
					return root.Sync()
				},
				canFallback: func(err error) bool { return errors.Is(err, unsupportedLink) },
			}
			err := root.createAtomicContext(context.Background(), "marker", ".marker-*.tmp", []byte("complete marker"), ops)
			if !errors.Is(err, failure) {
				t.Fatalf("atomicCreateContext() error = %v, want injected failure", err)
			}
			if _, err := root.Lstat("marker"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("partial destination remained after failure: %v", err)
			}
			fresh, err := OpenRoot(root.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = fresh.Close() }()
			if _, err := fresh.Lstat("marker"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("partial destination visible after reopen: %v", err)
			}
			entries, err := fresh.ReadDir()
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".tmp") {
					t.Fatalf("temporary entry remained after fallback failure: %q", entry.Name())
				}
			}
		})
	}
}

func TestAtomicCreateHardLinkFailuresRemovePublishedDestination(t *testing.T) {
	tests := []struct {
		name           string
		failSyncCall   int
		failTempRemove bool
	}{
		{name: "publication sync", failSyncCall: 1},
		{name: "temporary removal", failTempRemove: true},
		{name: "cleanup sync", failSyncCall: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := openTestRoot(t)
			failure := errors.New("injected " + test.name + " failure")
			var syncCalls int
			var tempRemoveFailed bool
			ops := atomicCreateOps{
				link:          root.handle.Link,
				openExclusive: func(string) (atomicCreateFile, error) { return nil, errors.New("unexpected fallback") },
				remove: func(name string) error {
					if test.failTempRemove && name != "marker" && !tempRemoveFailed {
						tempRemoveFailed = true
						return failure
					}
					return root.handle.Remove(name)
				},
				syncDir: func() error {
					syncCalls++
					if syncCalls == test.failSyncCall {
						return failure
					}
					return root.Sync()
				},
				canFallback: func(error) bool { return false },
			}
			err := root.createAtomicContext(context.Background(), "marker", ".marker-*.tmp", []byte("complete marker"), ops)
			if !errors.Is(err, failure) {
				t.Fatalf("atomicCreateContext() error = %v, want injected failure", err)
			}
			if _, err := root.Lstat("marker"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("published destination remained after failure: %v", err)
			}
			entries, err := root.ReadDir()
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".tmp") {
					t.Fatalf("temporary entry remained after failure: %q", entry.Name())
				}
			}
		})
	}
}

func TestAtomicCreateDoesNotFallbackForOtherLinkErrors(t *testing.T) {
	root := openTestRoot(t)
	linkFailure := errors.New("link permission denied")
	opened := false
	ops := atomicCreateOps{
		link: func(_, _ string) error { return linkFailure },
		openExclusive: func(string) (atomicCreateFile, error) {
			opened = true
			return nil, errors.New("unexpected fallback")
		},
		remove:      root.handle.Remove,
		syncDir:     root.Sync,
		canFallback: func(error) bool { return false },
	}
	err := root.createAtomicContext(context.Background(), "marker", ".marker-*.tmp", []byte("data"), ops)
	if !errors.Is(err, linkFailure) {
		t.Fatalf("atomicCreateContext() error = %v, want link failure", err)
	}
	if opened {
		t.Fatal("atomicCreateContext() used direct fallback for an unclassified link error")
	}
	if _, err := root.Lstat("marker"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("destination after link failure: %v", err)
	}
}

func TestAtomicCreateFallbackStillHonorsNoOverwrite(t *testing.T) {
	root := openTestRoot(t)
	unsupportedLink := errors.New("test hard links unsupported")
	ops := atomicCreateOps{
		link: func(_, destination string) error {
			file, err := root.handle.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			if _, err := file.Write([]byte("raced value")); err != nil {
				_ = file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
			return unsupportedLink
		},
		openExclusive: func(name string) (atomicCreateFile, error) {
			return root.handle.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		},
		remove:      root.handle.Remove,
		syncDir:     root.Sync,
		canFallback: func(err error) bool { return errors.Is(err, unsupportedLink) },
	}
	err := root.createAtomicContext(context.Background(), "marker", ".marker-*.tmp", []byte("new value"), ops)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("atomicCreateContext() error = %v, want fs.ErrExist", err)
	}
	assertRootFile(t, root, "marker", "raced value")
}

func TestAtomicPublicationAndRemovalAreVisibleAfterReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	root, err := EnsureRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.AtomicWrite("record", ".record-*.tmp", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := root.AtomicWrite("record", ".record-*.tmp", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := root.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	root, err = OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertRootFile(t, root, "record", "second")
	info, err := root.Lstat("record")
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateStoreEntry(t, filepath.Join(dir, "record"), info)
	if err := root.RemoveRegular("record"); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	root, err = OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if _, err := root.Lstat("record"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed record Lstat() error = %v, want fs.ErrNotExist", err)
	}
	entries, err := root.ReadDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file remained after publication: %q", entry.Name())
		}
	}
}

func TestAtomicWritePreparedFailurePreservesExistingDestination(t *testing.T) {
	root := openTestRoot(t)
	if err := root.AtomicWrite("record", ".record-*.tmp", []byte("prior")); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("reservation failed")
	called := false
	err := root.AtomicWritePreparedContext(context.Background(), "record", ".record-*.tmp", []byte("replacement"), func(staged string) error {
		called = true
		if staged == "" {
			t.Fatal("prepared write did not expose its staged basename")
		}
		return failure
	})
	if !errors.Is(err, failure) || !called {
		t.Fatalf("prepared write error=%v called=%t", err, called)
	}
	assertRootFile(t, root, "record", "prior")
	entries, err := root.ReadDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".record-") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("prepared failure retained temp %q", entry.Name())
		}
	}
}

func TestAtomicOperationsHonorPreCanceledContext(t *testing.T) {
	root := openTestRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := root.AtomicWriteContext(ctx, "write", ".write-*.tmp", []byte("data")); !errors.Is(err, context.Canceled) {
		t.Fatalf("AtomicWriteContext() error = %v, want context.Canceled", err)
	}
	if err := root.AtomicCreateContext(ctx, "create", ".create-*.tmp", []byte("data")); !errors.Is(err, context.Canceled) {
		t.Fatalf("AtomicCreateContext() error = %v, want context.Canceled", err)
	}
	entries, err := root.ReadDir()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("canceled atomic operations left entries: %v", entryNames(entries))
	}
}

func TestReadRegularContextEnforcesBoundsAndCancellation(t *testing.T) {
	root := openTestRoot(t)
	content := bytes.Repeat([]byte("x"), (64<<10)+1)
	if err := root.AtomicWrite("record", ".record-*.tmp", content); err != nil {
		t.Fatal(err)
	}

	got, _, err := root.ReadRegularContext(context.Background(), "record", int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("bounded read returned different content")
	}
	if _, _, err := root.ReadRegularContext(context.Background(), "record", int64(len(content)-1)); err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("undersized limit error = %v, want oversized error", err)
	}
	if _, _, err := root.ReadRegularContext(context.Background(), "record", 0); err == nil {
		t.Fatal("zero read limit was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := root.ReadRegularContext(ctx, "record", int64(len(content))); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read error = %v, want context.Canceled", err)
	}
}

func TestRemoveEmptyDirHonorsCancellationAndRejectsUnsafeEntries(t *testing.T) {
	root := openTestRoot(t)
	child, _, err := root.EnsureDirContext(context.Background(), "empty")
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := root.RemoveEmptyDirContext(ctx, "empty"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled RemoveEmptyDirContext() error = %v, want context.Canceled", err)
	}
	if info, err := root.Lstat("empty"); err != nil || !info.IsDir() {
		t.Fatalf("canceled removal changed child: info=%v err=%v", info, err)
	}

	nonempty, _, err := root.EnsureDirContext(context.Background(), "nonempty")
	if err != nil {
		t.Fatal(err)
	}
	if err := nonempty.AtomicWrite("record", ".record-*.tmp", []byte("keep")); err != nil {
		t.Fatal(err)
	}
	if err := nonempty.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveEmptyDir("nonempty"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("non-empty removal error = %v, want fs.ErrExist", err)
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root.Path(), "linked")); err == nil {
		if err := root.RemoveEmptyDir("linked"); err == nil {
			t.Fatal("RemoveEmptyDir() accepted a symlink")
		}
	}
}

func TestOwnershipMarkerKindsMissingAndAdoption(t *testing.T) {
	root := openTestRoot(t)
	if owned, issue := root.Ownership("index"); owned || !strings.Contains(issue, "missing") {
		t.Fatalf("missing marker ownership = %v, %q", owned, issue)
	}
	if err := root.EnsureOwnershipContext(context.Background(), "index", false); err != nil {
		t.Fatal(err)
	}
	if owned, issue := root.Ownership("index"); !owned || issue != "" {
		t.Fatalf("index ownership = %v, %q", owned, issue)
	}
	if owned, issue := root.Ownership("history"); owned || !strings.Contains(issue, "incompatible") {
		t.Fatalf("wrong-kind ownership = %v, %q", owned, issue)
	}

	missingMarker := openTestRoot(t)
	if err := missingMarker.AtomicWrite("foreign", ".foreign-*.tmp", []byte("keep")); err != nil {
		t.Fatal(err)
	}
	if err := missingMarker.EnsureOwnershipContext(context.Background(), "index", false); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("non-adopting ownership error = %v, want non-empty rejection", err)
	}
	if _, err := missingMarker.Lstat(markerName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("marker after rejected adoption: %v", err)
	}
	if err := missingMarker.EnsureOwnershipContext(context.Background(), "index", true); err != nil {
		t.Fatal(err)
	}
	assertRootFile(t, missingMarker, "foreign", "keep")
	if owned, issue := missingMarker.Ownership("index"); !owned || issue != "" {
		t.Fatalf("adopted ownership = %v, %q", owned, issue)
	}

	incompatible := openTestRoot(t)
	if err := incompatible.AtomicWrite(markerName, ".marker-*.tmp", []byte("not-a-marker\n")); err != nil {
		t.Fatal(err)
	}
	if err := incompatible.EnsureOwnershipContext(context.Background(), "index", true); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("incompatible marker adoption error = %v", err)
	}
}

func TestWithLockSerializesConcurrentMissingLockCreation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	start := make(chan struct{})
	errs := make(chan error, workers)
	var active atomic.Int32
	var calls atomic.Int32
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			<-start
			errs <- WithLock(context.Background(), dir, func(_ *Root) error {
				if got := active.Add(1); got != 1 {
					return errors.New("lock callbacks overlapped")
				}
				time.Sleep(time.Millisecond)
				active.Add(-1)
				calls.Add(1)
				return nil
			})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != workers {
		t.Fatalf("callback calls = %d, want %d", got, workers)
	}
	info, err := os.Lstat(filepath.Join(dir, lockName))
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateStoreEntry(t, filepath.Join(dir, lockName), info)
}

func TestWithLockCancellationWhileBusyAndFree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- WithLock(context.Background(), dir, func(_ *Root) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("first lock callback did not start")
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-firstDone
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	called := false
	err := WithLock(ctx, dir, func(_ *Root) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy WithLock() error = %v, want context deadline", err)
	}
	if called {
		t.Fatal("busy canceled WithLock() invoked callback")
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	firstDone <- nil // Balance the cleanup receive.

	canceled, cancelFree := context.WithCancel(context.Background())
	cancelFree()
	called = false
	err = WithLock(canceled, dir, func(_ *Root) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("free canceled WithLock() error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("free canceled WithLock() invoked callback")
	}
}

func TestWithLockCoordinatesAcrossProcesses(t *testing.T) {
	if os.Getenv("DIRSTAT_STORE_LOCK_HELPER") == "1" {
		dir := os.Getenv("DIRSTAT_STORE_LOCK_DIR")
		ready := os.Getenv("DIRSTAT_STORE_LOCK_READY")
		release := os.Getenv("DIRSTAT_STORE_LOCK_RELEASE")
		err := WithLock(context.Background(), dir, func(_ *Root) error {
			if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
				return err
			}
			for {
				if _, err := os.Lstat(release); err == nil {
					return nil
				} else if !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		return
	}

	base := t.TempDir()
	dir := filepath.Join(base, "store")
	ready := filepath.Join(base, "ready")
	release := filepath.Join(base, "release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWithLockCoordinatesAcrossProcesses$")
	cmd.Env = append(os.Environ(),
		"DIRSTAT_STORE_LOCK_HELPER=1",
		"DIRSTAT_STORE_LOCK_DIR="+dir,
		"DIRSTAT_STORE_LOCK_READY="+ready,
		"DIRSTAT_STORE_LOCK_RELEASE="+release,
	)
	var childOutput bytes.Buffer
	cmd.Stdout, cmd.Stderr = &childOutput, &childOutput
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	childDone := make(chan error, 1)
	go func() { childDone <- cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Lstat(ready); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not acquire store lock: %s", childOutput.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := WithLock(ctx, dir, func(_ *Root) error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cross-process lock wait error = %v, want deadline exceeded", err)
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-childDone:
		if err != nil {
			t.Fatalf("lock helper failed: %v\n%s", err, childOutput.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lock helper did not exit")
	}
	if err := WithLock(context.Background(), dir, func(_ *Root) error { return nil }); err != nil {
		t.Fatalf("store lock remained held after helper exit: %v", err)
	}
}

func TestWithLockPreCanceledContextDoesNotCreateStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WithLock(ctx, dir, func(_ *Root) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("WithLock() error = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("pre-canceled WithLock() created store: %v", err)
	}
}

func openTestRoot(t *testing.T) *Root {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func assertRootFile(t *testing.T, root *Root, name, want string) {
	t.Helper()
	got, _, err := root.ReadRegular(name, int64(len(want))+1)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

type failingAtomicCreateFile struct {
	*os.File
	writeErr     error
	chmodErr     error
	syncErr      error
	closeErr     error
	writePartial bool
}

func (f *failingAtomicCreateFile) Write(data []byte) (int, error) {
	if f.writeErr == nil {
		return f.File.Write(data)
	}
	if !f.writePartial || len(data) == 0 {
		return 0, f.writeErr
	}
	written, err := f.File.Write(data[:len(data)/2])
	if err != nil {
		return written, errors.Join(f.writeErr, err)
	}
	return written, f.writeErr
}

func (f *failingAtomicCreateFile) Chmod(mode fs.FileMode) error {
	if f.chmodErr != nil {
		return f.chmodErr
	}
	return f.File.Chmod(mode)
}

func (f *failingAtomicCreateFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.File.Sync()
}

func (f *failingAtomicCreateFile) Close() error {
	err := f.File.Close()
	if f.closeErr != nil {
		return errors.Join(f.closeErr, err)
	}
	return err
}
