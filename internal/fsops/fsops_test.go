package fsops

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
)

func TestPlanJSONLRoundTrip(t *testing.T) {
	t.Parallel()
	mode := uint32(0o700)
	want := Plan{
		Header:     PlanHeader{Version: PlanVersion, Root: "/tmp/root", CreatedAt: time.Unix(10, 0).UTC()},
		Operations: []Operation{{ID: "one", Action: ActionMkdir, Source: "new", Mode: &mode}},
	}
	var buf bytes.Buffer
	if err := WritePlan(&buf, want); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1; lines != 2 {
		t.Fatalf("got %d JSONL records", lines)
	}
	got, err := ReadPlan(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.Type != "plan" || len(got.Operations) != 1 || got.Operations[0].Type != "operation" {
		t.Fatalf("unexpected plan: %#v", got)
	}
}

func TestResultJSONLRoundTrip(t *testing.T) {
	t.Parallel()
	want := []Result{{OperationID: "one", Action: ActionDelete, Status: "ok"}, {OperationID: "two", Action: ActionCopy, Status: "error", Error: "failed"}}
	var buf bytes.Buffer
	if err := WriteResults(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResults(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Version != PlanVersion || got[1].Error != "failed" {
		t.Fatalf("unexpected results: %#v", got)
	}
}

func TestApplyRejectsStaleEntry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := fsinfo.Inspect(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(root, Operation{ID: "delete", Action: ActionDelete, Source: path, Expected: &expected})
	results, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true})
	if err == nil || !strings.Contains(err.Error(), "stale source: size changed") {
		t.Fatalf("got results=%v err=%v", results, err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("stale source was mutated: %v", statErr)
	}
}

func TestApplyRejectsEscapingSymlinkParent(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	plan := testPlan(root, Operation{ID: "touch", Action: ActionTouch, Source: "escape/file"})
	_, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true})
	if err == nil || !strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "file")); !os.IsNotExist(err) {
		t.Fatalf("outside file exists: %v", err)
	}
}

func TestApplyAcceptsCanonicalAliasOfPlanRoot(t *testing.T) {
	t.Parallel()
	realRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(alias, "file")
	plan := Plan{Header: PlanHeader{Version: PlanVersion, Root: alias}, Operations: []Operation{{ID: "touch", Action: ActionTouch, Source: path}}}
	if _, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(realRoot, "file")); err != nil {
		t.Fatalf("canonical target was not created: %v", err)
	}
}

func TestApplyDoesNotFollowFinalSymlinkForTruncate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target, link := filepath.Join(root, "target"), filepath.Join(root, "link")
	if err := os.WriteFile(target, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	size := int64(0)
	_, err := Apply(context.Background(), testPlan(root, Operation{ID: "truncate", Action: ActionTruncate, Source: link, Size: &size}), ApplyOptions{DisableAudit: true})
	if err == nil || !strings.Contains(err.Error(), "refusing to follow final symlink") {
		t.Fatalf("got %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" {
		t.Fatalf("target changed to %q", data)
	}
}

func TestApplyConflictFailsByDefault(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src, dst := filepath.Join(root, "source"), filepath.Join(root, "destination")
	if err := os.WriteFile(src, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("destination"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Apply(context.Background(), testPlan(root, Operation{ID: "copy", Action: ActionCopy, Source: src, Destination: dst}), ApplyOptions{DisableAudit: true})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("got %v", err)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "destination" {
		t.Fatalf("destination changed to %q", data)
	}
}

func TestOverwriteRestoresDestinationWhenReplacementFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	destination := filepath.Join(dir, "destination")
	if err := os.WriteFile(destination, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("replacement failed")
	if err := withDestination(destination, ConflictOverwrite, func() error {
		if err := os.WriteFile(destination, []byte("partial"), 0o600); err != nil {
			return err
		}
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "original" {
		t.Fatalf("destination = %q, %v", data, err)
	}
}

func TestApplyRequiresGuardForExistingSource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := Plan{Header: PlanHeader{Version: PlanVersion, Root: root}, Operations: []Operation{{ID: "delete", Action: ActionDelete, Source: path}}}
	if _, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true}); err == nil || !strings.Contains(err.Error(), "expected metadata") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unguarded source changed: %v", err)
	}
}

func TestOpenAuditRejectsSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target, link := filepath.Join(dir, "target"), filepath.Join(dir, "audit")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if f, err := OpenAudit(link); err == nil {
		_ = f.Close()
		t.Fatal("audit symlink was accepted")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "keep" {
		t.Fatalf("audit target changed to %q", data)
	}
}

func TestApplyRefusesToDeleteOrRelocatePlanRoot(t *testing.T) {
	t.Parallel()
	for _, action := range []Action{ActionDelete, ActionMove, ActionRename} {
		action := action
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			op := Operation{ID: string(action), Action: action, Source: root, Recursive: true}
			if action != ActionDelete {
				op.Destination = filepath.Join(root, "moved")
			}
			_, err := Apply(context.Background(), testPlan(root, op), ApplyOptions{DisableAudit: true})
			if err == nil || !strings.Contains(err.Error(), "plan root") {
				t.Fatalf("got %v", err)
			}
			if _, err := os.Stat(root); err != nil {
				t.Fatalf("root changed: %v", err)
			}
		})
	}
}

