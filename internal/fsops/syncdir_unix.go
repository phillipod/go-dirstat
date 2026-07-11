//go:build !windows

package fsops

import (
	"errors"
	"os"
)

func syncDirectory(path string, filesystem mutationFilesystem) (returnErr error) {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, filesystem.close(directory))
	}()
	return filesystem.sync(directory)
}
