package tui

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/fsops"
	"github.com/phillipod/go-dirstat/internal/index"
	"github.com/phillipod/go-dirstat/internal/preview"
	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

func TestAppRunHeadlessReadOnlyLifecycle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)

	app := New(root, scope.New(), tree.SizeOnDisk, 1, Options{
		ReadOnly:     true,
		DisableAudit: true,
	})
	app.programOptions = []tea.ProgramOption{
		tea.WithInput(strings.NewReader("q")),
		tea.WithOutput(io.Discard),
		tea.WithoutRenderer(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Run(ctx); err != nil {
		t.Fatalf("headless Run() error = %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("read-only App.Run created mutation state: %v", err)
	}
}

func TestAppRunHonorsCanceledParent(t *testing.T) {
	app := New(t.TempDir(), scope.New(), tree.SizeOnDisk, 1, Options{DisableAudit: true})
	app.programOptions = []tea.ProgramOption{
		tea.WithInput(strings.NewReader("")),
		tea.WithOutput(io.Discard),
		tea.WithoutRenderer(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.Run(ctx); !errors.Is(err, tea.ErrProgramKilled) {
		t.Fatalf("Run() error = %v, want ErrProgramKilled", err)
	}
}

func TestModelInitLoadsCacheAndStartsRefresh(t *testing.T) {
	isolateUserCache(t)
	root, node := mkTree(t)
	app := New(root, scope.New(), tree.SizeApparent, 1, Options{UseCache: true, DisableAudit: true})
	m := newModel(app)
	if m.store == nil {
		t.Fatalf("cache store unavailable: %v", m.cacheErr)
	}
	stats := scan.Stats{
		Files:  node.FileCount,
		Dirs:   node.DirCount + 1,
		RootFS: "testfs",
	}
	snapshot := index.FromTree(node, m.fingerprint, stats.RootFS, stats.Files, stats.Dirs, 0, true, time.Now().Add(-2*time.Minute))
	snapshot.Root = m.rootAbs
	if err := m.store.Save(snapshot); err != nil {
		t.Fatal(err)
	}

	cmd := m.Init()
	if cmd == nil || !m.scanning || !m.gotData || !m.completeTree {
		t.Fatalf("Init state: command=%t scanning=%t data=%t complete=%t", cmd != nil, m.scanning, m.gotData, m.completeTree)
	}
	if !strings.Contains(m.cacheNote, "cached") || m.stats.Files != stats.Files || m.findNode(filepath.Join(root, testBigFile)) == nil {
		t.Fatalf("cached startup not adopted: note=%q stats=%#v", m.cacheNote, m.stats)
	}
	m.cancelScan()
}

func TestSchemaV2PartialOutcomesReconcileAndRemainQueued(t *testing.T) {
	tests := []struct {
		name        string
		action      fsops.Action
		status      string
		may         bool
		resultError string
		outer       error
	}{
		{name: "partial delete", action: fsops.ActionDelete, status: fsops.ResultStatusPartial, may: true, resultError: "delete incomplete", outer: errors.New("delete incomplete")},
		{name: "partial copy without redundant flag", action: fsops.ActionCopy, status: fsops.ResultStatusPartial},
		{name: "move may-have-mutated error", action: fsops.ActionMove, status: fsops.ResultStatusError, may: true, resultError: "move incomplete"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, node := mkTree(t)
			m := newModel(New(root, scope.New(), tree.SizeApparent, 1, Options{DisableAudit: true}))
			m = asModel(t, m, scanDoneMsg{node: node, stats: scan.Stats{Complete: true}})
			m.width, m.height = 120, 30
			source := filepath.Join(root, testGoFile)
			op := fsops.Operation{ID: "pending", Action: test.action, Source: source}
			if test.action != fsops.ActionDelete {
				op.Destination = filepath.Join(root, "destination")
			}
			m.queue = []fsops.Operation{op}
			m.marks[source] = true
			m.management = managementApplying

			updated, cmd := m.Update(appliedMsg{
				results: []fsops.Result{{
					Version: fsops.PlanVersion, OperationID: op.ID, Action: test.action,
					Status: test.status, MayHaveMutated: test.may, Error: test.resultError,
				}},
				err: test.outer,
			})
			m = updated.(*model)
			if cmd == nil || !m.scanning {
				t.Fatal("partial outcome did not start reconciliation")
			}
			if len(m.queue) != 1 || m.queue[0].ID != op.ID || !m.marks[source] {
				t.Fatalf("pending state was discarded: queue=%#v marks=%#v", m.queue, m.marks)
			}
			if m.management != managementResult || m.managementError == "" {
				t.Fatalf("partial result hidden: mode=%v error=%q", m.management, m.managementError)
			}
			if body := m.managementBody(); !strings.Contains(body, "may have changed disk") || !strings.Contains(body, "reconciling") {
				t.Fatalf("partial result message is not actionable:\n%s", body)
			}
			m.cancelScan()
		})
	}
}

func TestResultReconciliationContractIsFailSafe(t *testing.T) {
	tests := []struct {
		name   string
		result fsops.Result
		want   bool
	}{
		{name: "dry run", result: fsops.Result{DryRun: true, Status: fsops.ResultStatusPartial, MayHaveMutated: true}},
		{name: "complete", result: fsops.Result{Status: fsops.ResultStatusOK}},
		{name: "ok but mutation flag", result: fsops.Result{Status: fsops.ResultStatusOK, MayHaveMutated: true}, want: true},
		{name: "partial", result: fsops.Result{Status: fsops.ResultStatusPartial}, want: true},
		{name: "error", result: fsops.Result{Status: fsops.ResultStatusError}, want: true},
		{name: "unknown", result: fsops.Result{Status: "future-status"}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resultNeedsReconciliation(test.result); got != test.want {
				t.Fatalf("resultNeedsReconciliation(%#v) = %t, want %t", test.result, got, test.want)
			}
		})
	}
}

func TestApplyAuditFailureAndCancellationPreserveTruthfulQueueState(t *testing.T) {
	t.Run("completed mutation with failed audit cannot be retried", func(t *testing.T) {
		root, node := mkTree(t)
		m := newModel(New(root, scope.New(), tree.SizeApparent, 1, Options{DisableAudit: true}))
		m = asModel(t, m, scanDoneMsg{node: node, stats: scan.Stats{Complete: true}})
		source := filepath.Join(root, testGoFile)
		op := fsops.Operation{ID: "copy-audit", Action: fsops.ActionCopy, Source: source, Destination: filepath.Join(root, "copy.go")}
		m.queue = []fsops.Operation{op}
		m.marks[source] = true
		m.management = managementApplying
		m.width, m.height = 120, 24

		m = asModel(t, m, appliedMsg{results: []fsops.Result{{
			OperationID: op.ID, Action: op.Action, Status: fsops.ResultStatusPartial,
			MayHaveMutated: true, MutationCompleted: true, AuditStatus: fsops.AuditStatusFailed,
			Error: "mutation completed but audit result was not durable",
		}}})
		if len(m.queue) != 0 || m.marks[source] || !m.scanning {
			t.Fatalf("completed audit-failed state: queue=%#v marks=%#v scanning=%t", m.queue, m.marks, m.scanning)
		}
		if body := m.managementBody(); !strings.Contains(body, "audit:failed") || !strings.Contains(body, "mutation completed") {
			t.Fatalf("audit failure evidence missing:\n%s", body)
		}
		m.cancelScan()
	})

	t.Run("completed prefix retains only pending marks", func(t *testing.T) {
		root, node := mkTree(t)
		m := newModel(New(root, scope.New(), tree.SizeApparent, 1, Options{DisableAudit: true}))
		m = asModel(t, m, scanDoneMsg{node: node, stats: scan.Stats{Complete: true}})
		doneSource := filepath.Join(root, testGoFile)
		pendingSource := filepath.Join(root, testBigFile)
		m.queue = []fsops.Operation{
			{ID: "done", Action: fsops.ActionCopy, Source: doneSource, Destination: filepath.Join(root, "copy.go")},
			{ID: "pending", Action: fsops.ActionMove, Source: pendingSource, Destination: filepath.Join(root, "moved.bin")},
		}
		m.marks[doneSource], m.marks[pendingSource] = true, true
		updated, cmd := m.Update(appliedMsg{
			results: []fsops.Result{
				{OperationID: "done", Action: fsops.ActionCopy, Status: fsops.ResultStatusOK},
				{OperationID: "pending", Action: fsops.ActionMove, Status: fsops.ResultStatusPartial, MayHaveMutated: true, Error: "cleanup incomplete"},
			},
			err: errors.New("cleanup incomplete"),
		})
		m = updated.(*model)
		if cmd == nil || !m.scanning || len(m.queue) != 1 || m.queue[0].ID != "pending" {
			t.Fatalf("mixed outcome state: command=%t scanning=%t queue=%#v", cmd != nil, m.scanning, m.queue)
		}
		if m.marks[doneSource] || !m.marks[pendingSource] {
			t.Fatalf("mixed outcome marks = %#v", m.marks)
		}
		m.cancelScan()
	})

	t.Run("completed mutation with audit failure", func(t *testing.T) {
		root, node := mkTree(t)
		m := newModel(New(root, scope.New(), tree.SizeApparent, 1, Options{DisableAudit: true}))
		m = asModel(t, m, scanDoneMsg{node: node, stats: scan.Stats{Complete: true}})
		source := filepath.Join(root, testGoFile)
		m.queue = []fsops.Operation{{ID: "copy", Action: fsops.ActionCopy, Source: source, Destination: filepath.Join(root, "copy.go")}}
		m.marks[source] = true
		updated, cmd := m.Update(appliedMsg{
			results: []fsops.Result{{OperationID: "copy", Action: fsops.ActionCopy, Status: fsops.ResultStatusOK}},
			err:     errors.New("write audit result: disk full"),
		})
		m = updated.(*model)
		if cmd == nil || !m.scanning || len(m.queue) != 0 || len(m.marks) != 0 {
			t.Fatalf("audit failure state: command=%t scanning=%t queue=%#v marks=%#v", cmd != nil, m.scanning, m.queue, m.marks)
		}
		if !strings.Contains(m.managementError, "audit") {
			t.Fatalf("audit failure hidden: %q", m.managementError)
		}
		m.cancelScan()
	})

	t.Run("canceled before an outcome", func(t *testing.T) {
		root, node := mkTree(t)
		m := newModel(New(root, scope.New(), tree.SizeApparent, 1, Options{DisableAudit: true}))
		m = asModel(t, m, scanDoneMsg{node: node, stats: scan.Stats{Complete: true}})
		source := filepath.Join(root, testGoFile)
		m.queue = []fsops.Operation{{ID: "move", Action: fsops.ActionMove, Source: source, Destination: filepath.Join(root, "moved.go")}}
		m.marks[source] = true
		updated, cmd := m.Update(appliedMsg{err: context.Canceled})
		m = updated.(*model)
		if cmd == nil || !m.scanning || len(m.queue) != 1 || !m.marks[source] {
			t.Fatalf("canceled state: command=%t scanning=%t queue=%#v marks=%#v", cmd != nil, m.scanning, m.queue, m.marks)
		}
		if !strings.Contains(m.managementError, context.Canceled.Error()) {
			t.Fatalf("cancellation hidden: %q", m.managementError)
		}
		m.cancelScan()
	})
}

func TestApplyCommandCancellationUsesOperationContext(t *testing.T) {
	started := make(chan struct{})
	app := New(t.TempDir(), scope.New(), tree.SizeOnDisk, 1, Options{DisableAudit: true})
	app.applyPlan = func(ctx context.Context, _ fsops.Plan, _ fsops.ApplyOptions) ([]fsops.Result, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m := newModel(app)
	m.scanning = false
	m.queue = []fsops.Operation{{ID: "delete", Action: fsops.ActionDelete, Source: filepath.Join(m.rootAbs, "file")}}
	cmd := m.applyCmd()
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("apply command did not start")
	}
	if m.applyCancel == nil {
		t.Fatal("apply command omitted cancellation handle")
	}
	m.applyCancel()
	var msg tea.Msg
	select {
	case msg = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled apply command did not return")
	}
	updated, reconcile := m.Update(msg)
	m = updated.(*model)
	if reconcile == nil || !m.scanning || len(m.queue) != 1 || !strings.Contains(m.managementError, context.Canceled.Error()) {
		t.Fatalf("canceled apply integration: reconcile=%t scanning=%t queue=%#v error=%q", reconcile != nil, m.scanning, m.queue, m.managementError)
	}
	m.cancelScan()
}

func TestExternalCommandsAndCompletionsCoverMutationTrustBoundary(t *testing.T) {
	root, node := mkTree(t)
	app := New(root, scope.New(), tree.SizeApparent, 1, Options{
		DisableAudit: true,
		Editor:       []string{"editor", "--literal"},
		Pager:        []string{"pager", "--literal"},
		Shell:        []string{"shell", "--literal"},
	})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: node, stats: scan.Stats{Complete: true}})
	for i, row := range m.rows {
		if row.node.Name == testGoFile {
			m.cursor = i
			break
		}
	}
	if cmd := m.externalEditorCmd(); cmd == nil {
		t.Fatalf("editor command missing: %q", m.managementError)
	}
	if cmd := m.pagerCmd(); cmd == nil {
		t.Fatalf("pager command missing: %q", m.managementError)
	}
	if cmd := m.shellCmd(); cmd == nil {
		t.Fatalf("shell command missing: %q", m.managementError)
	}
	if got := m.selectedWorkingDirectory(); got != root {
		t.Fatalf("selected working directory = %q, want %q", got, root)
	}

	for _, kind := range []string{"editor", "shell"} {
		m.scanning = false
		updated, cmd := m.Update(externalDoneMsg{kind: kind})
		m = updated.(*model)
		if cmd == nil || !m.scanning || m.managementError != "" {
			t.Fatalf("%s success: command=%t scanning=%t error=%q", kind, cmd != nil, m.scanning, m.managementError)
		}
		m.cancelScan()
	}
	m.scanning = false
	updated, cmd := m.Update(externalDoneMsg{kind: "pager"})
	m = updated.(*model)
	if cmd != nil || m.scanning || m.managementError != "" {
		t.Fatalf("pager success: command=%t scanning=%t error=%q", cmd != nil, m.scanning, m.managementError)
	}
}

