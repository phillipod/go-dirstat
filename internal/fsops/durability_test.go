package fsops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
)

func TestDirectoryCopyPublishesCompleteSiblingStage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	t.Cleanup(func() { makeTreeRemovable(root) })
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "nested", "payload"), "complete")
	if err := os.Chmod(filepath.Join(source, "nested"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o550); err != nil {
		t.Fatal(err)
	}

	results, err := Apply(context.Background(), testPlan(root, Operation{
		ID: "copy", Action: ActionCopy, Source: source, Destination: destination,
	}), ApplyOptions{DisableAudit: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].MutationCompleted || results[0].Status != ResultStatusOK {
		t.Fatalf("results = %#v", results)
	}
	assertTestFile(t, filepath.Join(destination, "nested", "payload"), "complete")
	if runtime.GOOS != windowsOS {
		if info, statErr := os.Stat(destination); statErr != nil || info.Mode().Perm() != 0o550 {
			t.Fatalf("destination mode: info=%v error=%v", info, statErr)
		}
	}
	assertNoStagingArtifacts(t, root)
}

func TestDirectoryCopyRestoreModeDoesNotFollowReplacedDestinationSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOS {
		t.Skip("symlink chmod race is Unix-specific")
	}
	root := t.TempDir()
	t.Cleanup(func() { makeTreeRemovable(root) })
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	victim := filepath.Join(root, "victim-file")
	replacedDestination := filepath.Join(root, "published-directory")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "payload"), "complete")
	writeTestFile(t, victim, "sensitive")
	if err := os.Chmod(source, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victim, 0o600); err != nil {
		t.Fatal(err)
	}

	filesystem := defaultMutationFilesystem()
	publish := filesystem.publish
	filesystem.publish = func(oldPath, newPath string) error {
		if err := publish(oldPath, newPath); err != nil {
			return err
		}
		if newPath != destination {
			return nil
		}
		if err := os.Rename(newPath, replacedDestination); err != nil {
			return err
		}
		return os.Symlink(victim, newPath)
	}

	if err := copyDirectoryStaged(context.Background(), source, destination, filesystem); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destination was not replaced with symlink: %s", info.Mode())
	}
	if victimInfo, err := os.Stat(victim); err != nil || victimInfo.Mode().Perm() != 0o600 {
		t.Fatalf("victim mode changed through replacement symlink: info=%v error=%v", victimInfo, err)
	}
	if publishedInfo, err := os.Stat(replacedDestination); err != nil || publishedInfo.Mode().Perm() != 0o777 {
		t.Fatalf("published directory mode not restored through descriptor: info=%v error=%v", publishedInfo, err)
	}
}

func TestDirectoryCopyFailureAndCancellationNeverExposeDestination(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		skipOnWindows bool
		configure     func(*mutationFilesystem, context.CancelFunc)
		want          string
	}{
		{
			name: "child copy failure",
			configure: func(filesystem *mutationFilesystem, _ context.CancelFunc) {
				filesystem.copy = func(io.Writer, io.Reader) (int64, error) {
					return 0, errors.New("injected child copy failure")
				}
			},
			want: "injected child copy failure",
		},
		{
			name: "cancel between children",
			configure: func(filesystem *mutationFilesystem, cancel context.CancelFunc) {
				copyFile := filesystem.copy
				filesystem.copy = func(writer io.Writer, reader io.Reader) (int64, error) {
					count, err := copyFile(writer, reader)
					cancel()
					return count, err
				}
			},
			want: context.Canceled.Error(),
		},
		{
			name: "publish rename failure",
			configure: func(filesystem *mutationFilesystem, _ context.CancelFunc) {
				publish := filesystem.publish
				filesystem.publish = func(oldPath, newPath string) error {
					if strings.HasPrefix(filepath.Base(oldPath), ".dirstat-copy-") {
						return errors.New("injected publish failure")
					}
					return publish(oldPath, newPath)
				}
			},
			want: "injected publish failure",
		},
		{
			name:          "staging directory sync failure",
			skipOnWindows: true,
			configure: func(filesystem *mutationFilesystem, _ context.CancelFunc) {
				syncFile := filesystem.sync
				filesystem.sync = func(file *os.File) error {
					info, err := file.Stat()
					if err != nil {
						return err
					}
					if info.IsDir() {
						return errors.New("injected directory sync failure")
					}
					return syncFile(file)
				}
			},
			want: "injected directory sync failure",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if runtime.GOOS == windowsOS && test.skipOnWindows {
				t.Skip("Windows publication does not expose directory fsync")
			}
			root := t.TempDir()
			source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(source, "a"), "A")
			writeTestFile(t, filepath.Join(source, "b"), "B")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			filesystem := defaultMutationFilesystem()
			test.configure(&filesystem, cancel)
			results, err := Apply(ctx, testPlan(root, Operation{
				ID: "copy", Action: ActionCopy, Source: source, Destination: destination,
			}), ApplyOptions{DisableAudit: true, filesystem: &filesystem})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("results=%#v error=%v", results, err)
			}
			if len(results) != 1 || results[0].Status != ResultStatusError || results[0].MayHaveMutated {
				t.Fatalf("results = %#v", results)
			}
			if _, statErr := os.Lstat(destination); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("incomplete destination is visible: %v", statErr)
			}
			assertNoStagingArtifacts(t, root)
		})
	}
}

