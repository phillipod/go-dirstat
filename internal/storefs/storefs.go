// Package storefs provides the capability-style filesystem boundary shared by
// dirstat's private cache and durable-state stores.
package storefs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	lockName   = ".dirstat.lock"
	markerName = ".dirstat-store"
)

// Root is a stable directory-handle capability. All entry operations are
// relative to this handle, so renaming or replacing its original pathname
// cannot redirect an in-flight writer outside the opened store.
type Root struct {
	path   string
	handle *os.Root
	info   fs.FileInfo
}

// ResolveStoreDir resolves symlinked ancestors once while rejecting a symlink
// at the owned store boundary itself. This permits conventional symlinked home
// directories while preserving a no-follow store root.
func ResolveStoreDir(dir string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return "", err
	}
	if info, statErr := os.Lstat(abs); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("store directory %q is a symlink", abs)
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", err
		}
		return filepath.Clean(resolved), nil
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return "", statErr
	}
	current := abs
	var missing []string
	for {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("store ancestor %q is not a directory", current)
			}
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("resolve store directory %q: no existing ancestor", abs)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// OpenRoot opens an existing, non-symlinked store and verifies the pathname and
// handle identify the same directory before returning the capability.
func OpenRoot(dir string) (*Root, error) {
	before, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("store directory %q is not a real directory", dir)
	}
	handle, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	opened, err := handle.Stat(".")
	if err != nil {
		_ = handle.Close()
		return nil, err
	}
	after, err := os.Lstat(dir)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = handle.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("store directory %q changed while opening", dir)
	}
	return &Root{path: dir, handle: handle, info: opened}, nil
}

// EnsureRoot securely creates any missing store components below a stable
// existing ancestor, rejecting links component by component.
func EnsureRoot(dir string) (*Root, error) {
	return EnsureRootContext(context.Background(), dir)
}

