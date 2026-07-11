package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/phillipod/go-dirstat/internal/scan"
)

func TestAcceptScanRequiresExplicitPartialOptIn(t *testing.T) {
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	stats := scan.Stats{Errors: 2, Complete: false}
	err := acceptScan(cmd, "/srv", stats, false)
	var incomplete *IncompleteScanError
	if !errors.As(err, &incomplete) || incomplete.Errors != 2 {
		t.Fatalf("error = %#v, want incomplete scan", err)
	}
	if ExitCode(err) != ExitScanIncomplete {
		t.Fatalf("exit code = %d, want %d", ExitCode(err), ExitScanIncomplete)
	}
	if err := acceptScan(cmd, "/srv", stats, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "--allow-partial") || !strings.Contains(stderr.String(), "2 filesystem entries") {
		t.Fatalf("warning = %q", stderr.String())
	}
}

func TestExitCodeDefaultsToFailure(t *testing.T) {
	if got := ExitCode(errors.New("failure")); got != 1 {
		t.Fatalf("exit code = %d, want 1", got)
	}
}

func TestMachineCommandsRejectPartialScansBeforeOutput(t *testing.T) {
	root := partialScanFixture(t)
	for _, args := range [][]string{
		{"--format=tsv", root},
		{"extensions", root},
		{"query", root},
	} {
		cmd := New()
		cmd.SetArgs(args)
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&bytes.Buffer{})
		err := cmd.Execute()
		var incomplete *IncompleteScanError
		if !errors.As(err, &incomplete) {
			t.Fatalf("dirstat %v error = %v, want incomplete scan", args, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("dirstat %v emitted partial stdout: %q", args, stdout.String())
		}
	}
}

func TestAllowPartialEmitsResultsAndWarning(t *testing.T) {
	root := partialScanFixture(t)
	cmd := New()
	cmd.SetArgs([]string{"query", "--allow-partial", root})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() == 0 || !strings.Contains(stderr.String(), "1 filesystem entries") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestHistoryDoesNotRecordPartialScan(t *testing.T) {
	root := partialScanFixture(t)
	store := t.TempDir()
	cmd := New()
	cmd.SetArgs([]string{"history", "--store", store, "growth", root})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	var incomplete *IncompleteScanError
	if !errors.As(err, &incomplete) {
		t.Fatalf("history error = %v, want incomplete scan", err)
	}
	if err := filepath.WalkDir(store, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && filepath.Ext(path) == ".bin" {
			t.Fatalf("partial history snapshot was recorded at %s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func partialScanFixture(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == windowsOS {
		t.Skip("POSIX mode bits are required to force a deterministic ReadDir failure")
	}
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "unknown.bin"), []byte("unknown"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	if _, err := os.ReadDir(blocked); err == nil {
		t.Skip("test runner can read mode-000 directories")
	}
	return root
}
