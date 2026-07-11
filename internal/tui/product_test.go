package tui

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phillipod/go-dirstat/internal/diagnose"
	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/fsops"
	"github.com/phillipod/go-dirstat/internal/history"
	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

func TestTargetSizeAndPressureForecast(t *testing.T) {
	tests := []struct {
		input string
		want  uint64
	}{
		{input: "4096", want: 4096},
		{input: "20G", want: 20 << 30},
		{input: "3k", want: 3 << 10},
		{input: "0B", want: 0},
	}
	for _, test := range tests {
		got, err := parseTargetBytes(test.input)
		if err != nil || got != test.want {
			t.Fatalf("parseTargetBytes(%q) = %d, %v; want %d", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{"-1", "1.5G", "G", "16E"} {
		if _, err := parseTargetBytes(input); err == nil {
			t.Errorf("parseTargetBytes(%q) succeeded", input)
		}
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := newModel(New(t.TempDir(), scope.New(), tree.SizeOnDisk, 1, Options{}))
	m.volume = fsinfo.Volume{Available: 100, PhysicalUsed: 800, CallerCapacity: 900}
	m.targetAvailable = 500
	m.queue = []fsops.Operation{{
		ID: "delete", Action: fsops.ActionDelete, Source: filepath.Join(m.rootAbs, "gone"),
		Expected: &fsinfo.Entry{Allocated: 100},
	}}
	forecast := m.pressureForecast()
	if forecast.queued != 100 || forecast.availableAfter != 200 || forecast.targetGap != 400 || forecast.targetGapAfter != 300 {
		t.Fatalf("forecast = %#v", forecast)
	}
	if math.Abs(forecast.callerPressureAfter-700.0/9.0) > 0.001 {
		t.Fatalf("forecast caller pressure = %.4f", forecast.callerPressureAfter)
	}
	m.width, m.height = 200, 20
	m.volumeLoadedAt = time.Now()
	if badges := strings.Join(m.pressureBadges(), " "); !strings.Contains(badges, "available") || !strings.Contains(badges, "forecast gap") {
		t.Fatalf("pressure badges = %q", badges)
	}
}

func TestQueuePolicyCapsAreEnforcedAtStageAndConfirmation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	m := newModel(New(root, scope.New(), tree.SizeOnDisk, 1, Options{
		QueueMaxOperations: 1, QueueMaxReclaimBytes: 50,
	}))
	op := fsops.Operation{ID: "too-large", Action: fsops.ActionDelete, Source: filepath.Join(root, "gone"), Expected: &fsinfo.Entry{Allocated: 60}}
	m = asModel(t, m, stagedMsg{operations: []fsops.Operation{op}})
	if len(m.queue) != 0 || !strings.Contains(m.managementError, "reclaim estimate") {
		t.Fatalf("stage cap state: queue=%#v error=%q", m.queue, m.managementError)
	}

	m.queue = []fsops.Operation{op}
	m.management = managementReview
	m.managementSeen = 1
	m.managementError = ""
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.management != managementReview || !strings.Contains(m.managementError, "reclaim estimate") {
		t.Fatalf("confirmation cap state: mode=%v error=%q", m.management, m.managementError)
	}
}

func TestGrowthReadOnlyExcludesContainedStateWithoutCreatingIt(t *testing.T) {
	root, node := mkTree(t)
	stateHome := filepath.Join(root, ".operator-state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	originalPolicy := scope.New()
	app := New(root, originalPolicy, tree.SizeOnDisk, 1, Options{UseCache: true, ReadOnly: true, HistoryMax: 5})
	m := newModel(app)
	if m.cacheWritable {
		t.Fatal("read-only model enabled cache writes")
	}
	if !m.app.policy.RejectsPath(m.historyDir) {
		t.Fatalf("history state %q is not excluded from broad-root scan", m.historyDir)
	}
	_, expectedFingerprint, contained, err := historyAwarePolicy(root, originalPolicy, m.historyDir)
	if err != nil || !contained || m.fingerprint != expectedFingerprint {
		t.Fatalf("history policy parity: contained=%t fingerprint=%q want=%q err=%v", contained, m.fingerprint, expectedFingerprint, err)
	}
	m.root, m.stats, m.completeTree = node, scan.Stats{Complete: true}, true
	m.snapshotAt = time.Now().UTC()
	msg := m.startGrowthAnalysis()()
	m = asModel(t, m, msg)
	if m.growth.err != nil || !m.growth.baselineMissing || m.growth.baselineRecorded {
		t.Fatalf("read-only growth state = %#v", m.growth)
	}
	if _, err := os.Stat(stateHome); !os.IsNotExist(err) {
		t.Fatalf("read-only growth created state: %v", err)
	}
}

func TestHistoryAwarePolicyKeepsDisjointStoreFingerprintStable(t *testing.T) {
	root := t.TempDir()
	storeA := t.TempDir()
	storeB := t.TempDir()
	policyA, fingerprintA, containedA, err := historyAwarePolicy(root, scope.New(), storeA)
	if err != nil {
		t.Fatalf("historyAwarePolicy(storeA): %v", err)
	}
	policyB, fingerprintB, containedB, err := historyAwarePolicy(root, scope.New(), storeB)
	if err != nil {
		t.Fatalf("historyAwarePolicy(storeB): %v", err)
	}
	if containedA || containedB {
		t.Fatalf("disjoint stores were treated as contained: A=%t B=%t", containedA, containedB)
	}
	if fingerprintA != fingerprintB {
		t.Fatalf("disjoint store changed fingerprint: %q != %q", fingerprintA, fingerprintB)
	}
	if len(policyA.ExcludePaths) != 0 || len(policyB.ExcludePaths) != 0 {
		t.Fatalf("disjoint stores changed exclusions: A=%#v B=%#v", policyA.ExcludePaths, policyB.ExcludePaths)
	}

	if _, _, _, err := historyAwarePolicy(root, scope.New(), filepath.Dir(root)); err == nil {
		t.Fatal("scan root nested beneath history store was accepted")
	}
}

func TestOpenDeletedViewShowsCoverageLowerBoundAndHolderEvidence(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := newModel(New(t.TempDir(), scope.New(), tree.SizeOnDisk, 1, Options{ReadOnly: true}))
	m.width, m.height, m.view = 160, 24, viewOpenDeleted
	coverage := diagnose.OpenDeletedCoverage{ProcessesScanned: 4, ProcessEntries: 5, DescriptorsScanned: 8, DescriptorEntries: 9, ProcessesSkipped: 1, DescriptorsSkipped: 1}
	file := diagnose.OpenDeletedFile{Device: 7, Inode: 9, Path: "/tmp/gone", Size: 8192, Allocated: 4096,
		Holders: []diagnose.OpenDeletedHolder{{PID: 42, Process: "worker", Descriptors: []string{"3", "8"}}}}
	m.openDeleted = openDeletedViewState{loadedAt: time.Now(), result: diagnose.Result{
		Capabilities: []diagnose.Capability{{Name: "open_deleted", Available: true}},
		OpenDeleted:  []diagnose.OpenDeletedFile{file},
		OpenDeletedSummary: &diagnose.OpenDeletedSummary{Objects: 1, Holders: 1, Descriptors: 2,
			ReclaimableBytes: 4096, Coverage: coverage},
	}}
	if body := m.openDeletedBody(); !strings.Contains(body, "observed lower bound") || !strings.Contains(body, "coverage:partial") || !strings.Contains(body, "dev=7 ino=9") {
		t.Fatalf("open-deleted body:\n%s", body)
	}
	if contextBody := m.analysisContextBody(60); !strings.Contains(contextBody, "pid 42") || !strings.Contains(contextBody, "fds 3,8") {
		t.Fatalf("open-deleted context:\n%s", contextBody)
	}

	for _, cancelKey := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("c")},
		{Type: tea.KeyEsc},
	} {
		canceled := false
		m.openDeleted.loading = true
		m.analysisCancel = func() { canceled = true }
		generation := m.analysisGeneration
		m = asModel(t, m, cancelKey)
		if !canceled || m.openDeleted.loading || m.analysisGeneration != generation+1 {
			t.Fatalf("cancel %q state: canceled=%t loading=%t generation=%d", cancelKey.String(), canceled, m.openDeleted.loading, m.analysisGeneration)
		}
	}
}

func TestDestinationPickerIsConfinedAndCapacityAware(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "target")
	outside := t.TempDir()
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		if os.IsPermission(err) {
			t.Skip("symlinks unavailable")
		}
		t.Fatal(err)
	}
	msg := destinationPickerCmd(context.Background(), 3, root, root)().(destinationLoadedMsg)
	if msg.state.err != nil || len(msg.state.entries) != 1 || msg.state.entries[0].path != child {
		t.Fatalf("destination listing = %#v", msg.state)
	}
	if _, err := confinedDestinationDirectory(root, outside); err == nil {
		t.Fatal("outside destination was accepted")
	}
	if _, err := confinedDestinationDirectory(root, filepath.Join(root, "escape")); err == nil {
		t.Fatal("symlink escape was accepted")
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	source := filepath.Join(root, "source.txt")
	if err := os.WriteFile(source, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(New(root, scope.New(), tree.SizeOnDisk, 1, Options{}))
	m.width, m.height = 52, 10
	m.management, m.managementAction = managementDestination, fsops.ActionCopy
	m.marks[source] = true
	m.destination = msg.state
	m.destination.path = child
	m.volume.Device, m.destination.volume.Device = "source-device", "target-device"
	if body := m.destinationBody(); !strings.Contains(body, "cross-device") || strings.Count(body, "\n")+1 > m.availHeight() {
		t.Fatalf("destination body:\n%s", body)
	}
	handled, cmd := m.handleDestinationPickerKey(tea.KeyMsg{Type: tea.KeyTab})
	if !handled || cmd == nil {
		t.Fatal("Tab did not choose the current destination")
	}
	staged := cmd().(stagedMsg)
	if staged.err != nil || len(staged.operations) != 1 || staged.operations[0].Destination != filepath.Join(child, filepath.Base(source)) {
		t.Fatalf("staged destination = %#v, %v", staged.operations, staged.err)
	}

	readOnly := newModel(New(root, scope.New(), tree.SizeOnDisk, 1, Options{ReadOnly: true}))
	readOnly.marks[source] = true
	if cmd := readOnly.startInput(fsops.ActionMove); cmd != nil || readOnly.management != managementNone || !strings.Contains(readOnly.managementError, "read-only") {
		t.Fatalf("read-only picker state: cmd=%t mode=%v error=%q", cmd != nil, readOnly.management, readOnly.managementError)
	}
}

func TestAnalyticalViewsAndTargetControlsCoverStateTransitions(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	m := newModel(New(root, scope.New(), tree.SizeOnDisk, 1, Options{ReadOnly: true, DisableAudit: true}))
	m.width, m.height = 120, 30

	m.beginTargetInput()
	if !m.targeting || m.targetInput != "" {
		t.Fatalf("target input did not open: targeting=%t input=%q", m.targeting, m.targetInput)
	}
	if err := m.applyTargetInput(); err == nil {
		t.Fatal("empty target input was accepted")
	}
	m.targetInput = "not-a-size"
	if err := m.applyTargetInput(); err == nil {
		t.Fatal("invalid target input was accepted")
	}
	m.targetInput = "2G"
	if err := m.applyTargetInput(); err != nil || m.targeting || m.targetAvailable != 2<<30 {
		t.Fatalf("valid target input: targeting=%t target=%d error=%v", m.targeting, m.targetAvailable, err)
	}

	m.view = viewGrowth
	m.growth = growthViewState{loading: true}
	if body := m.growthBody(); !strings.Contains(body, "Comparing") {
		t.Fatalf("growth loading body = %q", body)
	}
	m.growth = growthViewState{err: errors.New("history unavailable")}
	if body := m.growthBody(); !strings.Contains(body, "history unavailable") {
		t.Fatalf("growth error body = %q", body)
	}
	m.growth = growthViewState{loadedAt: time.Now(), currentAt: time.Now().Add(-time.Minute), currentComplete: true,
		baselineAt: time.Now().Add(-time.Hour), truncated: true,
		deltas: []history.Delta{{Path: "big.bin", Change: history.ChangeGrown, BeforeAlloc: 1, AfterAlloc: 9, BeforeApparent: 2, AfterApparent: 10}}}
	m.cursor = 0
	if body := m.growthBody(); !strings.Contains(body, "Showing the first") || !strings.Contains(body, "big.bin") {
		t.Fatalf("growth populated body = %q", body)
	}
	if detail := m.analysisDetailLine(); !strings.Contains(detail, "big.bin") {
		t.Fatalf("growth detail = %q", detail)
	}
	if evidence := m.analysisContextBody(80); !strings.Contains(evidence, "allocated") || !strings.Contains(evidence, "apparent") {
		t.Fatalf("growth evidence = %q", evidence)
	}
	m.growth = growthViewState{baselineMissing: true, baselineRecorded: true}
	if body := m.growthBody(); !strings.Contains(body, "first growth baseline") {
		t.Fatalf("growth baseline body = %q", body)
	}

	m.view = viewOpenDeleted
	m.openDeleted = openDeletedViewState{err: errors.New("proc unavailable")}
	if body := m.openDeletedBody(); !strings.Contains(body, "proc unavailable") {
		t.Fatalf("open-deleted error body = %q", body)
	}
	m.openDeleted = openDeletedViewState{loadedAt: time.Now(), result: diagnose.Result{
		Capabilities: []diagnose.Capability{{Name: "open_deleted", Available: false, Reason: "unsupported host"}},
		Warnings:     []string{"one process skipped"},
	}}
	if body := m.openDeletedBody(); !strings.Contains(body, "unsupported host") {
		t.Fatalf("open-deleted unavailable body = %q", body)
	}
	m.openDeleted = openDeletedViewState{loadedAt: time.Now(), result: diagnose.Result{
		OpenDeleted: []diagnose.OpenDeletedFile{{Path: "/tmp/gone", Device: 1, Inode: 2, Size: 10, Allocated: 8}},
		Warnings:    []string{"one process skipped"},
	}}
	if body := m.openDeletedBody(); !strings.Contains(body, "one process skipped") || !strings.Contains(body, "/tmp/gone") {
		t.Fatalf("open-deleted populated body = %q", body)
	}
	m.cursor = 0
	if detail := m.analysisDetailLine(); !strings.Contains(detail, "/tmp/gone") {
		t.Fatalf("open-deleted detail = %q", detail)
	}
	if evidence := m.analysisContextBody(80); !strings.Contains(evidence, "logical") {
		t.Fatalf("open-deleted evidence = %q", evidence)
	}

	m.rows = []row{{}}
	m.extRows = []extRow{{}}
	m.topRows = []topRow{{}}
	m.growth.deltas = []history.Delta{{Path: "growth"}}
	m.openDeleted.result.OpenDeleted = []diagnose.OpenDeletedFile{{Path: "deleted"}}
	for _, view := range []viewMode{viewTree, viewExt, viewLargest, viewGrowth, viewOpenDeleted, viewHelp} {
		m.view = view
		_ = m.visibleListLen()
		_ = m.activateViewCmd()
	}
	m.view = viewGrowth
	m.cycleView()
	m.cycleViewBackward()
	m.view = viewOpenDeleted
	m.cycleView()
	m.cycleViewBackward()
	if cmd := m.startOpenDeletedAnalysis(); cmd == nil {
		t.Fatal("open-deleted analysis command is nil")
	}
	m.cancelAnalysis()
	if message := scanStreamCmd(m.app, nil, context.Background(), 1)(); message.(scanDoneMsg).err != nil {
		t.Fatalf("streaming analytical scan: %v", message.(scanDoneMsg).err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if message := scanStreamCmd(m.app, nil, canceled, 2)(); !errors.Is(message.(scanDoneMsg).err, context.Canceled) {
		t.Fatalf("canceled streaming analytical scan: %v", message.(scanDoneMsg).err)
	}
}

func TestProductFormattingAndNavigationEdges(t *testing.T) {
	if parentPath("a/b") != "a" || parentPath("single") != "" {
		t.Fatalf("parentPath boundaries failed")
	}
	for _, test := range []struct {
		value int64
		want  string
	}{
		{value: 0, want: "+0B"},
		{value: -1, want: "-1B"},
		{value: -maxSignedInt64 - 1, want: "-9223372036854775808B"},
	} {
		if got := signedBytes(test.value); got != test.want {
			t.Fatalf("signedBytes(%d) = %q, want %q", test.value, got, test.want)
		}
	}
	if got := formatVolumeBytes(^uint64(0)); got == "" || !strings.HasSuffix(got, "B") {
		t.Fatalf("large volume formatting = %q", got)
	}
	if got := ageSince(time.Time{}); got != unknownLabel {
		t.Fatalf("zero age = %q", got)
	}
	if got := destinationHelp(); !strings.Contains(got, "Tab choose current") {
		t.Fatalf("destination help = %q", got)
	}

	root := t.TempDir()
	m := newModel(New(root, scope.New(), tree.SizeOnDisk, 1, Options{ReadOnly: true, DisableAudit: true}))
	m.width, m.height = 100, 24
	m.management = managementNone
	m.targeting = true
	m.managementError = "target invalid"
	if footer := m.footerView(); !strings.Contains(footer, "target invalid") {
		t.Fatalf("target footer = %q", footer)
	}
	m.targeting, m.managementError = false, ""
	m.view = viewGrowth
	if footer := m.footerView(); !strings.Contains(footer, "r refresh/record") {
		t.Fatalf("growth footer = %q", footer)
	}
	m.view = viewOpenDeleted
	if footer := m.footerView(); !strings.Contains(footer, "Y growth") {
		t.Fatalf("open-deleted footer = %q", footer)
	}

	node := &tree.Node{Name: filepath.Base(root), IsDir: true}
	dir := &tree.Node{Name: "dir", IsDir: true}
	dir.AddChild(&tree.Node{Name: "file"})
	node.AddChild(dir)
	m.root = node
	m.expanded = map[string]bool{"": true, "dir": true}
	m.rebuild()
	m.view = viewTree
	m.cursor = 1
	m.collapseOrUp()
	m.view = viewExt
	m.extRows = []extRow{{}, {}}
	m.cursor = 0
	_, _ = m.moveOnly(tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 1 {
		t.Fatalf("moveOnly navigation: cursor=%d", m.cursor)
	}
}
