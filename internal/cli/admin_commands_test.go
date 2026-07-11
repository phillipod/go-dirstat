package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/phillipod/go-dirstat/internal/diagnose"
	"github.com/phillipod/go-dirstat/internal/history"
	querypkg "github.com/phillipod/go-dirstat/internal/query"
)

func TestQueryTSVSelectableHumanAndRawFields(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "small.log"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large.log"), bytes.Repeat([]byte{'x'}, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := executeCLI("--apparent", "--min-size=1K", "query", "--kind=file", "--extension=log",
		"--fields=relative,size,size-human,apparent,allocated", "--sort=name", root)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) != 5 || fields[0] != "large.log" || fields[1] != "2048" || fields[2] != "2.00K" || fields[3] != "2048" {
		t.Fatalf("query TSV = %q", out)
	}
}

func TestQueryMachineFormatsAndControlSafeNames(t *testing.T) {
	root := t.TempDir()
	name := "line-break"
	if runtime.GOOS != "windows" {
		name = "line\nbreak\tfile"
	}
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tsv, err := executeCLI("--apparent", "query", "--kind=file", "--fields=relative", root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(tsv, "\n") != 1 {
		t.Fatalf("filesystem controls broke TSV records: %q", tsv)
	}
	if runtime.GOOS != "windows" && !strings.Contains(tsv, `line\nbreak\tfile`) {
		t.Fatalf("TSV did not visibly escape controls: %q", tsv)
	}

	jsonl, err := executeCLI("query", "--format=jsonl", "--kind=file", root)
	if err != nil {
		t.Fatal(err)
	}
	var record querypkg.Record
	if err := json.Unmarshal(bytes.TrimSpace([]byte(jsonl)), &record); err != nil {
		t.Fatalf("decode JSONL %q: %v", jsonl, err)
	}
	if record.Path != path {
		t.Fatalf("JSONL path = %q, want %q", record.Path, path)
	}
	projected, err := executeCLI("--apparent", "query", "--format=jsonl", "--kind=file", "--fields=path,size,size-human", root)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(projected)), &values); err != nil {
		t.Fatal(err)
	}
	if values["path"] != path || values["size"] != float64(1) || values["size-human"] != "1B" {
		t.Fatalf("projected JSONL = %#v", values)
	}

	nul, err := executeCLIBytes("query", "--format=nul", "--kind=file", root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(nul, append([]byte(path), 0)) {
		t.Fatalf("NUL output = %q", nul)
	}
}

