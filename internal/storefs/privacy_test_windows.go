//go:build windows

package storefs

import (
	"io/fs"
	"testing"
)

func assertPrivateStoreEntry(t *testing.T, path string, info fs.FileInfo) {
	t.Helper()
	if !info.Mode().IsRegular() {
		t.Fatalf("entry mode = %v, want regular file", info.Mode())
	}
	if !privateStore(path, info) {
		t.Fatalf("entry %q does not have a current-user private owner/DACL", path)
	}
}