// EnsureRootContext is EnsureRoot with cancellation checks before every
// directory mutation.
func EnsureRootContext(ctx context.Context, dir string) (*Root, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := OpenRoot(dir)
	if err == nil {
		return root, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	current := dir
	var missing []string
	for {
		if _, statErr := os.Lstat(current); statErr == nil {
			break
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return nil, statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil, fmt.Errorf("create store %q: no existing ancestor", dir)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	ancestor, err := OpenRoot(current)
	if err != nil {
		return nil, err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			_ = ancestor.Close()
			return nil, err
		}
		name := missing[i]
		created := false
		info, statErr := ancestor.handle.Lstat(name)
		if errors.Is(statErr, fs.ErrNotExist) {
			mkdirErr := ancestor.handle.Mkdir(name, 0o700)
			if mkdirErr == nil {
				created = true
			} else if !errors.Is(mkdirErr, fs.ErrExist) {
				_ = ancestor.Close()
				return nil, mkdirErr
			}
			if err := ancestor.Sync(); err != nil {
				_ = ancestor.Close()
				return nil, err
			}
			info, statErr = ancestor.handle.Lstat(name)
		}
		if statErr != nil {
			_ = ancestor.Close()
			return nil, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			_ = ancestor.Close()
			return nil, fmt.Errorf("store component %q is not a real directory", filepath.Join(current, name))
		}
		child, err := ancestor.OpenDir(name)
		if err != nil {
			_ = ancestor.Close()
			return nil, err
		}
		if created {
			if err := makePrivateStore(child.path, func(mode fs.FileMode) error { return child.handle.Chmod(".", mode) }); err != nil {
				_ = child.Close()
				_ = ancestor.Close()
				return nil, err
			}
			if err := child.refreshInfo(); err != nil || !privateStore(child.path, child.info) {
				_ = child.Close()
				_ = ancestor.Close()
				if err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("store component %q could not be made private", child.path)
			}
		}
		_ = ancestor.Close()
		ancestor = child
		current = filepath.Join(current, name)
	}
	return ancestor, nil
}

// Close releases the directory capability.
func (r *Root) Close() error { return r.handle.Close() }

// Path returns the canonical store path used to open this capability.
func (r *Root) Path() string { return r.path }

// Writable reports the owner-write capability advertised by the opened store.
func (r *Root) Writable() bool {
	if err := r.refreshInfo(); err != nil {
		return false
	}
	return r.info.Mode().Perm()&0o200 != 0
}

// CanAdopt verifies the non-mutating ownership prerequisite used by explicit
// migration. It mirrors EnsureOwnershipContext(adopt=true) without changing
// permissions or publishing a marker, so dry runs faithfully predict apply.
func (r *Root) CanAdopt() error {
	if err := r.refreshInfo(); err != nil {
		return err
	}
	if !ownedByCurrentUser(r.path, r.info) {
		return fmt.Errorf("store directory %q is owned by another user", r.path)
	}
	return nil
}

// CanInitialize verifies the stricter prerequisite for creating an ownership
// marker without migration authority: the directory must already be private
// and owned by the current user.
func (r *Root) CanInitialize() error {
	if err := r.refreshInfo(); err != nil {
		return err
	}
	if !privateStore(r.path, r.info) {
		return fmt.Errorf("store directory %q is not private or is owned by another user", r.path)
	}
	return nil
}

// Ownership reports whether root has the exact marker for kind. Missing or
// corrupt markers are non-owned state, not implicit authorization to delete.
func (r *Root) Ownership(kind string) (bool, string) {
	if err := r.refreshInfo(); err != nil {
		return false, err.Error()
	}
	if !privateStore(r.path, r.info) {
		return false, "store directory is not private or is owned by another user"
	}
	return r.markerOwnership(kind)
}

func (r *Root) markerOwnership(kind string) (bool, string) {
	data, _, err := r.ReadRegular(markerName, 256)
	if errors.Is(err, fs.ErrNotExist) {
		return false, "store marker is missing"
	}
	if err != nil {
		return false, err.Error()
	}
	want := "dirstat-store-v1:" + kind + "\n"
	if string(data) != want {
		return false, "store marker is incompatible"
	}
	return true, ""
}

// EnsureOwnershipContext creates the ownership marker only for an empty
// private store, unless adopt is an explicit migration authorization.
func (r *Root) EnsureOwnershipContext(ctx context.Context, kind string, adopt bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	markerOwned, markerIssue := r.markerOwnership(kind)
	if markerOwned {
		if privateStore(r.path, r.info) {
			return nil
		}
		if !adopt || !ownedByCurrentUser(r.path, r.info) {
			return fmt.Errorf("store directory %q is not private", r.path)
		}
		if err := makePrivateStore(r.path, func(mode fs.FileMode) error { return r.handle.Chmod(".", mode) }); err != nil {
			return err
		}
		if err := r.refreshInfo(); err != nil {
			return err
		}
		if !privateStore(r.path, r.info) {
			return fmt.Errorf("store directory %q could not be made private", r.path)
		}
		return nil
	}
	if markerIssue != "store marker is missing" {
		return errors.New(markerIssue)
	}
	if !adopt && !privateStore(r.path, r.info) {
		return fmt.Errorf("store directory %q is not private; chmod 0700 or use state migrate --yes", r.path)
	}
	if adopt && !ownedByCurrentUser(r.path, r.info) {
		return fmt.Errorf("store directory %q is owned by another user", r.path)
	}
	if !adopt {
		entries, err := r.ReadDir()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() != lockName {
				return fmt.Errorf("store directory %q is unowned and non-empty; use state migrate --dry-run then --yes", r.path)
			}
		}
	} else {
		if err := makePrivateStore(r.path, func(mode fs.FileMode) error { return r.handle.Chmod(".", mode) }); err != nil {
			return err
		}
		if err := r.refreshInfo(); err != nil {
			return err
		}
		if !privateStore(r.path, r.info) {
			return fmt.Errorf("store directory %q could not be made private", r.path)
		}
	}
	err := r.AtomicCreateContext(ctx, markerName, ".marker-*.tmp", []byte("dirstat-store-v1:"+kind+"\n"))
	if errors.Is(err, fs.ErrExist) {
		if owned, issue := r.Ownership(kind); owned {
			return nil
		} else {
			return errors.New(issue)
		}
	}
	return err
}

func (r *Root) refreshInfo() error {
	info, err := r.handle.Stat(".")
	if err != nil {
		return err
	}
	r.info = info
	return nil
}

// StillCurrent verifies the user-visible path still names this directory.
func (r *Root) StillCurrent() error {
	current, err := os.Lstat(r.path)
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(r.info, current) {
		return fmt.Errorf("store directory %q was replaced during operation", r.path)
	}
	return nil
}

// OpenDir opens one real child directory through the current capability.
func (r *Root) OpenDir(name string) (*Root, error) {
	if !validEntryName(name) {
		return nil, fmt.Errorf("invalid store directory entry %q", name)
	}
	before, err := r.handle.Lstat(name)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("store directory entry %q is not a real directory", name)
	}
	handle, err := r.handle.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := handle.Stat(".")
	if err != nil {
		_ = handle.Close()
		return nil, err
	}
	after, err := r.handle.Lstat(name)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = handle.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("store directory entry %q changed while opening", name)
	}
	return &Root{path: filepath.Join(r.path, name), handle: handle, info: opened}, nil
}

