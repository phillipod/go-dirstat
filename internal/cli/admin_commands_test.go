package cli

import (
	"bytes"
	"context"
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
	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/history"
	querypkg "github.com/phillipod/go-dirstat/internal/query"
)

const windowsOS = "windows"

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
	if runtime.GOOS != windowsOS {
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
	if runtime.GOOS != windowsOS && !strings.Contains(tsv, `line\nbreak\tfile`) {
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

func TestQueryFollowUsesTargetMetadata(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("hosted Windows link capability is covered in the native lane")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	alias := filepath.Join(root, "alias")
	if err := os.WriteFile(target, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	out, err := executeCLI("--follow", "query", "--metadata", "--kind=file", "--fields=relative,mode-text", "--sort=name", root)
	if err != nil {
		t.Fatal(err)
	}
	foundAlias := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) == 2 && fields[0] == "alias" {
			foundAlias = true
			if fields[1] != "-rw-------" {
				t.Fatalf("followed alias metadata = %q, want target mode", line)
			}
		}
	}
	if !foundAlias {
		t.Fatalf("followed alias missing from query output: %q", out)
	}
}

func TestQueryFollowUsesTargetMetadataForDirectoryAlias(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("hosted Windows link capability is covered in the native lane")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target-dir")
	alias := filepath.Join(root, "alias-dir")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "payload"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	out, err := executeCLI("--follow", "query", "--metadata", "--kind=directory", "--fields=relative,mode-text", "--sort=name", root)
	if err != nil {
		t.Fatal(err)
	}
	foundAlias := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) == 2 && fields[0] == "alias-dir" {
			foundAlias = true
			if fields[1] != "drwx------" {
				t.Fatalf("followed directory alias metadata = %q, want target mode", line)
			}
		}
	}
	if !foundAlias {
		t.Fatalf("followed directory alias missing from query output: %q", out)
	}
}

func TestQueryFollowDanglingAliasFailsAsIncompleteBeforeOutput(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("hosted Windows link capability is covered in the native lane")
	}
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "missing-target"), filepath.Join(root, "dangling")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	output, err := executeCLI("--follow", "query", "--kind=file", root)
	if ExitCode(err) != ExitScanIncomplete || output != "" {
		t.Fatalf("dangling followed query output=%q error=%v exit=%d", output, err, ExitCode(err))
	}
}

func TestQueryOwnershipCapabilityRejectsUnsupportedRequests(t *testing.T) {
	fields, _, err := parseQueryFields("path,owner")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		flags  queryFlags
		fields []queryOutputField
		sorts  []querypkg.SortKey
	}{
		{flags: queryFlags{owners: []string{"alice"}}},
		{fields: fields},
		{sorts: []querypkg.SortKey{{Field: querypkg.SortGroup}}},
	}
	for _, test := range tests {
		if err := validateQueryOwnershipCapability(false, test.flags, test.fields, test.sorts); err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("unsupported ownership request error = %v", err)
		}
	}
	if err := validateQueryOwnershipCapability(true, queryFlags{owners: []string{"alice"}}, fields, nil); err != nil {
		t.Fatalf("available ownership rejected: %v", err)
	}
}

