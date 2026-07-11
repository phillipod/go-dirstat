package cli

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/phillipod/go-dirstat/internal/fsops"
)

func TestPlanCommandWritesVersionedExpectedMetadata(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "large.log")
	if err := os.WriteFile(source, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := executeOperationCLI(t, nil, "plan", "--root", root, "delete", "large.log")
	plan := readCLIPlan(t, out)
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSource := filepath.Join(canonicalRoot, "large.log")
	if plan.Header.Version != fsops.PlanVersion || plan.Header.Root != canonicalRoot || len(plan.Operations) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	op := plan.Operations[0]
	if op.Action != fsops.ActionDelete || op.Source != canonicalSource || op.Expected == nil {
		t.Fatalf("operation = %#v", op)
	}
	if op.Expected.Path != canonicalSource || op.Expected.Size != 7 || op.Expected.Kind != "file" {
		t.Fatalf("expected = %#v", op.Expected)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("planning mutated source: %v", err)
	}
}

func TestPlanCommandCoversEveryAction(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "source")
	if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "directory")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	archiveFile, err := os.Create(filepath.Join(root, "source.tar"))
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(archiveFile)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, action       string
		args               []string
		check              func(*testing.T, fsops.Operation)
		unsupportedWindows bool
	}{
		{name: "delete", action: "delete", args: []string{"source"}},
		{name: "recursive delete", action: "delete", args: []string{"--recursive", "directory"}, check: func(t *testing.T, op fsops.Operation) {
			if !op.Recursive {
				t.Fatal("recursive bit not recorded")
			}
		}},
		{name: "copy", action: "copy", args: []string{"source", "copy"}},
		{name: "move", action: "move", args: []string{"source", "moved"}},
		{name: "rename", action: "rename", args: []string{"source", "renamed"}},
		{name: "mkdir", action: "mkdir", args: []string{"--mode", "0750", "new-dir"}, unsupportedWindows: true, check: func(t *testing.T, op fsops.Operation) {
			if op.Expected != nil || op.Mode == nil || *op.Mode != 0o750 {
				t.Fatalf("mkdir operation = %#v", op)
			}
		}},
		{name: "touch", action: "touch", args: []string{"new-file"}},
		{name: "truncate", action: "truncate", args: []string{"--size", "2K", "source"}, check: func(t *testing.T, op fsops.Operation) {
			if op.Size == nil || *op.Size != 2048 {
				t.Fatalf("truncate size = %v", op.Size)
			}
		}},
		{name: "chmod", action: "chmod", args: []string{"--mode", "640", "source"}, unsupportedWindows: true},
		{name: "chown", action: "chown", args: []string{"--uid", "0", "source"}, unsupportedWindows: true},
		{name: "archive", action: "archive", args: []string{"--archive-format", "tar.gz", "directory", "data.tgz"}},
		{name: "extract", action: "extract", args: []string{"--archive-format", "tar", "source.tar", "out"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"plan", "--root", root, tt.action}
			args = append(args, tt.args...)
			if runtime.GOOS == windowsOS && tt.unsupportedWindows {
				err := executeOperationCLIError(t, args...)
				if err == nil || !strings.Contains(err.Error(), "unsupported on windows") {
					t.Fatalf("error = %v, want unsupported-on-windows rejection", err)
				}
				return
			}
			plan := readCLIPlan(t, executeOperationCLI(t, nil, args...))
			if len(plan.Operations) != 1 || plan.Operations[0].Action != fsops.Action(tt.action) {
				t.Fatalf("plan = %#v", plan)
			}
			if tt.check != nil {
				tt.check(t, plan.Operations[0])
			}
		})
	}
}

