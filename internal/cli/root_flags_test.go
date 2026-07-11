package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnrelatedCommandsRejectChangedScanFlagsBeforeAndAfterSubcommand(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := [][]string{
		{"--follow", "status", root},
		{"status", "--follow", root},
		{"diagnose", "--cross-device", root},
		{"inspect", "--apparent", file},
		{"--jobs=2", "version"},
		{"skills", "view", "--no-hidden"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, err := executeCLI(args...)
			if err == nil || !strings.Contains(err.Error(), "scan flag(s)") || !strings.Contains(err.Error(), "not valid") {
				t.Fatalf("Execute(%q) error = %v, want irrelevant scan-flag rejection", args, err)
			}
		})
	}
}

func TestScanCommandsConsumeRelevantPersistentFlags(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "visible"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	tests := [][]string{
		{"--jobs=1", root},
		{"--no-hidden", "extensions", root},
		{"query", "--follow", "--kind=file", root},
		{"--exclude=*.tmp", "history", "--store", store, "list", root},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			if _, err := executeCLI(args...); err != nil {
				t.Fatalf("Execute(%q): %v", args, err)
			}
		})
	}
}

func TestHistoryRejectsPersistentFlagsItDoesNotConsume(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	for _, flag := range []string{"--jobs=2", "--apparent"} {
		_, err := executeCLI(flag, "history", "--store", store, "list", root)
		if err == nil || !strings.Contains(err.Error(), "not valid") {
			t.Fatalf("history list with %s error = %v", flag, err)
		}
	}
}