// EnsureDirContext opens or creates one immediate real child directory.
func (r *Root) EnsureDirContext(ctx context.Context, name string) (*Root, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	child, err := r.OpenDir(name)
	if err == nil {
		return child, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := r.handle.Mkdir(name, 0o700); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, false, err
		}
		child, openErr := r.OpenDir(name)
		return child, false, openErr
	}
	if err := r.Sync(); err != nil {
		return nil, false, err
	}
	child, err = r.OpenDir(name)
	return child, true, err
}

// ReadDir returns a stable snapshot of immediate directory entries.
func (r *Root) ReadDir() ([]os.DirEntry, error) {
	dir, err := r.handle.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	return dir.ReadDir(-1)
}

// Lstat returns entry metadata without following a final link.
func (r *Root) Lstat(name string) (fs.FileInfo, error) {
	if !validEntryName(name) {
		return nil, fmt.Errorf("invalid store entry %q", name)
	}
	return r.handle.Lstat(name)
}

// ReadRegular reads one immediate entry after no-follow and identity checks.
func (r *Root) ReadRegular(name string, maxBytes int64) ([]byte, fs.FileInfo, error) {
	return r.ReadRegularContext(context.Background(), name, maxBytes)
}

// ReadRegularContext is ReadRegular with cancellation checks between bounded
// read chunks and a final identity check after EOF.
func (r *Root) ReadRegularContext(ctx context.Context, name string, maxBytes int64) ([]byte, fs.FileInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if maxBytes <= 0 {
		return nil, nil, errors.New("store read limit must be greater than zero")
	}
	before, err := r.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("store entry %q is not a regular file", name)
	}
	if before.Size() < 0 || before.Size() > maxBytes {
		return nil, nil, fmt.Errorf("store entry %q is oversized (%d > %d bytes)", name, before.Size(), maxBytes)
	}
	file, err := r.handle.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	after, err := r.Lstat(name)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("store entry %q changed while opening", name)
	}
	if opened.Size() < 0 || opened.Size() > maxBytes {
		return nil, nil, fmt.Errorf("store entry %q is oversized (%d > %d bytes)", name, opened.Size(), maxBytes)
	}
	var data bytes.Buffer
	if opened.Size() <= int64(^uint(0)>>1) {
		data.Grow(int(opened.Size()))
	}
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			if int64(n) > maxBytes-total {
				return nil, nil, fmt.Errorf("store entry %q grew beyond %d bytes while reading", name, maxBytes)
			}
			total += int64(n)
			_, _ = data.Write(buffer[:n])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, nil, readErr
		}
	}
	final, err := r.Lstat(name)
	if err != nil || final.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, final) || final.Size() != total {
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("store entry %q changed while reading", name)
	}
	return data.Bytes(), opened, nil
}

