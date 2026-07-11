package fsops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
)

func TestPlanV2DestinationGuardAndPartialResultRoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	destination := filepath.Join(root, "destination")
	expected, err := fsinfo.CapturePath(destination)
	if err != nil {
		t.Fatal(err)
	}
	plan := Plan{
		Header: PlanHeader{Version: PlanVersion, Root: root},
		Operations: []Operation{{
			ID: "move", Action: ActionMove, Source: "source", Destination: destination,
			ExpectedDestination: &expected,
		}},
	}
	var encoded bytes.Buffer
	if err := WritePlan(&encoded, plan); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadPlan(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	got := decoded.Operations[0].ExpectedDestination
	if got == nil || got.Exists || got.Path != destination {
		t.Fatalf("destination expectation = %#v", got)
	}

	encoded.Reset()
	wantResult := Result{OperationID: "move", Action: ActionMove, Status: "partial", MayHaveMutated: true, Error: "source cleanup incomplete"}
	if err := WriteResult(&encoded, wantResult); err != nil {
		t.Fatal(err)
	}
	results, err := ReadResults(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Version != PlanVersion || results[0].Status != "partial" || !results[0].MayHaveMutated {
		t.Fatalf("results = %#v", results)
	}
}

func TestReadersAcceptVersionOnePlansAndResults(t *testing.T) {
	t.Parallel()
	planJSON := "{\"type\":\"plan\",\"version\":1,\"root\":\"/tmp/root\"}\n" +
		"{\"type\":\"operation\",\"id\":\"touch\",\"action\":\"touch\",\"source\":\"new\"}\n"
	plan, err := ReadPlan(strings.NewReader(planJSON))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Header.Version != legacyPlanVersion || len(plan.Operations) != 1 {
		t.Fatalf("legacy plan = %#v", plan)
	}
	results, err := ReadResults(strings.NewReader("{\"type\":\"result\",\"version\":1,\"operation_id\":\"touch\",\"action\":\"touch\",\"status\":\"ok\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Version != legacyPlanVersion {
		t.Fatalf("legacy results = %#v", results)
	}
}

func TestOverwriteRequiresDestinationGuard(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	writeTestFile(t, source, "new")
	writeTestFile(t, destination, "old")
	expectedSource, err := fsinfo.Inspect(source, false)
	if err != nil {
		t.Fatal(err)
	}
	plan := Plan{
		Header: PlanHeader{Version: legacyPlanVersion, Root: root},
		Operations: []Operation{{
			ID: "move", Action: ActionMove, Source: source, Destination: destination, Expected: &expectedSource,
		}},
	}
	_, err = Apply(context.Background(), plan, ApplyOptions{Conflict: ConflictOverwrite, DisableAudit: true})
	if err == nil || !strings.Contains(err.Error(), "cannot authorize overwrite") {
		t.Fatalf("error = %v", err)
	}
	assertTestFile(t, source, "new")
	assertTestFile(t, destination, "old")
	plan.Header.Version = PlanVersion
	_, err = Apply(context.Background(), plan, ApplyOptions{Conflict: ConflictOverwrite, DisableAudit: true})
	if err == nil || !strings.Contains(err.Error(), "requires expected destination guard") {
		t.Fatalf("version 2 error = %v", err)
	}
	assertTestFile(t, source, "new")
	assertTestFile(t, destination, "old")
}

func TestOverwriteRejectsDestinationCreatedAfterReview(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	writeTestFile(t, source, "new")
	op := guardedMoveOperation(t, source, destination)
	writeTestFile(t, destination, "concurrent")
	results, err := Apply(
		context.Background(),
		Plan{Header: PlanHeader{Version: PlanVersion, Root: root}, Operations: []Operation{op}},
		ApplyOptions{Conflict: ConflictOverwrite, DisableAudit: true},
	)
	if err == nil || !strings.Contains(err.Error(), "created after review") {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	assertTestFile(t, source, "new")
	assertTestFile(t, destination, "concurrent")
}

func TestOverwriteDetectsDestinationReplacementAtBackupRename(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	writeTestFile(t, source, "new")
	writeTestFile(t, destination, "reviewed")
	op := guardedMoveOperation(t, source, destination)
	filesystem := defaultMutationFilesystem()
	filesystem.rename = func(oldPath, newPath string) error {
		// The first rename in an overwrite is the reviewed destination into a
		// sibling backup. Match the backup naming contract instead of relying on
		// Windows path spelling/casing from EvalSymlinks.
		if strings.HasPrefix(filepath.Base(newPath), ".dirstat-backup-") {
			if err := os.Remove(oldPath); err != nil {
				return err
			}
			if err := os.WriteFile(oldPath, []byte("concurrent replacement"), 0o600); err != nil {
				return err
			}
		}
		return os.Rename(oldPath, newPath)
	}
	results, err := Apply(
		context.Background(),
		Plan{Header: PlanHeader{Version: PlanVersion, Root: root}, Operations: []Operation{op}},
		ApplyOptions{Conflict: ConflictOverwrite, DisableAudit: true, filesystem: &filesystem},
	)
	if err == nil || !strings.Contains(err.Error(), "changed during overwrite") {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	assertTestFile(t, source, "new")
	assertTestFile(t, destination, "concurrent replacement")
}

func TestCrossDeviceMovePartialCleanupRetainsPublishedOverwrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "a"), "A")
	writeTestFile(t, filepath.Join(source, "b"), "B")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(destination, "old"), "old")
	op := guardedMoveOperation(t, source, destination)
	filesystem := forcedCrossDeviceFilesystem(t, destination, nil)
	wantCleanupError := errors.New("forced source cleanup failure")
	failPath := filepath.Join(source, "a")
	filesystem.remove = func(path string) error {
		if samePath(path, failPath) {
			return wantCleanupError
		}
		return os.Remove(path)
	}
	results, err := Apply(
		context.Background(),
		Plan{Header: PlanHeader{Version: PlanVersion, Root: root}, Operations: []Operation{op}},
		ApplyOptions{Conflict: ConflictOverwrite, DisableAudit: true, filesystem: filesystem},
	)
	assertPartialMoveResult(t, results, err)
	assertTestFile(t, filepath.Join(destination, "a"), "A")
	assertTestFile(t, filepath.Join(destination, "b"), "B")
	if _, statErr := os.Stat(filepath.Join(destination, "old")); !os.IsNotExist(statErr) {
		t.Fatalf("old destination was restored: %v", statErr)
	}
	assertTestFile(t, filepath.Join(source, "a"), "A")
	if _, statErr := os.Stat(filepath.Join(source, "b")); !os.IsNotExist(statErr) {
		t.Fatalf("captured source removed before failure still exists: %v", statErr)
	}
	backups, globErr := filepath.Glob(filepath.Join(root, ".dirstat-backup-*"))
	if globErr != nil || len(backups) != 0 {
		t.Fatalf("overwrite backups=%v error=%v", backups, globErr)
	}
}

