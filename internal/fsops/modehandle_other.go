//go:build !unix && !windows

package fsops

import "os"

func openDirectoryModeHandle(path string) (*os.File, error) {
	return os.Open(path)
}
