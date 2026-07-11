package fileclass

import "testing"

func TestExtension(t *testing.T) {
	tests := map[string]string{
		"Makefile":       "",
		".env":           "",
		".gitignore":     "",
		".config.json":   ".json",
		"archive.tar.GZ": ".gz",
		"trailing.":      "",
	}
	for name, want := range tests {
		if got := Extension(name); got != want {
			t.Errorf("Extension(%q) = %q, want %q", name, got, want)
		}
	}
}