// RemoveRegular removes one immediate regular entry without following links.
func (r *Root) RemoveRegular(name string) error {
	return r.RemoveRegularContext(context.Background(), name)
}

// RemoveRegularContext is RemoveRegular with a final cancellation gate before
// the unlink becomes visible.
func (r *Root) RemoveRegularContext(ctx context.Context, name string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := r.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return fmt.Errorf("store entry %q is not a regular file", name)
	}
	if err := r.StillCurrent(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	after, err := r.Lstat(name)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) {
		if err != nil {
			return err
		}
		return fmt.Errorf("store entry %q changed before removal", name)
	}
	if err := r.handle.Remove(name); err != nil {
		return err
	}
	return r.Sync()
}

// RemoveEmptyDir removes one empty real child directory.
func (r *Root) RemoveEmptyDir(name string) error {
	return r.RemoveEmptyDirContext(context.Background(), name)
}

// RemoveEmptyDirContext removes an empty child only if its directory identity
// is unchanged immediately before removal.
func (r *Root) RemoveEmptyDirContext(ctx context.Context, name string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	child, err := r.OpenDir(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := child.ReadDir()
	closeErr := child.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(entries) != 0 {
		return fs.ErrExist
	}
	current, err := r.Lstat(name)
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(child.info, current) {
		return fmt.Errorf("store directory entry %q changed before removal", name)
	}
	if err := r.StillCurrent(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.handle.Remove(name); err != nil {
		return err
	}
	return r.Sync()
}

// AtomicWrite publishes one immediate private regular file by rename.
func (r *Root) AtomicWrite(destination, prefix string, data []byte) error {
	return r.AtomicWriteContext(context.Background(), destination, prefix, data)
}

// AtomicWriteContext is AtomicWrite with cancellation gates before temporary
// creation and again immediately before publication.
func (r *Root) AtomicWriteContext(ctx context.Context, destination, prefix string, data []byte) error {
	return r.AtomicWritePreparedContext(ctx, destination, prefix, data, nil)
}

// AtomicWritePreparedContext writes and fsyncs a private temporary file, then
// invokes beforePublish immediately before the atomic rename. Callers use the
// hook to reserve retention/quota only after staging has succeeded. The staged
// basename is provided so inventory code can exclude the in-flight temp.
func (r *Root) AtomicWritePreparedContext(
	ctx context.Context,
	destination string,
	prefix string,
	data []byte,
	beforePublish func(staged string) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validEntryName(destination) {
		return fmt.Errorf("invalid store destination %q", destination)
	}
	if info, err := r.Lstat(destination); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("store destination %q is not a regular file", destination)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	name, file, err := r.createTemp(prefix)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		_ = r.handle.Remove(name)
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.StillCurrent(); err != nil {
		return err
	}
	if beforePublish != nil {
		if err := beforePublish(name); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.StillCurrent(); err != nil {
			return err
		}
	}
	if err := replaceStoreEntry(r.handle, r.path, name, destination); err != nil {
		return fmt.Errorf("publish store entry: %w", err)
	}
	return r.Sync()
}

// AtomicCreateContext durably publishes destination only if it does not exist.
// A hard-link publication prevents a raced marker from being overwritten.
func (r *Root) AtomicCreateContext(ctx context.Context, destination, prefix string, data []byte) error {
	return r.createAtomicContext(ctx, destination, prefix, data, atomicCreateOps{
		link: func(oldName, newName string) error {
			return publishStoreEntryNoReplace(r.handle, r.path, oldName, newName)
		},
		openExclusive: func(name string) (atomicCreateFile, error) {
			return r.handle.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		},
		remove:      r.handle.Remove,
		syncDir:     r.Sync,
		canFallback: canFallbackAtomicCreateLink,
	})
}

type atomicCreateFile interface {
	io.Writer
	Chmod(fs.FileMode) error
	Sync() error
	Close() error
}

type atomicCreateOps struct {
	link          func(string, string) error
	openExclusive func(string) (atomicCreateFile, error)
	remove        func(string) error
	syncDir       func() error
	canFallback   func(error) bool
}

func (r *Root) createAtomicContext(
	ctx context.Context,
	destination string,
	prefix string,
	data []byte,
	ops atomicCreateOps,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validEntryName(destination) {
		return fmt.Errorf("invalid store destination %q", destination)
	}
	if _, err := r.Lstat(destination); err == nil {
		return fs.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	name, file, err := r.createTemp(prefix)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		_ = r.handle.Remove(name)
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.StillCurrent(); err != nil {
		return err
	}
	if err := ops.link(name, destination); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return err
		}
		if !ops.canFallback(err) {
			return fmt.Errorf("publish store entry without replacement: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.StillCurrent(); err != nil {
			return err
		}
		direct, createErr := ops.openExclusive(destination)
		if createErr != nil {
			return createErr
		}
		closed := false
		cleanup := func(cause error) error {
			failures := []error{cause}
			if !closed {
				if closeErr := direct.Close(); closeErr != nil {
					failures = append(failures, fmt.Errorf("close failed store entry: %w", closeErr))
				}
				closed = true
			}
			if removeErr := ops.remove(destination); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				failures = append(failures, fmt.Errorf("remove failed store entry: %w", removeErr))
			}
			if syncErr := ops.syncDir(); syncErr != nil {
				failures = append(failures, fmt.Errorf("sync failed store cleanup: %w", syncErr))
			}
			return errors.Join(failures...)
		}
		if err := ctx.Err(); err != nil {
			return cleanup(err)
		}
		if written, writeErr := direct.Write(data); writeErr != nil {
			return cleanup(writeErr)
		} else if written != len(data) {
			return cleanup(io.ErrShortWrite)
		}
		if chmodErr := direct.Chmod(0o600); chmodErr != nil {
			return cleanup(chmodErr)
		}
		if err := ctx.Err(); err != nil {
			return cleanup(err)
		}
		if syncErr := direct.Sync(); syncErr != nil {
			return cleanup(syncErr)
		}
		if closeErr := direct.Close(); closeErr != nil {
			closed = true
			return cleanup(closeErr)
		}
		closed = true
		if syncErr := ops.syncDir(); syncErr != nil {
			return cleanup(syncErr)
		}
		return nil
	}
	rollbackLink := func(cause error) error {
		failures := []error{cause}
		for _, entry := range []string{destination, name} {
			if removeErr := ops.remove(entry); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				failures = append(failures, fmt.Errorf("remove failed store entry %q: %w", entry, removeErr))
			}
		}
		if syncErr := ops.syncDir(); syncErr != nil {
			failures = append(failures, fmt.Errorf("sync failed store cleanup: %w", syncErr))
		}
		return errors.Join(failures...)
	}
	if syncErr := ops.syncDir(); syncErr != nil {
		return rollbackLink(syncErr)
	}
	if removeErr := ops.remove(name); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		return rollbackLink(removeErr)
	}
	if syncErr := ops.syncDir(); syncErr != nil {
		return rollbackLink(syncErr)
	}
	return nil
}

