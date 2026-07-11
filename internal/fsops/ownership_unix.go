//go:build unix

package fsops

import "os"

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

func chownNoFollow(path string, uid, gid int) error { return os.Lchown(path, uid, gid) }