func TestDirectoryCopyCleanupFailureIsExplicitlyPartial(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "payload"), "payload")
	filesystem := defaultMutationFilesystem()
	filesystem.copy = func(io.Writer, io.Reader) (int64, error) {
		return 0, errors.New("injected copy failure")
	}
	removeAll := filesystem.removeAll
	filesystem.removeAll = func(path string) error {
		if strings.HasPrefix(filepath.Base(path), ".dirstat-copy-") {
			return errors.New("injected staging cleanup failure")
		}
		return removeAll(path)
	}
	results, err := Apply(context.Background(), testPlan(root, Operation{
		ID: "copy", Action: ActionCopy, Source: source, Destination: destination,
	}), ApplyOptions{DisableAudit: true, filesystem: &filesystem})
	if err == nil || !strings.Contains(err.Error(), "staging cleanup failure") {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	if len(results) != 1 || results[0].Status != ResultStatusPartial || !results[0].MayHaveMutated {
		t.Fatalf("results = %#v", results)
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("destination is visible: %v", statErr)
	}
	matches, globErr := filepath.Glob(filepath.Join(root, ".dirstat-copy-*"))
	if globErr != nil || len(matches) != 1 {
		t.Fatalf("staging residue=%v error=%v", matches, globErr)
	}
}

func TestDirectoryCopyOverwriteFailureRestoresReviewedDestination(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "new"), "new")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(destination, "old"), "old")
	expectedDestination, err := fsinfo.CapturePath(destination)
	if err != nil {
		t.Fatal(err)
	}
	filesystem := defaultMutationFilesystem()
	filesystem.copy = func(io.Writer, io.Reader) (int64, error) {
		return 0, errors.New("injected overwrite copy failure")
	}
	results, err := Apply(context.Background(), testPlan(root, Operation{
		ID: "copy", Action: ActionCopy, Source: source, Destination: destination,
		ExpectedDestination: &expectedDestination,
	}), ApplyOptions{Conflict: ConflictOverwrite, DisableAudit: true, filesystem: &filesystem})
	if err == nil || !strings.Contains(err.Error(), "injected overwrite copy failure") {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	assertTestFile(t, filepath.Join(destination, "old"), "old")
	if _, statErr := os.Stat(filepath.Join(destination, "new")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("replacement leaked: %v", statErr)
	}
	assertNoStagingArtifacts(t, root)
}

func TestDirectoryCopyDoesNotReplaceDestinationCreatedDuringStaging(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "payload"), "reviewed")
	filesystem := defaultMutationFilesystem()
	publish := filesystem.publish
	filesystem.publish = func(oldPath, newPath string) error {
		if err := os.Mkdir(newPath, 0o700); err != nil {
			return err
		}
		writeTestFile(t, filepath.Join(newPath, "concurrent"), "concurrent")
		return publish(oldPath, newPath)
	}
	results, err := Apply(context.Background(), testPlan(root, Operation{
		ID: "copy", Action: ActionCopy, Source: source, Destination: destination,
	}), ApplyOptions{DisableAudit: true, filesystem: &filesystem})
	if err == nil || len(results) != 1 || results[0].Status != ResultStatusError {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	assertTestFile(t, filepath.Join(destination, "concurrent"), "concurrent")
	if _, statErr := os.Stat(filepath.Join(destination, "payload")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("staged payload replaced concurrent destination: %v", statErr)
	}
	assertNoStagingArtifacts(t, root)
}