func TestQueryLimitAndUnsortedStreaming(t *testing.T) {
	root := t.TempDir()
	for name, size := range map[string]int{"small": 1, "largest": 30, "middle": 20} {
		if err := os.WriteFile(filepath.Join(root, name), bytes.Repeat([]byte{'x'}, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sorted, err := executeCLI("--apparent", "query", "--kind=file", "--fields=relative", "--sort=size:desc", "--limit=2", root)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSpace(sorted), "\n"); len(got) != 2 || got[0] != "largest" || got[1] != "middle" {
		t.Fatalf("bounded sorted query = %q", sorted)
	}
	streamed, err := executeCLI("query", "--stream", "--kind=file", "--fields=relative", "--limit=2", root)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSpace(streamed), "\n"); len(got) != 2 || got[0] != "largest" || got[1] != "middle" {
		t.Fatalf("bounded stream order = %q", streamed)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := executeCLI("query", "--limit=-1", missing); err == nil || !strings.Contains(err.Error(), "zero or greater") {
		t.Fatalf("negative limit did not fail before scan: %v", err)
	}
	if _, err := executeCLI("query", "--stream", "--sort=name", missing); err == nil || !strings.Contains(err.Error(), "cannot be used") {
		t.Fatalf("stream sort did not fail before scan: %v", err)
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

func TestDiagnoseGroupsOpenDeletedObjectsAndReportsUniqueTotal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("open-deleted object reporting requires Linux /proc")
	}
	root := t.TempDir()
	path := filepath.Join(root, "held.log")
	first, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if _, err := first.Write(bytes.Repeat([]byte{'x'}, 4096)); err != nil {
		t.Fatal(err)
	}
	second, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	encoded, err := executeCLI("diagnose", "--format=json", root)
	if err != nil {
		t.Fatal(err)
	}
	var result diagnose.Result
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != diagnose.SchemaVersion || len(result.OpenDeleted) != 1 || result.OpenDeletedSummary == nil {
		t.Fatalf("diagnostics = %#v", result)
	}
	file := result.OpenDeleted[0]
	if file.Path != path || file.Device == 0 || file.Inode == 0 || len(file.Holders) != 1 || len(file.Holders[0].Descriptors) < 2 {
		t.Fatalf("grouped open-deleted object = %#v", file)
	}
	if result.OpenDeletedSummary.Objects != 1 || result.OpenDeletedSummary.Descriptors < 2 ||
		result.OpenDeletedSummary.ReclaimableBytes != file.Allocated {
		t.Fatalf("summary = %#v", result.OpenDeletedSummary)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		t.Fatal(err)
	}
	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(payload["open_deleted"], &objects); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"device", "inode", "size_bytes", "allocated_bytes", "holders"} {
		if _, ok := objects[0][key]; !ok {
			t.Fatalf("open_deleted JSON missing %q: %s", key, encoded)
		}
	}
	for _, oldKey := range []string{"pid", "descriptor"} {
		if _, ok := objects[0][oldKey]; ok {
			t.Fatalf("schema v2 open_deleted JSON retained top-level %q: %s", oldKey, encoded)
		}
	}
	var summary map[string]json.RawMessage
	if err := json.Unmarshal(payload["open_deleted_summary"], &summary); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"reclaimable_bytes", "logical_bytes", "allocated_bytes", "coverage"} {
		if _, ok := summary[key]; !ok {
			t.Fatalf("open_deleted_summary JSON missing %q: %s", key, encoded)
		}
	}

	text, err := executeCLI("diagnose", "--bytes", root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"reclaimable", "logical", "dev=", "fds=", "Open-deleted coverage:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("diagnostic text missing %q:\n%s", want, text)
		}
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
	var volumes []fsinfo.Volume
	if err := json.Unmarshal([]byte(encoded), &volumes); err != nil || len(volumes) != 1 ||
		volumes[0].Path == "" || volumes[0].ResolvedPath == "" || volumes[0].Total == 0 ||
		volumes[0].PhysicalUsed != volumes[0].Used || volumes[0].CallerCapacity == 0 {
		t.Fatalf("JSON status = %q, %#v, %v", encoded, volumes, err)
	}
}

func TestStatusJSONIsOneDocumentForMultiplePaths(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	encoded, err := executeCLI("status", "--format=json", first, second)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	var volumes []map[string]any
	if err := decoder.Decode(&volumes); err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 2 {
		t.Fatalf("status volumes = %#v", volumes)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing JSON decode = %v, want EOF; output %q", err, encoded)
	}

	jsonl, err := executeCLI("status", "--format=jsonl", first, second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.TrimSpace(jsonl), "\n") != 1 {
		t.Fatalf("JSONL status = %q, want two records", jsonl)
	}
}

