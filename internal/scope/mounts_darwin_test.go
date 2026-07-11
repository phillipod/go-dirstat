//go:build darwin

package scope

import "testing"

func TestDarwinMountsResolveTemporaryDirectory(t *testing.T) {
	got := loadMounts().fstype(t.TempDir())
	if got == "" {
		t.Fatal("statfs did not report a filesystem type for a temporary directory")
	}
}
