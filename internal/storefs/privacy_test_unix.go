//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package storefs

import (
	"io/fs"
	"testing"
)

func assertPrivateStoreEntry(t *testing.T, _ string, info fs.FileInfo) {
	t.Helper()
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("entry mode = %v, want private regular 0600", info.Mode())
	}
}
