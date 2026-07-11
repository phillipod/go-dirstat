//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package storefs

import (
	"io/fs"
	"os"
	"syscall"
)

func privateStore(path string, info fs.FileInfo) bool {
	info = filesystemInfo(path, info)
	if info == nil {
		return false
	}
	return ownedByCurrentUser("", info) && info.Mode().Perm()&0o077 == 0
}

func ownedByCurrentUser(path string, info fs.FileInfo) bool {
	info = filesystemInfo(path, info)
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func makePrivateStore(_ string, chmod func(fs.FileMode) error) error { return chmod(0o700) }

// filesystemInfo prefers a fresh pathname stat when one is available. Some
// filesystems and Go versions expose reduced Sys metadata through os.Root.Stat;
// the path stat preserves the owner/mode check while the already-open Root
// capability continues to guard subsequent mutations against replacement.
func filesystemInfo(path string, info fs.FileInfo) fs.FileInfo {
	if path == "" {
		return info
	}
	actual, err := os.Lstat(path)
	if err != nil {
		return info
	}
	if info == nil || os.SameFile(info, actual) {
		return actual
	}
	// A pathname replacement is not evidence that the opened capability is
	// private. Return no metadata so callers fail closed instead of authorizing
	// the replacement through stale ownership information.
	return nil
}
