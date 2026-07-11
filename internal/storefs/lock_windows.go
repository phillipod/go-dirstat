//go:build windows

package storefs

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryLock(file *os.File) (bool, func() error, error) {
	overlapped := new(windows.Overlapped)
	handle := windows.Handle(file.Fd())
	err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return true, func() error { return windows.UnlockFileEx(handle, 0, 1, 0, overlapped) }, nil
}
