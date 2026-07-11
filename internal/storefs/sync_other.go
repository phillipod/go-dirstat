//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package storefs

import "os"

func syncFile(_ *os.File) error { return nil }
