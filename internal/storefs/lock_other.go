//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package storefs

import (
	"errors"
	"os"
)

func tryLock(_ *os.File) (bool, func() error, error) {
	return false, nil, errors.New("store locking is unavailable on this platform")
}