func TestManagementRenderingCoversLifecycleAndPartialResults(t *testing.T) {
	root, node := mkTree(t)
	m := newModel(New(root, scope.New(), tree.SizeApparent, 1, Options{DisableAudit: true}))
	m = asModel(t, m, scanDoneMsg{node: node, stats: scan.Stats{Complete: true}})
	m.width, m.height = 120, 30
	op := fsops.Operation{ID: "delete", Action: fsops.ActionDelete, Source: filepath.Join(root, testBigFile)}

	tests := []struct {
		name      string
		mode      managementMode
		configure func()
		want      string
	}{
		{name: "none", mode: managementNone, want: "Esc close"},
		{name: "destination", mode: managementDestination, configure: func() { m.managementAction, m.managementInput = fsops.ActionCopy, "copy.bin" }, want: "Destination"},
		{name: "mkdir", mode: managementMkdir, configure: func() { m.managementInput = "new-dir" }, want: "Create directory"},
		{name: "export", mode: managementExport, configure: func() { m.managementInput = "plan.jsonl" }, want: "Export guarded queue"},
		{name: "review empty", mode: managementReview, want: "Queue is empty"},
		{name: "review populated", mode: managementReview, configure: func() { m.queue = []fsops.Operation{op}; m.resetQueueReview(); m.managementDryRun = true }, want: "dry-run passed"},
		{name: "dry run", mode: managementDryRun, configure: func() { m.queue = []fsops.Operation{op} }, want: "Dry-running"},
		{name: "confirm", mode: managementConfirm, configure: func() { m.queue = []fsops.Operation{op}; m.managementInput = applyConfirm }, want: "Type APPLY"},
		{name: "applying", mode: managementApplying, configure: func() { m.queue = []fsops.Operation{op} }, want: "revalidated"},
		{name: "success", mode: managementResult, configure: func() {
			m.applyResults = []fsops.Result{{OperationID: op.ID, Action: op.Action, Status: fsops.ResultStatusOK}}
		}, want: "View updated"},
		{name: "partial", mode: managementResult, configure: func() {
			m.managementError = "cleanup incomplete"
			m.scanning = true
			m.applyResults = []fsops.Result{{OperationID: op.ID, Action: op.Action, Status: fsops.ResultStatusPartial, MayHaveMutated: true}}
		}, want: "may have changed disk"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m.management = test.mode
			m.managementInput, m.managementError, m.managementNote = "", "", ""
			m.managementDryRun, m.scanning = false, false
			m.queue, m.applyResults = nil, nil
			if test.configure != nil {
				test.configure()
			}
			if body := m.managementBody(); !strings.Contains(body, test.want) {
				t.Fatalf("management body missing %q:\n%s", test.want, body)
			}
			_ = m.managementHelp()
		})
	}
}

