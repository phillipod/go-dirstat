//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package storefs

import (
	"io/fs"
	"testing"
)

func assertPrivateStoreEntry(t *testing.T, _ string, info fs.FileInfo) {
	t.Helper()
	if !info.Mode().IsRegular() {
		t.Fatalf("entry mode = %v, want regular file", info.Mode())
	}
}
