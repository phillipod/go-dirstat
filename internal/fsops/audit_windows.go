//go:build windows

package fsops

import (
	"errors"
	"io/fs"
	"os"
)

func openAuditFile(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("refusing audit symlink")
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}