func TestStatusErrorDoesNotEmitValidPrefix(t *testing.T) {
	root := t.TempDir()
	out, err := executeCLI("status", "--format=json", root, filepath.Join(root, "missing"))
	if err == nil {
		t.Fatal("status accepted a missing second path")
	}
	if out != "" {
		t.Fatalf("status emitted a valid prefix before error: %q", out)
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
	if _, err := executeCLI("inspect", "--tail", path); err == nil || !strings.Contains(err.Error(), "requires --content") {
		t.Fatalf("tail without content error = %v", err)
	}
	if _, err := executeCLI("inspect", "--tail", filepath.Join(t.TempDir(), "missing")); err == nil ||
		!strings.Contains(err.Error(), "requires --content") {
		t.Fatalf("tail validation did not precede missing-path access: %v", err)
	}
}

func TestInspectTextEscapesTerminalControlsAndOffersExplicitRawContent(t *testing.T) {
	root := t.TempDir()
	name := "unsafe-file"
	if runtime.GOOS != windowsOS {
		name = "unsafe\x1b]0;title\a\nfile"
	}
	path := filepath.Join(root, name)
	content := []byte("line\tred\x1b[31m\nosc\x1b]0;title\a")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := executeCLI("inspect", "--content", path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(text, '\x1b') || strings.ContainsRune(text, '\a') || !strings.Contains(text, `\x1B`) || !strings.Contains(text, `\n`) || !strings.Contains(text, `\t`) {
		t.Fatalf("unsafe inspect text = %q", text)
	}
	if runtime.GOOS != windowsOS && !strings.Contains(text, `unsafe\x1B]0;title\x07\nfile`) {
		t.Fatalf("unsafe filename was not visibly escaped: %q", text)
	}

	raw, err := executeCLIBytes("inspect", "--content", "--raw-content", path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, content) {
		t.Fatalf("raw preview != original bytes: %q", raw)
	}
	if _, err := executeCLI("inspect", "--format=json", "--content", "--raw-content", path); err == nil {
		t.Fatal("raw-content was accepted with JSON")
	}
}

func TestInspectJSONPreservesInvalidUTF8AndTailBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binary")
	content := []byte{'a', 'b', 0xff, 0x00, 'c'}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	encoded, err := executeCLI("inspect", "--format=json", "--content", "--tail", "--limit=3", path)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Preview struct {
			Data   []byte `json:"data"`
			Binary bool   `json:"binary"`
			Offset int64  `json:"offset"`
		} `json:"preview"`
	}
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Preview.Binary || result.Preview.Offset != 2 || !bytes.Equal(result.Preview.Data, []byte{0xff, 0x00, 'c'}) {
		t.Fatalf("lossless tail preview = %#v", result.Preview)
	}
}

