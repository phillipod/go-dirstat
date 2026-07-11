//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package storefs

import "os"

func replaceStoreEntry(root *os.Root, _ string, source, destination string) error {
	return root.Rename(source, destination)
}

func publishStoreEntryNoReplace(root *os.Root, _ string, source, destination string) error {
	return root.Link(source, destination)
}
