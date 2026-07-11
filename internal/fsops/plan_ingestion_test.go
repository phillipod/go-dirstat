package fsops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
)

func TestReadPlanRequiresStrictOneRecordPerLineJSON(t *testing.T) {
	t.Parallel()
	header := `{"type":"plan","version":2,"root":"/tmp/root"}`
	operation := `{"type":"operation","id":"one","action":"touch","source":"new"}`
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown header field", data: `{"type":"plan","version":2,"root":"/tmp/root","typo":true}` + "\n", want: `unknown field "typo"`},
		{name: "unknown operation field", data: header + "\n" + `{"type":"operation","id":"one","action":"touch","source":"new","typo":true}` + "\n", want: `unknown field "typo"`},
		{name: "duplicate field", data: header + "\n" + `{"type":"operation","id":"one","id":"two","action":"touch","source":"new"}` + "\n", want: `duplicate field "id"`},
		{name: "duplicate nested field", data: header + "\n" + `{"type":"operation","id":"one","action":"delete","source":"file","expected":{"path":"file","path":"other"}}` + "\n", want: `duplicate field "expected.path"`},
		{name: "two objects on header line", data: header + " " + operation + "\n", want: "trailing JSON value"},
		{name: "two objects on operation line", data: header + "\n" + operation + " " + operation + "\n", want: "trailing JSON value"},
		{name: "pretty printed record", data: "{\n\"type\":\"plan\",\"version\":2,\"root\":\"/tmp/root\"}\n", want: "close JSON object"},
		{name: "blank record", data: header + "\n\n" + operation + "\n", want: "blank JSONL records"},
		{name: "non-object record", data: header + "\n[]\n", want: "must be one object"},
		{name: "duplicate operation id", data: header + "\n" + operation + "\n" + operation + "\n", want: `duplicate operation ID "one"`},
		{name: "trailing malformed data", data: header + "\n" + operation + " garbage\n", want: "decode trailing JSON"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if plan, err := ReadPlan(strings.NewReader(test.data)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("plan=%#v error=%v, want %q", plan, err, test.want)
			}
		})
	}
}

func TestReadPlanAcceptsCRLFAndFinalLineWithoutNewline(t *testing.T) {
	t.Parallel()
	data := "{\"type\":\"plan\",\"version\":1,\"root\":\"/tmp/root\"}\r\n" +
		"{\"type\":\"operation\",\"id\":\"touch\",\"action\":\"touch\",\"source\":\"new\"}"
	plan, err := ReadPlan(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Header.Version != legacyPlanVersion || len(plan.Operations) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestReadPlanLimitedAcceptsExactLimitAndRejectsByteNPlusOne(t *testing.T) {
	var base bytes.Buffer
	plan := Plan{
		Header:     PlanHeader{Version: PlanVersion, Root: "/tmp/root"},
		Operations: []Operation{{ID: "touch", Action: ActionTouch, Source: "new"}},
	}
	if err := WritePlan(&base, plan); err != nil {
		t.Fatal(err)
	}
	data := bytes.TrimSuffix(base.Bytes(), []byte{'\n'})
	padding := int(MaxPlanBytes) - len(data) - 1
	if padding < 0 {
		t.Fatalf("base plan is larger than limit: %d", len(data))
	}
	exact := make([]byte, 0, MaxPlanBytes+1)
	exact = append(exact, data...)
	exact = append(exact, bytes.Repeat([]byte{' '}, padding)...)
	exact = append(exact, '\n')
	if int64(len(exact)) != MaxPlanBytes {
		t.Fatalf("exact payload size = %d", len(exact))
	}
	if _, err := ReadPlanLimited(bytes.NewReader(exact), MaxPlanBytes); err != nil {
		t.Fatalf("exact-limit plan rejected: %v", err)
	}
	over := append(exact, ' ')
	if _, err := ReadPlanLimited(bytes.NewReader(over), MaxPlanBytes); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("N+1 error = %v", err)
	}
}

func TestApplyPrevalidatesCompletePlanBeforeMutation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	created := filepath.Join(root, "would-have-been-created")
	missing := filepath.Join(root, "missing")
	plan := Plan{
		Header: PlanHeader{Version: PlanVersion, Root: root},
		Operations: []Operation{
			{ID: "valid-prefix", Action: ActionTouch, Source: created},
			{ID: "invalid-tail", Action: ActionDelete, Source: missing},
		},
	}
	results, err := Apply(context.Background(), plan, ApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), `operation 2 ("invalid-tail")`) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	if len(results) != 1 || results[0].OperationID != "invalid-tail" || results[0].Status != ResultStatusError || results[0].MayHaveMutated {
		t.Fatalf("results = %#v", results)
	}
	if _, statErr := os.Lstat(created); !os.IsNotExist(statErr) {
		t.Fatalf("valid prefix mutated source: %v", statErr)
	}
	if _, statErr := os.Lstat(DefaultAuditPath(root)); !os.IsNotExist(statErr) {
		t.Fatalf("prevalidation created audit log: %v", statErr)
	}
}

func TestApplyRejectsUnverifiableGeneratedArchiveDependencyBeforeMutation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	generated := filepath.Join(root, "generated.tar")
	output := filepath.Join(root, "output")
	plan := Plan{
		Header: PlanHeader{Version: PlanVersion, Root: root},
		Operations: []Operation{
			{ID: "create-empty", Action: ActionTouch, Source: generated},
			{ID: "extract-generated", Action: ActionExtract, Source: generated, Destination: output, Format: archiveFormatTar},
		},
	}
	results, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true})
	if err == nil || !strings.Contains(err.Error(), "cannot be validated before mutation") {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	for _, path := range []string{generated, output} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("invalid generated-archive dependency mutated %q: %v", path, statErr)
		}
	}
}