func TestCopyFileInjectedIOFailuresAreCleaned(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		configure func(*mutationFilesystem)
	}{
		{
			name: "copy",
			configure: func(filesystem *mutationFilesystem) {
				filesystem.copy = func(writer io.Writer, _ io.Reader) (int64, error) {
					count, _ := writer.Write([]byte("partial"))
					return int64(count), errors.New("copy failed")
				}
			},
		},
		{
			name: "short successful copy",
			configure: func(filesystem *mutationFilesystem) {
				filesystem.copy = func(io.Writer, io.Reader) (int64, error) { return 0, nil }
			},
		},
		{
			name: "sync",
			configure: func(filesystem *mutationFilesystem) {
				filesystem.sync = func(*os.File) error { return errors.New("sync failed") }
			},
		},
		{
			name: "close",
			configure: func(filesystem *mutationFilesystem) {
				filesystem.close = func(file *os.File) error {
					if err := file.Close(); err != nil {
						return err
					}
					return errors.New("close failed")
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
			writeTestFile(t, source, "payload")
			filesystem := defaultMutationFilesystem()
			test.configure(&filesystem)
			results, err := Apply(context.Background(), testPlan(root, Operation{
				ID: "copy", Action: ActionCopy, Source: source, Destination: destination,
			}), ApplyOptions{DisableAudit: true, filesystem: &filesystem})
			if err == nil || len(results) != 1 || results[0].Status != ResultStatusError {
				t.Fatalf("results=%#v error=%v", results, err)
			}
			if _, statErr := os.Lstat(destination); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("incomplete copy remains: %v", statErr)
			}
		})
	}
}

func TestRecursiveDeleteCancellationReportsPartialOutcome(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "a"), "A")
	writeTestFile(t, filepath.Join(source, "b"), "B")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	filesystem := defaultMutationFilesystem()
	remove := filesystem.remove
	removed := 0
	filesystem.remove = func(path string) error {
		if err := remove(path); err != nil {
			return err
		}
		removed++
		if removed == 1 {
			cancel()
		}
		return nil
	}
	results, err := Apply(ctx, testPlan(root, Operation{
		ID: "delete", Action: ActionDelete, Source: source, Recursive: true,
	}), ApplyOptions{DisableAudit: true, filesystem: &filesystem})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	if len(results) != 1 || results[0].Status != ResultStatusPartial || !results[0].MayHaveMutated || results[0].MutationCompleted {
		t.Fatalf("results = %#v", results)
	}
	if _, statErr := os.Stat(source); statErr != nil {
		t.Fatalf("source root should remain after cancellation: %v", statErr)
	}
}

func TestRecursiveDeleteFailureBeforeFirstRemovalIsNonPartial(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "payload"), "payload")
	filesystem := defaultMutationFilesystem()
	filesystem.remove = func(string) error { return errors.New("remove denied") }
	results, err := Apply(context.Background(), testPlan(root, Operation{
		ID: "delete", Action: ActionDelete, Source: source, Recursive: true,
	}), ApplyOptions{DisableAudit: true, filesystem: &filesystem})
	if err == nil || !strings.Contains(err.Error(), "after removing 0") {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	if len(results) != 1 || results[0].Status != ResultStatusError || results[0].MayHaveMutated {
		t.Fatalf("results = %#v", results)
	}
	assertTestFile(t, filepath.Join(source, "payload"), "payload")
}

func TestRecursiveDeletePreservesConcurrentNewEntry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "captured"), "captured")
	filesystem := defaultMutationFilesystem()
	remove := filesystem.remove
	first := true
	filesystem.remove = func(path string) error {
		if first {
			first = false
			writeTestFile(t, filepath.Join(source, "late"), "late")
		}
		return remove(path)
	}
	results, err := Apply(context.Background(), testPlan(root, Operation{
		ID: "delete", Action: ActionDelete, Source: source, Recursive: true,
	}), ApplyOptions{DisableAudit: true, filesystem: &filesystem})
	if err == nil || len(results) != 1 || results[0].Status != ResultStatusPartial {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	assertTestFile(t, filepath.Join(source, "late"), "late")
}