func TestHistoryGrowthRecordsBaselineThenComparesAndLists(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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

func TestHistoryMigrationFingerprintMatchesDestinationStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root, source, destination := t.TempDir(), filepath.Join(t.TempDir(), "legacy"), filepath.Join(t.TempDir(), "durable")
	file := filepath.Join(root, "payload")
	if err := os.WriteFile(file, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := executeCLI("history", "--store", source, "growth", "--format=json", root); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, ".dirstat-store")); err != nil {
		t.Fatal(err)
	}
	legacy, err := history.OpenStoreAtWithPolicy(source, history.MaxRecords, history.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if adopted, err := legacy.AdoptContext(context.Background(), false); err != nil || !adopted {
		t.Fatalf("adopt legacy source: adopted=%t error=%v", adopted, err)
	}
	dest, err := history.NewStoreAtWithPolicy(destination, history.MaxRecords, history.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	actions, err := legacy.MigrateTo(dest, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || !actions[0].Removed {
		t.Fatalf("migration actions = %#v", actions)
	}
	if err := os.WriteFile(file, bytes.Repeat([]byte{'b'}, 8193), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := executeCLI("history", "--store", destination, "growth", "--format=json", root)
	if err != nil {
		t.Fatal(err)
	}
	var growth struct {
		Baseline bool            `json:"baseline"`
		Changes  []history.Delta `json:"changes"`
	}
	if err := json.Unmarshal([]byte(result), &growth); err != nil {
		t.Fatal(err)
	}
	if growth.Baseline || !hasGrowthFor(growth.Changes, file) {
		t.Fatalf("destination history did not reuse migrated baseline: %#v", growth)
	}
}

func TestHistoryGrowthOperationalFiltersSuppressAncestorDoubleCounting(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root, store := t.TempDir(), t.TempDir()
	file := filepath.Join(root, "sub", "growing.log")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := executeCLI("history", "--store", store, "growth", "--format=json", root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, bytes.Repeat([]byte{'b'}, 8193), 0o600); err != nil {
		t.Fatal(err)
	}
	leafJSON, err := executeCLI("history", "--store", store, "growth", "--format=json",
		"--leaf-only", "--kind=file", "--depth=2", "--limit=1", root)
	if err != nil {
		t.Fatal(err)
	}
	var leaf struct {
		Changes []history.Delta `json:"changes"`
	}
	if err := json.Unmarshal([]byte(leafJSON), &leaf); err != nil {
		t.Fatal(err)
	}
	if len(leaf.Changes) != 1 || leaf.Changes[0].Path != file || leaf.Changes[0].IsDir || leaf.Changes[0].Depth != 2 {
		t.Fatalf("leaf-only history = %#v", leaf.Changes)
	}

	if err := os.WriteFile(file, bytes.Repeat([]byte{'c'}, 16385), 0o600); err != nil {
		t.Fatal(err)
	}
	directoryJSON, err := executeCLI("history", "--store", store, "growth", "--format=json",
		"--kind=directory", "--depth=0", root)
	if err != nil {
		t.Fatal(err)
	}
	var directories struct {
		Changes []history.Delta `json:"changes"`
	}
	if err := json.Unmarshal([]byte(directoryJSON), &directories); err != nil {
		t.Fatal(err)
	}
	if len(directories.Changes) != 1 || directories.Changes[0].Path != root || !directories.Changes[0].IsDir {
		t.Fatalf("root-only directory history = %#v", directories.Changes)
	}
}

func TestHistoryGrowthRejectsInvalidFiltersBeforeFilesystemAccess(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "missing")
	tests := [][]string{
		{"history", "growth", "--kind=socket", missing},
		{"history", "growth", "--depth=-2", missing},
		{"history", "growth", "--limit=-1", missing},
	}
	for _, args := range tests {
		_, err := executeCLI(args...)
		if err == nil || strings.Contains(err.Error(), "no such file") {
			t.Fatalf("Execute(%q) error = %v, want filter validation", args, err)
		}
	}
}

func TestHistoryMaxConfigurationControlsRetention(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configPath := filepath.Join(configHome, "dirstat", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"history_max":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root, store := t.TempDir(), t.TempDir()
	file := filepath.Join(root, "growing.log")
	for i := 1; i <= 3; i++ {
		if err := os.WriteFile(file, bytes.Repeat([]byte{'x'}, i), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := executeCLI("history", "--store", store, "growth", "--format=json", root); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := executeCLI("history", "--store", store, "list", "--format=json", root)
	if err != nil {
		t.Fatal(err)
	}
	var records []history.Record
	if err := json.Unmarshal([]byte(listed), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("history_max=1 retained %d records: %#v", len(records), records)
	}
}

func TestHistoryGrowthExcludesContainedStore(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, ".history")
	file := filepath.Join(root, "stable.log")
	if err := os.WriteFile(file, []byte("stable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := executeCLI("history", "--store", store, "growth", "--format=json", root); err != nil {
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
	if growth.Baseline || len(growth.Changes) != 0 {
		t.Fatalf("history measured its own contained store: %#v", growth)
	}
}

func TestHistoryGrowthExcludesDefaultStateBelowScanRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, ".local-state"))
	if err := os.WriteFile(filepath.Join(root, "stable.log"), []byte("stable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := executeCLI("history", "growth", "--format=json", root); err != nil {
		t.Fatal(err)
	}
	second, err := executeCLI("history", "growth", "--format=json", root)
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
	if growth.Baseline || len(growth.Changes) != 0 {
		t.Fatalf("history measured default state below root: %#v", growth)
	}
}

func TestHistoryGrowthExcludesStateThroughSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, ".local-state"))
	if err := os.WriteFile(filepath.Join(root, "stable.log"), []byte("stable"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "root-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	if _, err := executeCLI("--follow", "history", "growth", "--format=json", alias); err != nil {
		t.Fatal(err)
	}
	second, err := executeCLI("--follow", "history", "growth", "--format=json", alias)
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
	if growth.Baseline || len(growth.Changes) != 0 {
		t.Fatalf("history measured state through symlinked root: %#v", growth)
	}
}

func TestHistoryRejectsStoreAsScanRoot(t *testing.T) {
	root := t.TempDir()
	_, err := executeCLI("history", "--store", root, "growth", root)
	if err == nil || !strings.Contains(err.Error(), "must not be the scan root") {
		t.Fatalf("error = %v, want store/root rejection", err)
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
