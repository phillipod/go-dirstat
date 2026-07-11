package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/phillipod/go-dirstat/internal/fsops"
)

func TestPlanRepeatedSourcesAreGuardedOrderedDeduplicatedAndSummarized(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, name), bytes.Repeat([]byte(name), 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, stderr, err := runPlanCLI(t, context.Background(), nil,
		"plan", "--root", root, "--source", "a", "--source", "b", "--source", "a", "--summary", "delete",
	)
	if err != nil {
		t.Fatal(err)
	}
	plan := readCLIPlan(t, out)
	if len(plan.Operations) != 2 {
		t.Fatalf("operations = %#v", plan.Operations)
	}
	for index, name := range []string{"a", "b"} {
		operation := plan.Operations[index]
		if operation.ID != fmt.Sprintf("delete-%d", index+1) || !sameTestPath(operation.Source, filepath.Join(canonicalRoot, name)) || operation.Expected == nil {
			t.Fatalf("operation %d = %#v", index, operation)
		}
	}
	var summary planSummary
	if err := json.Unmarshal([]byte(stderr), &summary); err != nil {
		t.Fatalf("summary %q: %v", stderr, err)
	}
	if summary.Type != "plan_summary" || summary.InputOperations != 3 || summary.Operations != 2 ||
		summary.DeduplicatedOperations != 1 || summary.ActionCounts["delete"] != 2 ||
		!summary.ReclaimEstimateComplete || summary.DeleteReclaimBytes <= 0 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestPlanFiles0FromPreservesHostileNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not accept every control character used by this filename fixture")
	}
	t.Parallel()
	root := t.TempDir()
	names := []string{"line\nbreak", "tab\tname", "-leading-dash"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var input bytes.Buffer
	for _, name := range names {
		input.WriteString(name)
		input.WriteByte(0)
	}
	out, _, err := runPlanCLI(t, context.Background(), &input,
		"plan", "--root", root, "--files0-from", "-", "delete",
	)
	if err != nil {
		t.Fatal(err)
	}
	plan := readCLIPlan(t, out)
	if len(plan.Operations) != len(names) {
		t.Fatalf("operations = %#v", plan.Operations)
	}
	for index, name := range names {
		if plan.Operations[index].Source != filepath.Join(root, name) {
			t.Fatalf("source %d = %q", index, plan.Operations[index].Source)
		}
	}
}

func TestPlanBatchInputsFromFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	nulPath := filepath.Join(t.TempDir(), "sources.nul")
	if err := os.WriteFile(nulPath, []byte("source\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runPlanCLI(t, context.Background(), nil,
		"plan", "--root", root, "--files0-from", nulPath, "delete",
	)
	if err != nil || len(readCLIPlan(t, out).Operations) != 1 {
		t.Fatalf("NUL file plan error=%v output=%q", err, out)
	}
	requestPath := filepath.Join(t.TempDir(), "requests.jsonl")
	if err := os.WriteFile(requestPath, []byte("{\"action\":\"touch\",\"source\":\"created\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err = runPlanCLI(t, context.Background(), nil,
		"plan", "--root", root, "--operations-from", requestPath,
	)
	if err != nil || len(readCLIPlan(t, out).Operations) != 1 {
		t.Fatalf("JSONL file plan error=%v output=%q", err, out)
	}
}

func TestPlanMixedJSONLDependencyRoundTripDryRunAndApply(t *testing.T) {
	configureCLIState(t)
	root := t.TempDir()
	requests := strings.Join([]string{
		`{"action":"mkdir","source":"generated"}`,
		`{"action":"touch","source":"generated/file"}`,
		`{"action":"copy","source":"generated/file","destination":"copy"}`,
	}, "\n") + "\n"
	out, _, err := runPlanCLI(t, context.Background(), strings.NewReader(requests),
		"plan", "--root", root, "--operations-from", "-",
	)
	if err != nil {
		t.Fatal(err)
	}
	plan := readCLIPlan(t, out)
	if len(plan.Operations) != 3 || plan.Operations[0].ID != "mkdir-1" ||
		plan.Operations[1].ID != "touch-2" || plan.Operations[2].ID != "copy-3" {
		t.Fatalf("plan = %#v", plan)
	}
	for _, operation := range plan.Operations {
		if operation.Expected != nil {
			t.Fatalf("generated dependency unexpectedly has pre-plan guard: %#v", operation)
		}
	}
	dryOutput, _, err := runPlanCLI(t, context.Background(), strings.NewReader(out),
		"apply", "--dry-run", "--no-audit", "-",
	)
	if err != nil {
		t.Fatalf("CLI dry run: %v", err)
	}
	dryResults, err := fsops.ReadResults(strings.NewReader(dryOutput))
	if err != nil || len(dryResults) != 3 {
		t.Fatalf("dry-run results=%#v error=%v", dryResults, err)
	}
	if _, err := os.Stat(filepath.Join(root, "generated")); !os.IsNotExist(err) {
		t.Fatalf("dry run mutated root: %v", err)
	}
	applyOutput, _, err := runPlanCLI(t, context.Background(), strings.NewReader(out),
		"apply", "--yes", "--no-audit", "-",
	)
	if err != nil {
		t.Fatalf("CLI apply: %v", err)
	}
	results, err := fsops.ReadResults(strings.NewReader(applyOutput))
	if err != nil || len(results) != 3 {
		t.Fatalf("apply results=%#v error=%v", results, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "copy")); err != nil || len(data) != 0 {
		t.Fatalf("copied generated file data=%q error=%v", data, err)
	}
}

func TestPlanBatchCopyMapsBasenamesAndCapturesDestinationGuards(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"left", "right", "destination"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"left/one", "right/two"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	existingDestination := filepath.Join(root, "destination", "two")
	canonicalExistingDestination := filepath.Join(canonicalRoot, "destination", "two")
	if err := os.WriteFile(existingDestination, []byte("reviewed"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runPlanCLI(t, context.Background(), nil,
		"plan", "--root", root, "--source", "left/one", "--source", "right/two",
		"--destination-dir", "destination", "copy",
	)
	if err != nil {
		t.Fatal(err)
	}
	plan := readCLIPlan(t, out)
	if len(plan.Operations) != 2 {
		t.Fatalf("operations = %#v", plan.Operations)
	}
	first, second := plan.Operations[0], plan.Operations[1]
	if !sameTestPath(first.Destination, filepath.Join(canonicalRoot, "destination", "one")) || first.ExpectedDestination == nil || first.ExpectedDestination.Exists || !sameTestPath(first.ExpectedDestination.Path, filepath.Join(canonicalRoot, "destination", "one")) {
		t.Fatalf("first destination guard = %#v", first)
	}
	if !sameTestPath(second.Destination, canonicalExistingDestination) || second.ExpectedDestination == nil || !second.ExpectedDestination.Exists || second.ExpectedDestination.Entry == nil {
		t.Fatalf("second destination guard = %#v", second)
	}
	if !sameTestPath(second.ExpectedDestination.Path, canonicalExistingDestination) || !sameTestPath(second.ExpectedDestination.Entry.Path, canonicalExistingDestination) {
		t.Fatalf("second destination paths = %#v", second.ExpectedDestination)
	}
	if first.Expected == nil || second.Expected == nil {
		t.Fatalf("source guards missing: %#v", plan.Operations)
	}
}

func TestPlanRecursiveDeleteDeduplicatesDescendants(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "child"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runPlanCLI(t, context.Background(), nil,
		"plan", "--root", root, "--recursive", "--source", "parent/child", "--source", "parent", "delete",
	)
	if err != nil {
		t.Fatal(err)
	}
	plan := readCLIPlan(t, out)
	if len(plan.Operations) != 1 || !sameTestPath(plan.Operations[0].Source, parent) || !plan.Operations[0].Recursive {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestPlanBatchCopyRequiresUniqueDestinationTargets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, directory := range []string{"left", "right", "destination"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{"left", "right"} {
		if err := os.WriteFile(filepath.Join(root, directory, "same"), []byte(directory), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, _, err := runPlanCLI(t, context.Background(), nil,
		"plan", "--root", root, "--source", "left/same", "--source", "right/same",
		"--destination-dir", "destination", "copy",
	)
	if err == nil || !strings.Contains(err.Error(), "destination") || out != "" {
		t.Fatalf("output=%q error=%v", out, err)
	}
}

func TestPlanBatchRejectsIncompatibleInputsWithoutPrefix(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tests := []struct {
		name string
		in   io.Reader
		args []string
		want string
	}{
		{name: "source and NUL", args: []string{"plan", "--root", root, "--source", "a", "--files0-from", "-", "delete"}, want: "mutually exclusive"},
		{name: "NUL and JSONL", args: []string{"plan", "--root", root, "--files0-from", "-", "--operations-from", "requests", "delete"}, want: "mutually exclusive"},
		{name: "JSONL positional", args: []string{"plan", "--root", root, "--operations-from", "-", "delete"}, want: "no positional"},
		{name: "JSONL global recursive", in: strings.NewReader(`{"action":"delete","source":"x"}` + "\n"), args: []string{"plan", "--root", root, "--operations-from", "-", "--recursive"}, want: "cannot be used"},
		{name: "batch copy destination", args: []string{"plan", "--root", root, "--source", "x", "copy"}, want: "requires --destination-dir"},
		{name: "batch archive ambiguous", args: []string{"plan", "--root", root, "--source", "x", "--destination-dir", "out", "archive"}, want: "explicit destinations"},
		{name: "legacy destination dir", args: []string{"plan", "--root", root, "--destination-dir", "out", "touch", "x"}, want: "only valid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			out, _, err := runPlanCLI(t, context.Background(), test.in, test.args...)
			if err == nil || !strings.Contains(err.Error(), test.want) || out != "" {
				t.Fatalf("output=%q error=%v, want %q", out, err, test.want)
			}
		})
	}
}

func TestPlanBatchRejectsMalformedTruncatedAndInvalidUTF8Inputs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tests := []struct {
		name string
		in   []byte
		args []string
		want string
	}{
		{name: "truncated NUL", in: []byte("name"), args: []string{"plan", "--root", root, "--files0-from", "-", "delete"}, want: "not NUL-terminated"},
		{name: "empty NUL record", in: []byte("a\x00\x00"), args: []string{"plan", "--root", root, "--files0-from", "-", "delete"}, want: "record 2 is empty"},
		{name: "invalid UTF-8 NUL", in: []byte{0xff, 0}, args: []string{"plan", "--root", root, "--files0-from", "-", "delete"}, want: "not valid UTF-8"},
		{name: "invalid later JSONL", in: []byte("{\"action\":\"touch\",\"source\":\"ok\"}\n{\"action\":\"touch\",\"source\":\"bad\",\"unknown\":true}\n"), args: []string{"plan", "--root", root, "--operations-from", "-"}, want: "unknown field"},
		{name: "invalid duplicate is not hidden", in: []byte("{\"action\":\"delete\",\"source\":\"x\",\"recursive\":true}\n{\"action\":\"delete\",\"source\":\"x\",\"size\":1}\n"), args: []string{"plan", "--root", root, "--operations-from", "-"}, want: "size cannot be used with delete"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			out, _, err := runPlanCLI(t, context.Background(), bytes.NewReader(test.in), test.args...)
			if err == nil || !strings.Contains(err.Error(), test.want) || out != "" {
				t.Fatalf("output=%q error=%v, want %q", out, err, test.want)
			}
		})
	}
}

func TestPlanBatchRejectsConflictAndCancellationWithoutPrefix(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := "{\"action\":\"copy\",\"source\":\"source\",\"destination\":\"same\"}\n" +
		"{\"action\":\"copy\",\"source\":\"other\",\"destination\":\"same\"}\n"
	out, _, err := runPlanCLI(t, context.Background(), strings.NewReader(requests),
		"plan", "--root", root, "--operations-from", "-",
	)
	if err == nil || out != "" {
		t.Fatalf("conflict output=%q error=%v", out, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, _, err = runPlanCLI(t, ctx, nil,
		"plan", "--root", root, "--source", "source", "delete",
	)
	if err == nil || out != "" {
		t.Fatalf("cancellation output=%q error=%v", out, err)
	}
}

func TestPlanBatchConfinementPrecedesRecursiveDeduplicationInspection(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	out, _, err := runPlanCLI(t, context.Background(), nil,
		"plan", "--root", root, "--recursive", "--source", "escape/tree", "--source", "escape/tree/child", "delete",
	)
	if err == nil || !strings.Contains(err.Error(), "escapes root") || out != "" {
		t.Fatalf("output=%q error=%v", out, err)
	}
}

func TestReadBoundedInputChecksByteNPlusOne(t *testing.T) {
	t.Parallel()
	if data, err := readBoundedInput(strings.NewReader("1234"), 4); err != nil || string(data) != "1234" {
		t.Fatalf("exact limit data=%q error=%v", data, err)
	}
	if _, err := readBoundedInput(strings.NewReader("12345"), 4); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("byte N+1 error = %v", err)
	}
}

func TestPlanHelpDocumentsSingleAndBatchContracts(t *testing.T) {
	t.Parallel()
	out, _, err := runPlanCLI(t, context.Background(), nil, "plan", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"plan [ACTION [SOURCE [DESTINATION]]]", "repeatable --source", "--files0-from",
		"--operations-from", "strict request-only JSONL", "maximum 64 MiB",
		"aggregate plan-impact JSON to stderr",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q:\n%s", want, out)
		}
	}
}

func runPlanCLI(t *testing.T, ctx context.Context, input io.Reader, args ...string) (string, string, error) {
	t.Helper()
	cmd := New()
	cmd.SetContext(ctx)
	cmd.SetArgs(args)
	if input != nil {
		cmd.SetIn(input)
	}
	var stdout, stderr strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}
