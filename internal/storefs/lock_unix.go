//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package storefs

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLock(file *os.File) (bool, func() error, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return true, func() error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }, nil
}