func TestQueryAgePathAndMetadataFilters(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "logs", "old.log")
	if err := os.Mkdir(filepath.Dir(oldPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.log"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := executeCLI("query", "--kind=file", "--older-than=7d", "--path-glob=logs/*",
		"--path-regexp=old", "--fields=relative,mode-text,links", root)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSpace(out), "\t")
	if len(parts) != 3 || parts[0] != "logs/old.log" || parts[1] == "" || parts[2] == "" {
		t.Fatalf("filtered metadata query = %q", out)
	}
}

func TestQueryRejectsInvalidFlagsBeforeScanning(t *testing.T) {
	tests := [][]string{
		{"query", "--format=csv"},
		{"query", "--format=nul", "--fields=path"},
		{"query", "--fields=path,bogus"},
		{"query", "--kind=socket"},
		{"query", "--sort=size:sideways"},
		{"query", "--sort=bogus"},
		{"query", "--older-than=-1h"},
		{"query", "--newer-than=soon"},
		{"query", "--path-regexp=("},
		{"--min-size=2K", "--max-size=1K", "query"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, err := executeCLI(append(args, filepath.Join(t.TempDir(), "missing"))...)
			if err == nil {
				t.Fatalf("Execute(%q) unexpectedly succeeded", args)
			}
			if strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "cannot find") {
				t.Fatalf("Execute(%q) scanned before validation: %v", args, err)
			}
		})
	}
}

func TestDiagnoseTextAndJSON(t *testing.T) {
	root := t.TempDir()
	text, err := executeCLI("diagnose", "--bytes", root)
	if err != nil {
		t.Fatal(err)
	}
	for _, heading := range []string{"Volumes:\n", "Capabilities:\n", "Open deleted files:\n"} {
		if !strings.Contains(text, heading) {
			t.Fatalf("diagnostic text missing %q:\n%s", heading, text)
		}
	}

	encoded, err := executeCLI("diagnose", "--format=json", root)
	if err != nil {
		t.Fatal(err)
	}
	var result diagnose.Result
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Volumes) != 1 || len(result.Capabilities) == 0 {
		t.Fatalf("diagnostics = %#v", result)
	}
}

func TestStatusSupportsHumanRawAndJSONOutput(t *testing.T) {
	root := t.TempDir()
	human, err := executeCLI("status", root)
	if err != nil || !strings.Contains(human, "available") {
		t.Fatalf("human status = %q, %v", human, err)
	}
	raw, err := executeCLI("status", "--bytes", root)
	if err != nil || !strings.Contains(raw, "B used / ") {
		t.Fatalf("raw status = %q, %v", raw, err)
	}
	encoded, err := executeCLI("status", "--format=json", root)
	if err != nil {
		t.Fatal(err)
	}
	var volume struct {
		Path  string `json:"path"`
		Total uint64 `json:"total_bytes"`
	}
	if err := json.Unmarshal([]byte(encoded), &volume); err != nil || volume.Path == "" || volume.Total == 0 {
		t.Fatalf("JSON status = %q, %#v, %v", encoded, volume, err)
	}
}

func TestInspectPreviewAndLimitValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.txt")
	if err := os.WriteFile(path, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	encoded, err := executeCLI("inspect", "--format=json", "--content", "--tail", "--limit=3", path)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Preview struct {
			Text   string `json:"text"`
			Offset int64  `json:"offset"`
		} `json:"preview"`
	}
	if err := json.Unmarshal([]byte(encoded), &result); err != nil || result.Preview.Text != "def" || result.Preview.Offset != 3 {
		t.Fatalf("inspect = %q, %#v, %v", encoded, result, err)
	}
	if _, err := executeCLI("inspect", "--limit=0", path); err == nil || !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("invalid limit error = %v", err)
	}
}

func TestHistoryGrowthRecordsBaselineThenComparesAndLists(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	file := filepath.Join(root, "growing.log")
	if err := os.WriteFile(file, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := executeCLI("history", "--store", store, "growth", "--format=json", root)
	if err != nil {
		t.Fatal(err)
	}
	var baseline struct {
		Baseline bool            `json:"baseline"`
		Current  history.Record  `json:"current"`
		Changes  []history.Delta `json:"changes"`
	}
	if err := json.Unmarshal([]byte(first), &baseline); err != nil {
		t.Fatal(err)
	}
	if !baseline.Baseline || len(baseline.Changes) != 0 || baseline.Current.Root != root {
		t.Fatalf("baseline = %#v", baseline)
	}
	if err := os.WriteFile(file, bytes.Repeat([]byte{'b'}, 8193), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := executeCLI("history", "--store", store, "growth", "--format=json", root)
	if err != nil {
		t.Fatal(err)
	}
	var growth struct {
		Baseline bool            `json:"baseline"`
		Changes  []history.Delta `json:"changes"`
	}
	if err := json.Unmarshal([]byte(second), &growth); err != nil {
		t.Fatal(err)
	}
	if growth.Baseline || !hasGrowthFor(growth.Changes, file) {
		t.Fatalf("growth = %#v", growth)
	}

	listed, err := executeCLI("history", "--store", store, "list", "--format=json", root)
	if err != nil {
		t.Fatal(err)
	}
	var records []history.Record
	if err := json.Unmarshal([]byte(listed), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ScannedAt.Before(records[1].ScannedAt) {
		t.Fatalf("history list = %#v", records)
	}
}

func TestAdminCommandsReturnWriterErrors(t *testing.T) {
	root := t.TempDir()
	store := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("closed")
	for name, args := range map[string][]string{
		"query":    {"query", root},
		"diagnose": {"diagnose", root},
		"history":  {"history", "--store", store, "growth", root},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := New()
			cmd.SetArgs(args)
			cmd.SetOut(cliErrorWriter{err: want})
			cmd.SetErr(io.Discard)
			if err := cmd.Execute(); !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
		})
	}
}

func executeCLI(args ...string) (string, error) {
	data, err := executeCLIBytes(args...)
	return string(data), err
}

func executeCLIBytes(args ...string) ([]byte, error) {
	cmd := New()
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	return out.Bytes(), err
}

func hasGrowthFor(changes []history.Delta, path string) bool {
	for _, change := range changes {
		if change.Path == path && change.Change == history.ChangeGrown && change.ApparentDelta > 0 {
			return true
		}
	}
	return false
}