func TestApplyRejectsDuplicateIDsAndInvalidDependenciesBeforeMutation(t *testing.T) {
	t.Parallel()
	t.Run("duplicate IDs", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
		plan := Plan{
			Header: PlanHeader{Version: PlanVersion, Root: root},
			Operations: []Operation{
				{ID: "duplicate", Action: ActionTouch, Source: first},
				{ID: "duplicate", Action: ActionTouch, Source: second},
			},
		}
		if _, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true}); err == nil || !strings.Contains(err.Error(), "duplicate operation ID") {
			t.Fatalf("error = %v", err)
		}
		for _, path := range []string{first, second} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("duplicate-ID plan mutated %q: %v", path, err)
			}
		}
	})

	t.Run("use after recursive delete", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		parent := filepath.Join(root, "parent")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		child := filepath.Join(parent, "child")
		if err := os.WriteFile(child, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		plan := testPlan(root,
			Operation{ID: "delete-parent", Action: ActionDelete, Source: parent, Recursive: true},
			Operation{ID: "copy-child", Action: ActionCopy, Source: child, Destination: filepath.Join(root, "copy")},
		)
		if _, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true}); err == nil || !strings.Contains(err.Error(), "invalid dependency") {
			t.Fatalf("error = %v", err)
		}
		assertTestFile(t, child, "keep")
		if _, err := os.Lstat(filepath.Join(root, "copy")); !os.IsNotExist(err) {
			t.Fatalf("invalid dependency created destination: %v", err)
		}
	})

	t.Run("stale guard after prior mutation", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == windowsOS {
			t.Skip("chmod is unsupported on Windows; the platform contract is covered by capability tests")
		}
		root := t.TempDir()
		path := filepath.Join(root, "file")
		if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		expected, err := fsinfo.Inspect(path, false)
		if err != nil {
			t.Fatal(err)
		}
		size, mode := int64(1), uint32(0o640)
		plan := Plan{
			Header: PlanHeader{Version: PlanVersion, Root: root},
			Operations: []Operation{
				{ID: "truncate", Action: ActionTruncate, Source: path, Expected: &expected, Size: &size},
				{ID: "chmod", Action: ActionChmod, Source: path, Expected: &expected, Mode: &mode},
			},
		}
		if _, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true}); err == nil || !strings.Contains(err.Error(), "source guard was captured before") {
			t.Fatalf("error = %v", err)
		}
		assertTestFile(t, path, "original")
	})
}

func FuzzReadPlanStrict(f *testing.F) {
	var valid bytes.Buffer
	if err := WritePlan(&valid, Plan{
		Header:     PlanHeader{Version: PlanVersion, Root: "/tmp/root"},
		Operations: []Operation{{ID: "touch", Action: ActionTouch, Source: "new"}},
	}); err != nil {
		f.Fatal(err)
	}
	for _, seed := range [][]byte{
		valid.Bytes(),
		[]byte(`{"type":"plan","version":2,"root":"/tmp/root","unknown":true}` + "\n"),
		[]byte("{} {}\n"),
		[]byte("\n"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		plan, err := ReadPlan(bytes.NewReader(data))
		if err != nil {
			return
		}
		var encoded bytes.Buffer
		if err := WritePlan(&encoded, plan); err != nil {
			t.Fatalf("accepted plan could not be encoded: %v", err)
		}
		roundTrip, err := ReadPlan(&encoded)
		if err != nil {
			t.Fatalf("encoded accepted plan was rejected: %v", err)
		}
		if roundTrip.Header.Version != plan.Header.Version || len(roundTrip.Operations) != len(plan.Operations) {
			t.Fatalf("round trip changed plan: before=%#v after=%#v", plan, roundTrip)
		}
	})
}

func FuzzPlanPrevalidationDoesNotMutate(f *testing.F) {
	for _, seed := range [][]byte{{0}, {1, 2}, {3, 3}, {4, 1, 0}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, shape []byte) {
		if len(shape) > 8 {
			shape = shape[:8]
		}
		root := t.TempDir()
		existing := filepath.Join(root, "existing")
		if err := os.WriteFile(existing, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		expected, err := fsinfo.Inspect(existing, false)
		if err != nil {
			t.Fatal(err)
		}
		operations := make([]Operation, 0, len(shape))
		for index, value := range shape {
			id := fmt.Sprintf("op-%d", index)
			if value%7 == 6 {
				id = "duplicate"
			}
			switch value % 6 {
			case 0:
				operations = append(operations, Operation{ID: id, Action: ActionTouch, Source: filepath.Join(root, fmt.Sprintf("new-%d", index))})
			case 1:
				operations = append(operations, Operation{ID: id, Action: ActionDelete, Source: existing, Expected: &expected})
			case 2:
				operations = append(operations, Operation{ID: id, Action: ActionCopy, Source: existing, Destination: filepath.Join(root, fmt.Sprintf("copy-%d", index)), Expected: &expected})
			case 3:
				operations = append(operations, Operation{ID: id, Action: Action("unknown"), Source: existing, Expected: &expected})
			case 4:
				operations = append(operations, Operation{ID: id, Action: ActionDelete, Source: filepath.Join(root, fmt.Sprintf("missing-%d", index))})
			case 5:
				operations = append(operations, Operation{ID: id, Action: ActionMkdir, Source: filepath.Join(root, fmt.Sprintf("dir-%d", index))})
			}
		}
		plan := Plan{Header: PlanHeader{Version: PlanVersion, Root: root}, Operations: operations}
		_ = prevalidatePlan(context.Background(), root, plan, ConflictFail, false, defaultMutationFilesystem())
		assertTestFile(t, existing, "unchanged")
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "existing" {
			t.Fatalf("prevalidation mutated root: %v", entries)
		}
	})
}