func TestPlanCommandRecordsDestinationAbsenceAndIdentity(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalDestination := filepath.Join(canonicalRoot, "destination")
	absentPlan := readCLIPlan(t, executeOperationCLI(t, nil, "plan", "--root", root, "copy", "source", "destination"))
	absent := absentPlan.Operations[0].ExpectedDestination
	if absent == nil || absent.Exists || absent.Entry != nil || !sameTestPath(absent.Path, canonicalDestination) {
		t.Fatalf("absent destination guard = %#v", absent)
	}
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	presentPlan := readCLIPlan(t, executeOperationCLI(t, nil, "plan", "--root", root, "copy", "source", "destination"))
	present := presentPlan.Operations[0].ExpectedDestination
	if present == nil || !present.Exists || present.Entry == nil || !sameTestPath(present.Entry.Path, canonicalDestination) || present.Entry.Size != 3 {
		t.Fatalf("present destination guard = %#v", present)
	}
}

func TestPlanCommandRejectsUnsafeOrInvalidRequests(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown action", args: []string{"explode", "source"}, want: "unsupported action"},
		{name: "missing destination", args: []string{"copy", "source"}, want: "requires SOURCE and DESTINATION"},
		{name: "extra destination", args: []string{"delete", "source", "other"}, want: "accepts SOURCE only"},
		{name: "root escape", args: []string{"touch", "../outside"}, want: "escapes root"},
		{name: "root mutation", args: []string{"delete", "."}, want: "plan root"},
		{name: "missing truncate size", args: []string{"truncate", "source"}, want: "requires --size"},
		{name: "bad mode", args: []string{"chmod", "--mode", "888", "source"}, want: "invalid --mode"},
		{name: "missing owner", args: []string{"chown", "source"}, want: "requires --uid or --gid"},
		{name: "recursive copy", args: []string{"copy", "--recursive", "source", "copy"}, want: "--recursive cannot be used"},
		{name: "wrong flag", args: []string{"delete", "--size", "1", "source"}, want: "--size cannot be used"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := New()
			cmd.SetArgs(append([]string{"plan", "--root", root}, tt.args...))
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPlanCommandWritesPrivateFileWithoutOverwriting(t *testing.T) {
	root := t.TempDir()
	planPath := filepath.Join(t.TempDir(), "cleanup.jsonl")
	cmd := New()
	cmd.SetArgs([]string{"plan", "--root", root, "--output", planPath, "touch", "new"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != windowsOS && info.Mode().Perm() != 0o600 {
		t.Fatalf("plan mode = %o", info.Mode().Perm())
	}
	cmd = New()
	cmd.SetArgs([]string{"plan", "--root", root, "--output", planPath, "touch", "other"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("overwrite error = %v", err)
	}
}

func TestApplyRequiresYesAndDryRunEmitsResult(t *testing.T) {
	configureCLIState(t)
	root := t.TempDir()
	planText := executeOperationCLI(t, nil, "plan", "--root", root, "touch", "created")

	cmd := New()
	cmd.SetArgs([]string{"apply", "-"})
	cmd.SetIn(strings.NewReader(planText))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "explicit --yes") {
		t.Fatalf("confirmation error = %v", err)
	}

	out := executeOperationCLI(t, strings.NewReader(planText), "apply", "--dry-run", "-")
	results, err := fsops.ReadResults(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].DryRun || results[0].Status != "ok" {
		t.Fatalf("results = %#v", results)
	}
	if _, err := os.Stat(filepath.Join(root, "created")); !os.IsNotExist(err) {
		t.Fatalf("dry-run mutated path: %v", err)
	}
}

func TestApplyRejectsStrictJSONLBeforeMutation(t *testing.T) {
	configureCLIState(t)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	header := fmt.Sprintf(`{"type":"plan","version":%d,"root":%q}`, fsops.PlanVersion, root)
	operation := fmt.Sprintf(`{"type":"operation","id":"touch","action":"touch","source":%q,"unknown":true}`, target)
	cmd := New()
	cmd.SetArgs([]string{"apply", "--yes", "--no-audit", "-"})
	cmd.SetIn(strings.NewReader(header + "\n" + operation + "\n"))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), `unknown field "unknown"`) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("strict-plan rejection mutated target: %v", err)
	}
}

func TestApplyHelpDocumentsStrictCompletePlanContract(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"apply", "--help"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	help := out.String()
	for _, want := range []string{"strict JSONL", "one object per physical line", "64 MiB", "before any mutation"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

func TestApplyMutatesAuditsAndSupportsNoAudit(t *testing.T) {
	state := configureCLIState(t)
	root := t.TempDir()
	source := filepath.Join(root, "remove")
	if err := os.WriteFile(source, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	planText := executeOperationCLI(t, nil, "plan", "--root", root, "delete", "remove")
	out := executeOperationCLI(t, strings.NewReader(planText), "apply", "--yes", "-")
	results, err := fsops.ReadResults(strings.NewReader(out))
	if err != nil || len(results) != 1 || results[0].Status != "ok" {
		t.Fatalf("results = %#v, err = %v", results, err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source remains: %v", err)
	}
	audit := filepath.Join(state, "dirstat", "operations.jsonl")
	if info, err := os.Stat(audit); err != nil || runtime.GOOS != windowsOS && info.Mode().Perm() != 0o600 {
		t.Fatalf("audit info = %v, %v", info, err)
	}

	second := executeOperationCLI(t, nil, "plan", "--root", root, "touch", "without-audit")
	if err := os.Remove(audit); err != nil {
		t.Fatal(err)
	}
	executeOperationCLI(t, strings.NewReader(second), "apply", "--yes", "--no-audit", "-")
	if _, err := os.Stat(audit); !os.IsNotExist(err) {
		t.Fatalf("--no-audit created audit: %v", err)
	}
}

func TestApplyConflictOverwriteAndStaleResult(t *testing.T) {
	configureCLIState(t)
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	copyPlan := executeOperationCLI(t, nil, "plan", "--root", root, "copy", "source", "destination")
	executeOperationCLI(t, strings.NewReader(copyPlan), "apply", "--yes", "--conflict", "overwrite", "-")
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "new" {
		t.Fatalf("destination = %q, %v", data, err)
	}

	stalePlan := executeOperationCLI(t, nil, "plan", "--root", root, "delete", "source")
	if err := os.WriteFile(source, []byte("changed-size"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := New()
	cmd.SetArgs([]string{"apply", "--yes", "-"})
	cmd.SetIn(strings.NewReader(stalePlan))
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "stale source") {
		t.Fatalf("stale error = %v", err)
	}
	results, readErr := fsops.ReadResults(strings.NewReader(out.String()))
	if readErr != nil || len(results) != 1 || results[0].Status != "error" {
		t.Fatalf("stale results = %#v, %v", results, readErr)
	}
}

func TestApplyRecursiveDeleteMustBePlannedExplicitly(t *testing.T) {
	configureCLIState(t)
	root := t.TempDir()
	dir := filepath.Join(root, "old-tree")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	plain := executeOperationCLI(t, nil, "plan", "--root", root, "delete", "old-tree")
	cmd := New()
	cmd.SetArgs([]string{"apply", "--yes", "--no-audit", "-"})
	cmd.SetIn(strings.NewReader(plain))
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("non-recursive plan deleted a non-empty directory")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("directory changed after rejected delete: %v", err)
	}

	recursive := executeOperationCLI(t, nil, "plan", "--root", root, "delete", "--recursive", "old-tree")
	executeOperationCLI(t, strings.NewReader(recursive), "apply", "--yes", "--no-audit", "-")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("recursive plan did not remove directory: %v", err)
	}
}

func executeOperationCLI(t *testing.T, in io.Reader, args ...string) string {
	t.Helper()
	cmd := New()
	cmd.SetArgs(args)
	if in != nil {
		cmd.SetIn(in)
	}
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("dirstat %v: %v", args, err)
	}
	return out.String()
}

func executeOperationCLIError(t *testing.T, args ...string) error {
	t.Helper()
	cmd := New()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.Execute()
}

func sameTestPath(left, right string) bool {
	if runtime.GOOS == windowsOS {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func readCLIPlan(t *testing.T, data string) fsops.Plan {
	t.Helper()
	plan, err := fsops.ReadPlan(bytes.NewBufferString(data))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func configureCLIState(t *testing.T) string {
	t.Helper()
	configHome, stateHome := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	return stateHome
}
