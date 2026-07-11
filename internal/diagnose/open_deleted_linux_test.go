//go:build linux

package diagnose

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGatherFindsOpenDeletedFileInsideRequestedRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "held-open.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	result := Gather(context.Background(), []string{root})
	found := false
	for _, file := range result.OpenDeleted {
		if file.PID == os.Getpid() && file.Path == path && file.Size == 4096 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("open deleted file not found in %#v (warnings: %v)", result.OpenDeleted, result.Warnings)
	}
}

func TestWithinAnyHonorsPathBoundaries(t *testing.T) {
	if withinAny([]string{"/srv/data"}, "/srv/database/file") {
		t.Fatal("path prefix without a component boundary was accepted")
	}
	if !withinAny([]string{"/srv/data"}, "/srv/data/sub/file") {
		t.Fatal("descendant was rejected")
	}
}
