//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package storefs

import "io/fs"

func privateStore(_ string, _ fs.FileInfo) bool { return true }

func ownedByCurrentUser(_ string, _ fs.FileInfo) bool { return true }

func makePrivateStore(_ string, chmod func(fs.FileMode) error) error { return chmod(0o700) }
