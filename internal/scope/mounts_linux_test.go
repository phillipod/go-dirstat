//go:build linux

package scope

import "testing"

func TestMountTableRootMatchesDescendants(t *testing.T) {
	mt := &mountTable{
		byPath: map[string]string{
			"/":         "ext4",
			"/mnt/data": "btrfs",
		},
		points: []string{"/mnt/data", "/"},
	}

	for path, want := range map[string]string{
		"/":                   "ext4",
		"/home/user/project":  "ext4",
		"/mnt/data":           "btrfs",
		"/mnt/data/documents": "btrfs",
	} {
		if got := mt.fstype(path); got != want {
			t.Errorf("fstype(%q) = %q, want %q", path, got, want)
		}
	}
}
