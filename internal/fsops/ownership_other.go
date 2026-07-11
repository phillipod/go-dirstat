//go:build !unix

package fsops

import (
	"errors"
	"os"
)

func chmodNoFollow(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return os.ErrInvalid
	}
	return os.Chmod(path, mode)
}

func chownNoFollow(string, int, int) error {
	return errors.New("chown is not supported on this platform")
}