// Sync durably records directory-entry changes where supported.
func (r *Root) Sync() error {
	dir, err := r.handle.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return syncFile(dir)
}

func (r *Root) createTemp(prefix string) (string, *os.File, error) {
	prefix = strings.TrimSuffix(strings.TrimSuffix(prefix, "*.tmp"), "*")
	for range 100 {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := prefix + hex.EncodeToString(random[:]) + ".tmp"
		file, err := r.handle.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return name, file, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("create store temporary file: name collision limit reached")
}

// CheckDir validates an existing store without creating it.
func CheckDir(dir string) (bool, error) {
	root, err := OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, root.Close()
}

// EnsurePrivateDir safely creates and validates a private store.
func EnsurePrivateDir(dir string) error {
	root, err := EnsureRoot(dir)
	if err != nil {
		return err
	}
	return root.Close()
}

// ReadRegularLimit safely reads a file through a stable parent capability.
func ReadRegularLimit(path string, maxBytes int64) ([]byte, fs.FileInfo, error) {
	root, err := OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadRegular(filepath.Base(path), maxBytes)
}

// ReadRegular safely reads a file with a defensive one-GiB limit.
func ReadRegular(path string) ([]byte, fs.FileInfo, error) {
	return ReadRegularLimit(path, 1<<30)
}

// RemoveRegular safely removes a file through a stable parent capability.
func RemoveRegular(path string) error {
	root, err := OpenRoot(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.RemoveRegular(filepath.Base(path))
}

// AtomicWrite safely publishes a file through a stable directory capability.
func AtomicWrite(dir, destination, prefix string, data []byte) error {
	root, err := EnsureRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.AtomicWrite(filepath.Base(destination), prefix, data)
}

// SyncDir syncs an existing directory through a stable capability.
func SyncDir(path string) error {
	root, err := OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.Sync()
}

// WithLock opens or creates dir, acquires a non-breakable advisory lock, and
// runs fn through the same stable directory capability.
func WithLock(ctx context.Context, dir string, fn func(*Root) error) error {
	return WithLockChecked(ctx, dir, nil, fn)
}

// WithLockChecked opens or creates dir, runs a read-only check through that
// exact stable capability before the lock file can be created, then locks and
// invokes fn through the same capability.
func WithLockChecked(ctx context.Context, dir string, check func(*Root) error, fn func(*Root) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := EnsureRootContext(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if check != nil {
		if err := check(root); err != nil {
			return err
		}
	}
	return root.withLock(ctx, fn)
}

// WithLockContext acquires the store lock through this already-open stable
// directory capability. Callers can perform a read-only ownership preflight
// and then lock the exact same directory object without reopening a raced path.
func (r *Root) WithLockContext(ctx context.Context, fn func(*Root) error) error {
	return r.withLock(ctx, fn)
}

func (r *Root) withLock(ctx context.Context, fn func(*Root) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var lock *os.File
	for {
		info, err := r.Lstat(lockName)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			lock, err = r.handle.OpenFile(lockName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			if err != nil {
				return err
			}
		case err != nil:
			return err
		case info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular():
			return fmt.Errorf("store lock %q is not a regular file", lockName)
		default:
			lock, err = r.handle.OpenFile(lockName, os.O_RDWR, 0)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
		}
		break
	}
	defer func() { _ = lock.Close() }()
	opened, err := lock.Stat()
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		acquired, unlock, err := tryLock(lock)
		if err != nil {
			return err
		}
		if acquired {
			if err := ctx.Err(); err != nil {
				_ = unlock()
				return err
			}
			current, statErr := r.Lstat(lockName)
			if statErr != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
				_ = unlock()
				if statErr != nil {
					return statErr
				}
				return fmt.Errorf("store lock %q was replaced while waiting", lockName)
			}
			if err := r.StillCurrent(); err != nil {
				_ = unlock()
				return err
			}
			if err := ctx.Err(); err != nil {
				_ = unlock()
				return err
			}
			defer func() { _ = unlock() }()
			return fn(r)
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for store lock: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func validEntryName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !filepath.IsAbs(name)
}
