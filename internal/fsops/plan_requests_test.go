package fsops

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadOperationRequestsLimitedStrictMixedActions(t *testing.T) {
	t.Parallel()
	input := "{\"action\":\"delete\",\"source\":\"old\\nname\",\"recursive\":true}\n" +
		"{\"action\":\"copy\",\"source\":\"source\",\"destination\":\"target\"}\n"
	requests, err := ReadOperationRequestsLimited(strings.NewReader(input), int64(len(input)))
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].Action != ActionDelete || requests[0].Source != "old\nname" ||
		!requests[0].Recursive || requests[1].Action != ActionCopy || requests[1].Destination != "target" {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestReadOperationRequestsLimitedRejectsUnsafeFraming(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{name: "empty", want: "input is empty"},
		{name: "blank", input: []byte("\n"), want: "blank JSONL"},
		{name: "unknown ID", input: []byte(`{"action":"delete","source":"x","id":"chosen"}` + "\n"), want: `unknown field "id"`},
		{name: "guard injection", input: []byte(`{"action":"delete","source":"x","expected":{}}` + "\n"), want: `unknown field "expected"`},
		{name: "duplicate", input: []byte(`{"action":"delete","action":"copy","source":"x"}` + "\n"), want: `duplicate field "action"`},
		{name: "trailing", input: []byte(`{"action":"delete","source":"x"} {}` + "\n"), want: "trailing JSON"},
		{name: "invalid UTF-8", input: append([]byte(`{"action":"delete","source":"`), []byte{0xff, '"', '}', '\n'}...), want: "not valid UTF-8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadOperationRequestsLimited(bytes.NewReader(test.input), int64(len(test.input)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReadOperationRequestsLimitedChecksByteNPlusOne(t *testing.T) {
	t.Parallel()
	input := []byte(`{"action":"delete","source":"x"}` + "\n")
	if _, err := ReadOperationRequestsLimited(bytes.NewReader(input), int64(len(input))); err != nil {
		t.Fatalf("exact limit: %v", err)
	}
	if _, err := ReadOperationRequestsLimited(bytes.NewReader(input), int64(len(input)-1)); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("byte N+1 error = %v", err)
	}
}

func TestValidateOperationRequestRejectsInvalidDuplicateShape(t *testing.T) {
	t.Parallel()
	size := int64(1)
	err := ValidateOperationRequest(OperationRequest{Action: ActionDelete, Source: "x", Size: &size})
	if err == nil || !strings.Contains(err.Error(), "size cannot be used with delete") {
		t.Fatalf("error = %v", err)
	}
}

func TestApplyDryRunSupportsStaticallyValidatedDependencyChain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	plan := Plan{
		Header: PlanHeader{Version: PlanVersion, Root: root},
		Operations: []Operation{
			{ID: "mkdir-1", Action: ActionMkdir, Source: filepath.Join(root, "generated")},
			{ID: "touch-2", Action: ActionTouch, Source: filepath.Join(root, "generated", "file")},
			{ID: "copy-3", Action: ActionCopy, Source: filepath.Join(root, "generated", "file"), Destination: filepath.Join(root, "copy")},
		},
	}
	results, err := Apply(context.Background(), plan, ApplyOptions{DryRun: true, DisableAudit: true})
	if err != nil || len(results) != len(plan.Operations) {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	for _, result := range results {
		if result.Status != ResultStatusOK || !result.DryRun || result.MutationCompleted {
			t.Fatalf("result = %#v", result)
		}
	}
}
