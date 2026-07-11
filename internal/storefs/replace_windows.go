//go:build windows

package storefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func replaceStoreEntry(_ *os.Root, rootPath, source, destination string) error {
	return moveStoreEntry(rootPath, source, destination, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func publishStoreEntryNoReplace(_ *os.Root, rootPath, source, destination string) error {
	err := moveStoreEntry(rootPath, source, destination, windows.MOVEFILE_WRITE_THROUGH)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return fs.ErrExist
	}
	return err
}

func moveStoreEntry(rootPath, source, destination string, flags uint32) error {
	oldPath, err := windows.UTF16PtrFromString(filepath.Join(rootPath, source))
	if err != nil {
		return err
	}
	newPath, err := windows.UTF16PtrFromString(filepath.Join(rootPath, destination))
	if err != nil {
		return err
	}
	return windows.MoveFileEx(oldPath, newPath, flags)
}
