package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phillipod/go-dirstat/internal/diagnose"
	"github.com/phillipod/go-dirstat/internal/fsinfo"
)

func TestPressureThresholdValidationAndBreach(t *testing.T) {
	for _, value := range []float64{-2, 101, math.NaN(), math.Inf(1)} {
		if _, err := validatePressureThresholds(value, -1); err == nil {
			t.Fatalf("byte threshold %v was accepted", value)
		}
	}
	thresholds, err := validatePressureThresholds(80, 90)
	if err != nil {
		t.Fatal(err)
	}
	volumes := []fsinfo.Volume{{Path: "/data", CallerPressurePct: 81, Inodes: 100, InodePct: 91}}
	err = thresholds.breach(volumes)
	if ExitCode(err) != ExitPressureThreshold || !strings.Contains(err.Error(), "byte pressure") || !strings.Contains(err.Error(), "inode pressure") {
		t.Fatalf("pressure breach = %v, exit=%d", err, ExitCode(err))
	}
	if err := (pressureThresholds{bytes: 81, inodes: 91}).breach(volumes); err != nil {
		t.Fatalf("equal maximum was treated as a breach: %v", err)
	}
	err = (pressureThresholds{bytes: 0, inodes: 90}).breach([]fsinfo.Volume{{
		Path: "/without-inodes", CallerPressurePct: 99,
	}})
	if ExitCode(err) != ExitDiagnosticPartial || !strings.Contains(err.Error(), "inode pressure is unavailable") {
		t.Fatalf("unavailable inode evidence = %v, exit=%d", err, ExitCode(err))
	}
}

func TestDiagnosticPartialClassification(t *testing.T) {
	complete := diagnose.Result{
		Capabilities:       []diagnose.Capability{{Name: "probe", Available: true}},
		OpenDeletedSummary: &diagnose.OpenDeletedSummary{Coverage: diagnose.OpenDeletedCoverage{Complete: true}},
	}
	if diagnosticPartial(complete) {
		t.Fatal("complete diagnostics classified as partial")
	}
	partial := []diagnose.Result{
		{Warnings: []string{"permission denied"}},
		{Capabilities: []diagnose.Capability{{Name: "probe", Available: false}}},
		{OpenDeletedSummary: &diagnose.OpenDeletedSummary{Coverage: diagnose.OpenDeletedCoverage{Complete: false}}},
	}
	for _, result := range partial {
		if !diagnosticPartial(result) {
			t.Fatalf("partial diagnostics classified complete: %#v", result)
		}
	}
}

func TestStatusThresholdReturnsStableExitAfterValidOutput(t *testing.T) {
	output, err := executeCLI("status", "--format=json", "--max-byte-pressure=0", t.TempDir())
	if ExitCode(err) != ExitPressureThreshold {
		t.Fatalf("status error = %v, exit=%d", err, ExitCode(err))
	}
	var volumes []fsinfo.Volume
	if decodeErr := json.Unmarshal([]byte(output), &volumes); decodeErr != nil || len(volumes) != 1 {
		t.Fatalf("threshold status JSON = %q, volumes=%#v, error=%v", output, volumes, decodeErr)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := executeCLI("status", "--max-byte-pressure=nan", missing); err == nil || strings.Contains(err.Error(), "no such") {
		t.Fatalf("invalid threshold did not precede filesystem access: %v", err)
	}
}

func TestQueryCandidateConditionsReturnStableExitAfterOutput(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "candidate"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := executeCLI("query", "--kind=file", "--fields=relative", "--fail-if-match", root)
	if ExitCode(err) != ExitCandidateState || strings.TrimSpace(output) != "candidate" {
		t.Fatalf("fail-if-match output=%q error=%v exit=%d", output, err, ExitCode(err))
	}
	empty := t.TempDir()
	output, err = executeCLI("query", "--kind=file", "--require-match", empty)
	if ExitCode(err) != ExitCandidateState || output != "" {
		t.Fatalf("require-match output=%q error=%v exit=%d", output, err, ExitCode(err))
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := executeCLI("query", "--require-match", "--fail-if-match", missing); err == nil || strings.Contains(err.Error(), "no such") {
		t.Fatalf("candidate flag conflict did not precede scan: %v", err)
	}
}

func TestDiagnoseRequireCompletePrecedesPressureCondition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := New()
	cmd.SetArgs([]string{"diagnose", "--format=json", "--require-complete", "--max-byte-pressure=0", t.TempDir()})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	err := cmd.ExecuteContext(ctx)
	if ExitCode(err) != ExitDiagnosticPartial {
		t.Fatalf("diagnose error = %v, exit=%d", err, ExitCode(err))
	}
	var result diagnose.Result
	if decodeErr := json.Unmarshal(output.Bytes(), &result); decodeErr != nil || len(result.Warnings) == 0 {
		t.Fatalf("partial diagnostic JSON = %q, result=%#v, error=%v", output.String(), result, decodeErr)
	}
}

func TestExitCodeConditionPrecedence(t *testing.T) {
	if got := ExitCode(&IncompleteScanError{Path: "/data", Errors: 1}); got != ExitScanIncomplete {
		t.Fatalf("incomplete exit = %d", got)
	}
	if got := ExitCode(&conditionError{code: ExitDiagnosticPartial, message: "partial"}); got != ExitDiagnosticPartial {
		t.Fatalf("diagnostic exit = %d", got)
	}
	if got := ExitCode(nil); got != 1 {
		t.Fatalf("nil error exit = %d", got)
	}
}