func TestRecursiveDeleteDoesNotFollowCapturedSymlink(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(outside, "keep")
	writeTestFile(t, external, "keep")
	if err := os.Symlink(outside, filepath.Join(source, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Apply(context.Background(), testPlan(root, Operation{
		ID: "delete", Action: ActionDelete, Source: source, Recursive: true,
	}), ApplyOptions{DisableAudit: true}); err != nil {
		t.Fatal(err)
	}
	assertTestFile(t, external, "keep")
	if _, err := os.Stat(source); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("source remains: %v", err)
	}
}

type faultAuditLog struct {
	bytes.Buffer
	writeErr   error
	writeErrAt int
	syncErr    error
	syncErrAt  int
	closeErr   error
	writes     int
	syncs      int
	closes     int
	onSync     func(int)
}

func (log *faultAuditLog) Write(data []byte) (int, error) {
	log.writes++
	if log.writeErr != nil && (log.writeErrAt == 0 || log.writes == log.writeErrAt) {
		return 0, log.writeErr
	}
	return log.Buffer.Write(data)
}

func (log *faultAuditLog) Sync() error {
	log.syncs++
	if log.onSync != nil {
		log.onSync(log.syncs)
	}
	if log.syncErr != nil && (log.syncErrAt == 0 || log.syncs == log.syncErrAt) {
		return log.syncErr
	}
	return nil
}

func TestCancellationBetweenOperationsKeepsFirstDurableOutcome(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &faultAuditLog{onSync: func(call int) {
		if call == 2 {
			cancel()
		}
	}}
	results, err := Apply(ctx, testPlan(root,
		Operation{ID: "first", Action: ActionTouch, Source: first},
		Operation{ID: "second", Action: ActionTouch, Source: second},
	), ApplyOptions{auditFactory: func(string) (auditLog, error) { return log, nil }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	if len(results) != 1 || !results[0].MutationCompleted || results[0].AuditStatus != AuditStatusDurable {
		t.Fatalf("results = %#v", results)
	}
	if _, statErr := os.Stat(first); statErr != nil {
		t.Fatalf("first operation missing: %v", statErr)
	}
	if _, statErr := os.Stat(second); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("second operation ran: %v", statErr)
	}
}

func (log *faultAuditLog) Close() error {
	log.closes++
	return log.closeErr
}

func TestAuditOutcomeIsSyncedBeforeSuccess(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	created := filepath.Join(root, "created")
	log := &faultAuditLog{}
	results, err := Apply(context.Background(), testPlan(root, Operation{
		ID: "touch", Action: ActionTouch, Source: created,
	}), ApplyOptions{auditFactory: func(string) (auditLog, error) { return log, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].AuditIntentStatus != AuditStatusDurable ||
		results[0].AuditStatus != AuditStatusDurable || !results[0].MutationCompleted {
		t.Fatalf("results = %#v", results)
	}
	if log.syncs != 2 || log.closes != 1 {
		t.Fatalf("syncs=%d closes=%d", log.syncs, log.closes)
	}
	audited, readErr := ReadResults(bytes.NewReader(log.Bytes()))
	if readErr != nil || len(audited) != 2 || audited[0].AuditPhase != AuditPhaseIntent ||
		audited[0].Status != ResultStatusIntent || audited[1].AuditPhase != AuditPhaseOutcome ||
		audited[1].AuditStatus != AuditStatusWritten || !audited[1].MutationCompleted {
		t.Fatalf("audit=%#v error=%v", audited, readErr)
	}
}

func TestCompletedMutationReportsAuditWriteSyncAndCloseFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		configure func(*faultAuditLog)
		want      string
	}{
		{name: "write", configure: func(log *faultAuditLog) { log.writeErr, log.writeErrAt = errors.New("disk full"), 2 }, want: "write audit outcome"},
		{name: "sync", configure: func(log *faultAuditLog) { log.syncErr, log.syncErrAt = errors.New("sync failed"), 2 }, want: "sync audit outcome"},
		{name: "close", configure: func(log *faultAuditLog) { log.closeErr = errors.New("close failed") }, want: "close audit log"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			created := filepath.Join(root, "created")
			log := &faultAuditLog{}
			test.configure(log)
			results, err := Apply(context.Background(), testPlan(root, Operation{
				ID: "touch", Action: ActionTouch, Source: created,
			}), ApplyOptions{auditFactory: func(string) (auditLog, error) { return log, nil }})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("results=%#v error=%v", results, err)
			}
			if len(results) != 1 || results[0].Status != ResultStatusPartial ||
				!results[0].MayHaveMutated || !results[0].MutationCompleted || results[0].AuditStatus != AuditStatusFailed {
				t.Fatalf("results = %#v", results)
			}
			if _, statErr := os.Stat(created); statErr != nil {
				t.Fatalf("completed mutation was not retained: %v", statErr)
			}
			if log.closes != 1 {
				t.Fatalf("close calls = %d", log.closes)
			}
		})
	}
}

