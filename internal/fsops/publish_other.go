//go:build !linux && !darwin && !windows

package fsops

import (
	"errors"
	"io/fs"
	"os"
)

// renameNoReplace is a best-effort fallback for platforms without an exposed
// atomic no-replace rename primitive. Native release platforms use their
// kernel primitive instead.
func renameNoReplace(oldPath, newPath string) error {
	if _, err := os.Lstat(newPath); err == nil {
		return fs.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(oldPath, newPath)
}