func TestCrossDeviceMovePreservesPostCopySourceChanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
		check  func(*testing.T, string)
	}{
		{
			name: "new entry",
			mutate: func(t *testing.T, source string) {
				writeTestFile(t, filepath.Join(source, "late"), "late")
			},
			check: func(t *testing.T, source string) {
				assertTestFile(t, filepath.Join(source, "late"), "late")
			},
		},
		{
			name: "changed captured entry",
			mutate: func(t *testing.T, source string) {
				writeTestFile(t, filepath.Join(source, "original"), "changed after publication")
			},
			check: func(t *testing.T, source string) {
				assertTestFile(t, filepath.Join(source, "original"), "changed after publication")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(source, "original"), "staged")
			op := guardedMoveOperation(t, source, destination)
			filesystem := forcedCrossDeviceFilesystem(t, destination, func() { test.mutate(t, source) })
			results, err := Apply(
				context.Background(),
				Plan{Header: PlanHeader{Version: PlanVersion, Root: root}, Operations: []Operation{op}},
				ApplyOptions{Conflict: ConflictOverwrite, DisableAudit: true, filesystem: filesystem},
			)
			assertPartialMoveResult(t, results, err)
			assertTestFile(t, filepath.Join(destination, "original"), "staged")
			test.check(t, source)
		})
	}
}

func TestCrossDeviceMoveSuccessRemovesCapturedSource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	writeTestFile(t, source, "payload")
	op := guardedMoveOperation(t, source, destination)
	filesystem := forcedCrossDeviceFilesystem(t, destination, nil)
	results, err := Apply(
		context.Background(),
		Plan{Header: PlanHeader{Version: PlanVersion, Root: root}, Operations: []Operation{op}},
		ApplyOptions{Conflict: ConflictOverwrite, DisableAudit: true, filesystem: filesystem},
	)
	if err != nil || len(results) != 1 || results[0].Status != "ok" || results[0].MayHaveMutated {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	assertTestFile(t, destination, "payload")
	if _, statErr := os.Stat(source); !os.IsNotExist(statErr) {
		t.Fatalf("source still exists: %v", statErr)
	}
}

func guardedMoveOperation(t *testing.T, source, destination string) Operation {
	t.Helper()
	expectedSource, err := fsinfo.Inspect(source, false)
	if err != nil {
		t.Fatal(err)
	}
	expectedDestination, err := fsinfo.CapturePath(destination)
	if err != nil {
		t.Fatal(err)
	}
	return Operation{
		ID: "move", Action: ActionMove, Source: source, Destination: destination,
		Expected: &expectedSource, ExpectedDestination: &expectedDestination,
	}
}

func forcedCrossDeviceFilesystem(
	t *testing.T,
	destination string,
	afterPublish func(),
) *mutationFilesystem {
	t.Helper()
	filesystem := defaultMutationFilesystem()
	crossDeviceError := errors.New("forced cross-device rename")
	forced := false
	filesystem.crossDevice = func(err error) bool { return errors.Is(err, crossDeviceError) }
	filesystem.publish = func(oldPath, newPath string) error {
		// The first publication to the requested destination models EXDEV. The
		// operation canonicalizes paths before invoking the seam, and Windows
		// may change their spelling, so matching the destination plus one-shot
		// state is more robust than comparing the original source string.
		if !forced && samePath(newPath, destination) {
			forced = true
			return crossDeviceError
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return err
		}
		if samePath(newPath, destination) && strings.HasPrefix(filepath.Base(oldPath), ".dirstat-move-") && afterPublish != nil {
			afterPublish()
		}
		return nil
	}
	return &filesystem
}

func assertPartialMoveResult(t *testing.T, results []Result, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "source cleanup incomplete") {
		t.Fatalf("results=%#v error=%v", results, err)
	}
	if len(results) != 1 || results[0].Status != "partial" || !results[0].MayHaveMutated {
		t.Fatalf("partial results=%#v", results)
	}
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertTestFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