func TestAuditIntentFailurePreventsFilesystemMutation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		configure func(*faultAuditLog)
		want      string
	}{
		{name: "write", configure: func(log *faultAuditLog) { log.writeErr, log.writeErrAt = errors.New("disk full"), 1 }, want: "write audit intent"},
		{name: "sync", configure: func(log *faultAuditLog) { log.syncErr, log.syncErrAt = errors.New("sync failed"), 1 }, want: "sync audit intent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			created := filepath.Join(root, "created")
			log := &faultAuditLog{}
			test.configure(log)
			results, err := Apply(context.Background(), testPlan(root, Operation{
				ID: "touch", Action: ActionTouch, Source: created,
			}), ApplyOptions{auditFactory: func(string) (auditLog, error) { return log, nil }})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("results=%#v error=%v", results, err)
			}
			if len(results) != 1 || results[0].Status != ResultStatusError ||
				results[0].MutationCompleted || results[0].MayHaveMutated ||
				results[0].AuditIntentStatus != AuditStatusFailed || results[0].AuditStatus != AuditStatusNotAttempted {
				t.Fatalf("results = %#v", results)
			}
			if _, statErr := os.Stat(created); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("mutation ran after intent failure: %v", statErr)
			}
		})
	}
}

func TestAuditOpenFailurePrecedesFilesystemMutation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	created := filepath.Join(root, "created")
	results, err := Apply(context.Background(), testPlan(root, Operation{
		ID: "touch", Action: ActionTouch, Source: created,
	}), ApplyOptions{auditFactory: func(string) (auditLog, error) {
		return nil, errors.New("audit unavailable")
	}})
	if err == nil || !strings.Contains(err.Error(), "open audit log") || len(results) != 0 {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	if _, statErr := os.Stat(created); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("mutation ran without audit: %v", statErr)
	}
}

func TestAuditInsideMutatedTreeIsRejectedBeforeOpen(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "payload"), "payload")
	auditPath := filepath.Join(source, "state", "audit.jsonl")
	results, err := Apply(context.Background(), testPlan(root, Operation{
		ID: "delete", Action: ActionDelete, Source: source, Recursive: true,
	}), ApplyOptions{AuditPath: auditPath})
	if err == nil || !strings.Contains(err.Error(), "source contains the audit log") || len(results) != 0 {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	assertTestFile(t, filepath.Join(source, "payload"), "payload")
	if _, statErr := os.Stat(auditPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("audit path was created: %v", statErr)
	}
}

func TestCallerOwnedUnsyncedAuditReportsWritten(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var audit bytes.Buffer
	results, err := Apply(context.Background(), testPlan(root, Operation{
		ID: "touch", Action: ActionTouch, Source: filepath.Join(root, "created"),
	}), ApplyOptions{Audit: &audit})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].AuditIntentStatus != AuditStatusWritten || results[0].AuditStatus != AuditStatusWritten {
		t.Fatalf("results = %#v", results)
	}
}

func TestValidateOperationForWindowsRejectsPOSIXSemantics(t *testing.T) {
	t.Parallel()
	mode := uint32(0o700)
	tests := []struct {
		name    string
		op      Operation
		wantErr bool
	}{
		{name: "chmod", op: Operation{Action: ActionChmod, Mode: &mode}, wantErr: true},
		{name: "chown", op: Operation{Action: ActionChown}, wantErr: true},
		{name: "mkdir explicit mode", op: Operation{Action: ActionMkdir, Mode: &mode}, wantErr: true},
		{name: "touch explicit mode", op: Operation{Action: ActionTouch, Mode: &mode}, wantErr: true},
		{name: "mkdir default mode", op: Operation{Action: ActionMkdir}},
		{name: "copy", op: Operation{Action: ActionCopy}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateOperationForOS(windowsOS, test.op)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func assertNoStagingArtifacts(t *testing.T, root string) {
	t.Helper()
	for _, pattern := range []string{".dirstat-copy-*", ".dirstat-backup-*"} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil || len(matches) != 0 {
			t.Fatalf("staging artifacts for %q: %v (error %v)", pattern, matches, err)
		}
	}
}
