//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package storefs

import "os"

func syncFile(file *os.File) error { return file.Sync() }