func TestContextBodyRendersErrorsMetadataSymlinksAndPreviews(t *testing.T) {
	m := newModel(New(t.TempDir(), scope.New(), tree.SizeApparent, 1, Options{DisableAudit: true}))
	m.width, m.height = 120, 30
	m.view = viewExt
	if body := m.contextBody(); !strings.Contains(body, "Extension analysis") {
		t.Fatalf("extension context = %q", body)
	}
	m.view = viewTree
	m.inspectPath = ""
	if body := m.contextBody(); !strings.Contains(body, "Move selection") {
		t.Fatalf("empty context = %q", body)
	}
	m.inspectPath = filepath.Join(m.rootAbs, "missing")
	m.inspectErr = errors.New("metadata denied")
	if body := m.contextBody(); !strings.Contains(body, "metadata denied") {
		t.Fatalf("error context = %q", body)
	}
	m.inspectErr = nil
	m.inspectEntry = fsinfo.Entry{
		Kind: "symlink", ModeText: "Lrwxrwxrwx", Size: 12, Allocated: 4096,
		Owner: "alice", Group: "staff", Links: 1, Symlink: "target.txt",
		ModTime: time.Date(2026, time.July, 11, 10, 30, 0, 0, time.UTC),
	}
	m.inspectPreview = &preview.Result{Text: "first line\nsecond line"}
	body := m.contextBody()
	for _, want := range []string{"symlink", "allocated", "alice:staff", "target.txt", "preview", "second line"} {
		if !strings.Contains(body, want) {
			t.Fatalf("metadata context missing %q:\n%s", want, body)
		}
	}
	m.inspectPreview = &preview.Result{Binary: true, Hex: "00000000  00 ff"}
	if body := m.contextBody(); !strings.Contains(body, "00000000") {
		t.Fatalf("binary context = %q", body)
	}
}

func TestManagementReviewClampsAfterResize(t *testing.T) {
	m := newModel(New(t.TempDir(), scope.New(), tree.SizeOnDisk, 1, Options{DisableAudit: true}))
	m.management = managementReview
	for i := 0; i < 20; i++ {
		m.queue = append(m.queue, fsops.Operation{ID: "op-" + time.Unix(int64(i), 0).Format("150405"), Action: fsops.ActionDelete, Source: filepath.Join(m.rootAbs, "item")})
	}
	m.width, m.height = 100, 20
	m.resetQueueReview()
	m.managementCursor = len(m.queue) - 1
	m.clampQueueReview()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 42, Height: 8})
	m = updated.(*model)
	page := m.reviewPageSize()
	if m.managementCursor < m.managementOffset || m.managementCursor >= m.managementOffset+page {
		t.Fatalf("cursor %d is outside resized page [%d,%d)", m.managementCursor, m.managementOffset, m.managementOffset+page)
	}
	if lines := strings.Count(m.managementBody(), "\n") + 1; lines > m.availHeight() {
		t.Fatalf("management body has %d lines, available height %d", lines, m.availHeight())
	}
}
