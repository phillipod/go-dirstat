package cli

import (
	"os"
	"path/filepath"
	"testing"

	appconfig "github.com/phillipod/go-dirstat/internal/config"
)

func TestResolveTUIAuditSkipsMutationStateInReadOnlyMode(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)
	cmd := newTUICommand(newConfig())
	readOnly, auditPath, disabled, err := resolveTUIAudit(cmd, appconfig.Config{ReadOnly: true}, false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !readOnly || !disabled || auditPath != "" {
		t.Fatalf("read_only=%t disabled=%t audit=%q", readOnly, disabled, auditPath)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("read-only audit resolution created state: %v", err)
	}
}

func TestResolveTUIAuditDefersDefaultDirectoryCreation(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)
	cmd := newTUICommand(newConfig())
	readOnly, auditPath, disabled, err := resolveTUIAudit(cmd, appconfig.Default(), false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(state, "dirstat", "operations.jsonl")
	if readOnly || disabled || auditPath != want {
		t.Fatalf("read_only=%t disabled=%t audit=%q, want %q", readOnly, disabled, auditPath, want)
	}
	if _, err := os.Stat(filepath.Dir(want)); !os.IsNotExist(err) {
		t.Fatalf("audit resolution eagerly created directory: %v", err)
	}
}

func TestResolveTUIAuditRejectsConflictingFlags(t *testing.T) {
	cmd := newTUICommand(newConfig())
	if err := cmd.Flags().Set("audit", filepath.Join(t.TempDir(), "audit.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := resolveTUIAudit(cmd, appconfig.Default(), false, true, "configured"); err == nil {
		t.Fatal("--audit and --no-audit were accepted together")
	}
}