func TestRecursiveDeleteMustBeExplicit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "directory")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(root, Operation{ID: "delete", Action: ActionDelete, Source: dir})
	if _, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true}); err == nil {
		t.Fatal("non-recursive directory deletion unexpectedly succeeded")
	}
	plan.Operations[0].Recursive = true
	if _, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("directory still exists: %v", err)
	}
}

func TestOperationModePreservesSpecialBits(t *testing.T) {
	t.Parallel()
	mode := operationMode(0o7754)
	if mode.Perm() != 0o754 || mode&os.ModeSetuid == 0 || mode&os.ModeSetgid == 0 || mode&os.ModeSticky == 0 {
		t.Fatalf("mode = %v (%#o)", mode, mode)
	}
}

func TestApplyChainedCreateTruncateChmodAndRename(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "new")
	file := filepath.Join(dir, "file")
	renamed := filepath.Join(dir, "renamed")
	size := int64(4096)
	mode := uint32(0o640)
	plan := Plan{
		Header: PlanHeader{Version: PlanVersion, Root: root},
		Operations: []Operation{
			{ID: "mkdir", Action: ActionMkdir, Source: dir},
			{ID: "touch", Action: ActionTouch, Source: file},
			{ID: "truncate", Action: ActionTruncate, Source: file, Size: &size},
			{ID: "chmod", Action: ActionChmod, Source: file, Mode: &mode},
			{ID: "rename", Action: ActionRename, Source: file, Destination: renamed},
		},
	}
	results, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true})
	if err != nil || len(results) != len(plan.Operations) {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	info, err := os.Stat(renamed)
	if err != nil || info.Size() != size || runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("renamed info=%v err=%v", info, err)
	}
}

func TestDryRunPerformsConflictAndArchivePreflight(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	if err := os.WriteFile(source, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(root, Operation{ID: "copy", Action: ActionCopy, Source: source, Destination: destination})
	if _, err := Apply(context.Background(), plan, ApplyOptions{DryRun: true, DisableAudit: true}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("dry-run conflict error = %v", err)
	}
	bad := filepath.Join(root, "bad.tar")
	if err := os.WriteFile(bad, []byte("not a tar"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan = testPlan(root, Operation{ID: "extract", Action: ActionExtract, Source: bad, Destination: filepath.Join(root, "out"), Format: "tar"})
	if _, err := Apply(context.Background(), plan, ApplyOptions{DryRun: true, DisableAudit: true}); err == nil {
		t.Fatal("dry-run accepted corrupt archive")
	}
	data, _ := os.ReadFile(destination)
	if string(data) != "keep" {
		t.Fatalf("dry-run changed destination to %q", data)
	}
}

func TestCopyRejectsDestinationInsideSource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Apply(context.Background(), testPlan(root, Operation{ID: "copy", Action: ActionCopy, Source: source, Destination: filepath.Join(source, "child")}), ApplyOptions{DisableAudit: true})
	if err == nil || !strings.Contains(err.Error(), "inside source") {
		t.Fatalf("got %v", err)
	}
}

func TestApplyDryRunAndDefaultAudit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "file")
	results, err := Apply(context.Background(), testPlan(root, Operation{ID: "touch", Action: ActionTouch, Source: path}), ApplyOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].DryRun || results[0].Status != "ok" {
		t.Fatalf("unexpected results: %#v", results)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("dry-run created source: %v", err)
	}
	audit := DefaultAuditPath(root)
	info, err := os.Stat(audit)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("audit mode = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	var result Result
	if err := json.Unmarshal(bytes.TrimSpace(data), &result); err != nil {
		t.Fatal(err)
	}
	if result.Type != "result" || result.Version != PlanVersion {
		t.Fatalf("unexpected audit: %#v", result)
	}
}

func TestArchiveExtractRoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	archive, extracted := filepath.Join(root, "data.tar.gz"), filepath.Join(root, "out")
	plan := testPlan(root,
		Operation{ID: "archive", Action: ActionArchive, Source: source, Destination: archive},
		Operation{ID: "extract", Action: ActionExtract, Source: archive, Destination: extracted},
	)
	if _, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(extracted, "source", "file"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("got %q", data)
	}
}

func TestZipArchiveExtractRoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("zip payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	archive, extracted := filepath.Join(root, "data.zip"), filepath.Join(root, "out")
	plan := testPlan(root, Operation{ID: "archive", Action: ActionArchive, Source: source, Destination: archive}, Operation{ID: "extract", Action: ActionExtract, Source: archive, Destination: extracted})
	if _, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(extracted, "source", "file"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "zip payload" {
		t.Fatalf("got %q", data)
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	archive := filepath.Join(root, "bad.tar")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "../outside", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Apply(context.Background(), testPlan(root, Operation{ID: "extract", Action: ActionExtract, Source: archive, Destination: "out"}), ApplyOptions{DisableAudit: true})
	if err == nil || !strings.Contains(err.Error(), "unsafe archive path") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "outside")); !os.IsNotExist(err) {
		t.Fatalf("traversal wrote outside: %v", err)
	}
}

func testPlan(root string, ops ...Operation) Plan {
	for i := range ops {
		if ops[i].Expected != nil {
			continue
		}
		path := ops[i].Source
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		if entry, err := fsinfo.Inspect(path, false); err == nil {
			ops[i].Expected = &entry
		}
	}
	return Plan{Header: PlanHeader{Version: PlanVersion, Root: root}, Operations: ops}
}
