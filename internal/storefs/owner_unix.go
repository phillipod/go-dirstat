//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package storefs

import (
	"io/fs"
	"os"
	"syscall"
)

func privateStore(_ string, info fs.FileInfo) bool {
	return ownedByCurrentUser("", info) && info.Mode().Perm()&0o077 == 0
}

func ownedByCurrentUser(_ string, info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func makePrivateStore(_ string, chmod func(fs.FileMode) error) error { return chmod(0o700) }
