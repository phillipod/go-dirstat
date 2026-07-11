//go:build windows

package fsops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPOSIXOperationsAreRejectedBeforeMutation(t *testing.T) {
	t.Parallel()
	mode := uint32(0o700)
	uid := 1
	tests := []struct {
		name string
		op   func(string, string) Operation
	}{
		{
			name: "chmod",
			op: func(_ string, existing string) Operation {
				return Operation{ID: "chmod", Action: ActionChmod, Source: existing, Mode: &mode}
			},
		},
		{
			name: "chown",
			op: func(_ string, existing string) Operation {
				return Operation{ID: "chown", Action: ActionChown, Source: existing, UID: &uid}
			},
		},
		{
			name: "mkdir mode",
			op: func(root, _ string) Operation {
				return Operation{ID: "mkdir", Action: ActionMkdir, Source: filepath.Join(root, "created"), Mode: &mode}
			},
		},
		{
			name: "touch mode",
			op: func(root, _ string) Operation {
				return Operation{ID: "touch", Action: ActionTouch, Source: filepath.Join(root, "created"), Mode: &mode}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			existing := filepath.Join(root, "existing")
			if err := os.WriteFile(existing, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			op := test.op(root, existing)
			results, err := Apply(context.Background(), testPlan(root, op), ApplyOptions{DisableAudit: true})
			if err == nil || !strings.Contains(err.Error(), "unsupported on windows") {
				t.Fatalf("results=%#v error=%v", results, err)
			}
			data, readErr := os.ReadFile(existing)
			if readErr != nil || string(data) != "unchanged" {
				t.Fatalf("existing file changed: data=%q error=%v", data, readErr)
			}
			if _, statErr := os.Stat(filepath.Join(root, "created")); !os.IsNotExist(statErr) {
				t.Fatalf("created path exists: %v", statErr)
			}
		})
	}
}
