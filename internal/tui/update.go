package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/fsops"
	"github.com/phillipod/go-dirstat/internal/index"
	"github.com/phillipod/go-dirstat/internal/tree"
)

const (
	keyCtrlC        = "ctrl+c"
	keyEscape       = "esc"
	keyEnter        = "enter"
	keyBackspace    = "backspace"
	keyDown         = "down"
	keyPageUp       = "pgup"
	keyPageDown     = "pgdown"
	queueEmptyError = "queue is empty"
	applyConfirm    = "APPLY"
	windowsOS       = "windows"
	kindDirectory   = "directory"
	unknownLabel    = "unknown"
)

// Update handles all input and async messages.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case progressMsg:
		if msg.generation != m.scanGeneration {
			return m, nil
		}
		if m.retainDuringScan {
			// A refresh snapshot is intentionally shallow and incomplete. Keep
			// the cached/previous tree intact until the authoritative result
			// arrives instead of making the UI regress mid-refresh.
			m.scanning = true
			m.scanNote = ""
			m.scanErr = nil
			return m, nil
		}
		// A partial snapshot arrived mid-scan. Snapshots are delivered oldest
		// first, so each one is progressively more complete — just adopt it.
		m.root = msg.node
		m.stats = msg.stats
		m.gotData = true
		m.completeTree = false
		m.scanning = true
		m.scanNote = ""
		m.scanErr = nil
		m.rebuild()
		if m.inspectGeneration == 0 {
			return m, m.requestInspect()
		}
		return m, nil

	case scanDoneMsg:
		if msg.generation != m.scanGeneration {
			return m, nil
		}
		m.cancelScan()
		m.scanning = false
		m.retainDuringScan = false
		m.scanNote = ""
		m.cacheNote = ""
		if msg.err != nil {
			m.scanErr = msg.err
			// Keep showing whatever we have; if nothing arrived yet, View shows
			// the error instead of an empty tree.
			return m, nil
		}
		m.root = msg.node
		m.stats = msg.stats
		m.gotData = true
		m.completeTree = msg.stats.Complete
		m.snapshotAt = time.Now().UTC()
		m.scanErr = nil
		m.rebuild()
		inspect := m.requestInspect()
		if !m.completeTree {
			m.scanNote = fmt.Sprintf("partial scan · %d errors; unresolved marks retained", msg.stats.Errors)
			return m, tea.Batch(inspect, m.loadPressureCmd())
		}
		if removed := m.reconcileMarks(); removed > 0 {
			m.scanNote = fmt.Sprintf("cleared %d stale mark(s) after complete scan", removed)
		}
		m.cacheSaves.markSuccessful(msg.generation)
		if m.store != nil && m.cacheWritable {
			return m, tea.Batch(saveCmd(m.ctx, &m.cacheSaves, m.store, m.rootAbs, m.fingerprint, msg), inspect, m.loadPressureCmd())
		}
		return m, tea.Batch(inspect, m.loadPressureCmd())

	case cacheSavedMsg:
		if msg.generation != m.scanGeneration {
			return m, nil
		}
		if msg.err != nil {
			m.cacheErr = fmt.Errorf("cache save failed: %w", msg.err)
		} else {
			m.cacheErr = nil
		}
		return m, nil

	case inspectMsg:
		if msg.generation != m.inspectGeneration {
			return m, nil
		}
		m.inspectPath, m.inspectEntry, m.inspectPreview, m.inspectErr = msg.path, msg.entry, msg.preview, msg.err
		return m, nil

	case stagedMsg:
		if msg.err != nil {
			m.managementError = msg.err.Error()
			return m, nil
		}
		candidate := append(append([]fsops.Operation(nil), m.queue...), msg.operations...)
		normalized, err := normalizeAndValidateQueue(m.rootAbs, candidate)
		if err != nil {
			m.management = managementReview
			m.managementError = err.Error()
			return m, nil
		}
		dropped := len(candidate) - len(normalized)
		if err := m.validateQueuePolicy(normalized); err != nil {
			m.management = managementReview
			m.managementError = err.Error()
			return m, nil
		}
		m.queue = normalized
		m.resetQueueReview()
		if dropped > 0 {
			m.managementNote = fmt.Sprintf("Collapsed %d duplicate or nested operation(s).", dropped)
		}
		m.management, m.managementInput, m.managementError = managementReview, "", ""
		return m, nil

	case dryRunMsg:
		if m.applyCancel != nil {
			m.applyCancel()
			m.applyCancel = nil
		}
		m.applyResults = msg.results
		m.management = managementReview
		m.managementError = ""
		m.managementDryRun = msg.err == nil && len(msg.results) == len(m.queue)
		for _, result := range msg.results {
			if result.Status != fsops.ResultStatusOK || !result.DryRun {
				m.managementDryRun = false
				break
			}
		}
		switch {
		case msg.err != nil:
			m.managementError = "dry-run failed: " + msg.err.Error()
			m.managementNote = "Resolve the failing operation before applying the queue."
		case m.managementDryRun:
			m.managementNote = fmt.Sprintf("Dry-run passed for all %d operation(s).", len(m.queue))
		default:
			m.managementError = "dry-run did not validate the complete queue"
		}
		return m, nil

	case exportedPlanMsg:
		m.management = managementReview
		m.managementInput = ""
		if msg.err != nil {
			m.managementError = "export plan: " + msg.err.Error()
			return m, nil
		}
		m.managementError = ""
		m.managementNote = "Exported guarded plan to " + msg.path
		return m, m.startScan()

	case pressureLoadedMsg:
		m.volume, m.volumeLoadedAt, m.volumeErr = msg.volume, msg.loadedAt, msg.err
		return m, nil

	case growthLoadedMsg:
		if msg.generation != m.analysisGeneration {
			return m, nil
		}
		m.cancelAnalysis()
		m.growth = msg.state
		m.growth.loading = false
		m.cursor, m.offset = 0, 0
		return m, nil

	case openDeletedLoadedMsg:
		if msg.generation != m.analysisGeneration {
			return m, nil
		}
		m.cancelAnalysis()
		m.openDeleted = msg.state
		m.openDeleted.loading = false
		m.cursor, m.offset = 0, 0
		return m, nil

	case destinationLoadedMsg:
		if msg.generation != m.destination.generation || m.management != managementDestination {
			return m, nil
		}
		m.destination = msg.state
		m.destination.loading = false
		m.destination.cursor = min(max(0, m.destination.cursor), max(0, len(m.destination.entries)-1))
		return m, nil

	case appliedMsg:
		if m.applyCancel != nil {
			m.applyCancel()
			m.applyCancel = nil
		}
		m.applyResults = msg.results
		completed, changed, needsScan := m.applyCompletedOperations(msg.results)
		needsScan = needsScan || m.applyNeedsScan || m.auditMutationNeedsReconciliation()
		failure := applyFailureMessage(msg.results, msg.err)
		if failure != "" {
			// An apply error or cancellation can follow a partial recursive
			// delete/copy/move or a completed mutation whose audit failed. The
			// result is therefore never evidence that the measured tree is exact.
			needsScan = true
		}
		for _, result := range msg.results {
			if resultNeedsReconciliation(result) {
				needsScan = true
				break
			}
		}
		m.applyNeedsScan = false
		if len(completed) > 0 {
			m.clearCompletedOperationMarks(completed)
			remaining := m.queue[:0]
			for _, op := range m.queue {
				if !completed[op.ID] {
					remaining = append(remaining, op)
				}
			}
			m.queue = remaining
			if len(m.queue) == 0 {
				m.marks = make(map[string]bool)
			}
		}
		if failure != "" {
			m.management, m.managementError = managementResult, failure
			return m, m.afterFilesystemMutation(changed, needsScan)
		}
		m.queue, m.marks = nil, make(map[string]bool)
		m.management, m.managementError = managementResult, ""
		return m, m.afterFilesystemMutation(changed, needsScan)

	case externalDoneMsg:
		if msg.err != nil {
			m.managementError = msg.kind + " failed: " + msg.err.Error()
		} else {
			m.managementError = ""
		}
		if msg.kind == "pager" {
			return m, nil
		}
		// Editors and shells can save changes and still return nonzero. Always
		// reconcile after control returns while preserving any process error.
		return m, m.startScan()

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampOffset()
		if m.management == managementReview {
			m.clampQueueReview()
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey routes keystrokes to the active view's controller.
func (m *model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.management != managementNone {
		return m.handleManagementKey(k)
	}
	if m.targeting {
		switch k.String() {
		case keyCtrlC:
			m.cancelAnalysis()
			m.cancelScan()
			return m, tea.Quit
		case keyEscape:
			m.targeting, m.targetInput = false, ""
			return m, nil
		case keyEnter:
			if err := m.applyTargetInput(); err != nil {
				m.managementError = err.Error()
				return m, nil
			}
			m.managementError = ""
			m.scanNote = "reclaim target updated"
			return m, nil
		case keyBackspace:
			m.managementError = ""
			input := []rune(m.targetInput)
			if len(input) > 0 {
				m.targetInput = string(input[:len(input)-1])
			}
			return m, nil
		default:
			if k.Type == tea.KeyRunes {
				m.managementError = ""
				m.targetInput += string(k.Runes)
			}
			return m, nil
		}
	}
	if m.filtering {
		switch k.String() {
		case keyCtrlC:
			m.cancelScan()
			return m, tea.Quit
		case keyEscape:
			m.filtering = false
			m.filterInput = m.filter
			return m, nil
		case keyEnter:
			m.filter = m.filterInput
			m.filtering = false
			m.rebuild()
			return m, m.requestInspect()
		case keyBackspace:
			if len(m.filterInput) > 0 {
				r := []rune(m.filterInput)
				m.filterInput = string(r[:len(r)-1])
			}
			return m, nil
		default:
			if k.Type == tea.KeyRunes {
				m.filterInput += string(k.Runes)
			}
			return m, nil
		}
	}
	// Global keys work everywhere.
	switch k.String() {
	case keyCtrlC, "q":
		m.cancelAnalysis()
		m.cancelScan()
		return m, tea.Quit
	case "c":
		if m.cancelLoadingAnalysis() {
			return m, nil
		}
		m.stopScan()
		return m, nil
	case keyEscape:
		if m.cancelLoadingAnalysis() {
			return m, nil
		}
		if m.view == viewHelp {
			m.switchView(m.returnView)
			return m, m.requestInspect()
		} else {
			m.stopScan()
		}
		return m, nil
	case "?":
		if m.view == viewHelp {
			m.switchView(m.returnView)
			return m, m.requestInspect()
		} else {
			m.rememberSelection()
			m.returnView = m.view
			m.view = viewHelp
		}
		return m, nil
	case "/":
		m.filtering = true
		m.filterInput = m.filter
		return m, nil
	case "ctrl+l":
		m.filter, m.filterInput = "", ""
		m.rebuild()
		return m, m.requestInspect()
	case "ctrl+x":
		cleared := len(m.marks)
		m.marks = make(map[string]bool)
		m.scanNote = fmt.Sprintf("cleared %d mark(s)", cleared)
		return m, nil
	case "i", "p":
		m.contextPanel = !m.contextPanel
		if m.contextPanel {
			return m, m.requestInspect()
		}
		return m, nil
	case "f3":
		m.contextPanel = !m.contextPanel
		if m.contextPanel {
			return m, m.requestInspect()
		}
		return m, nil
	case "tab":
		if m.view == viewHelp {
			return m, nil
		}
		m.cycleView()
		return m, m.activateViewCmd()
	case "shift+tab":
		if m.view == viewHelp {
			return m, nil
		}
		m.cycleViewBackward()
		return m, m.activateViewCmd()
	}
	// Help is modal: navigation and mode keys must not change the hidden view.
	if m.view == viewHelp {
		return m, nil
	}
	switch k.String() {
	case "f4":
		return m, m.externalEditorCmd()
	case "f5":
		return m, m.startInput(fsops.ActionCopy)
	case "f6":
		return m, m.startInput(fsops.ActionMove)
	case "f7":
		return m, m.startInput(fsops.ActionMkdir)
	case "f8":
		return m, m.stageDelete()
	case "a":
		m.management, m.managementError = managementReview, ""
		m.resetQueueReview()
		return m, nil
	case "o":
		return m, m.pagerCmd()
	case "!":
		return m, m.shellCmd()
	}

	// Size mode applies to both data views. Sorting is view-specific because
	// extension aggregates do not have a meaningful modification-time field.
	switch k.String() {
	case "m":
		m.rememberSelection()
		if m.sizeMode == tree.SizeOnDisk {
			m.sizeMode = tree.SizeApparent
		} else {
			m.sizeMode = tree.SizeOnDisk
		}
		m.rebuild()
		return m, nil
	case "s":
		m.rememberSelection()
		switch m.view {
		case viewExt:
			m.extSort = nextExtSort(m.extSort)
		case viewTree:
			m.sort = nextSort(m.sort)
		case viewLargest, viewGrowth, viewOpenDeleted, viewHelp:
			return m, nil
		}
		m.rebuild()
		return m, nil
	case "e":
		m.switchView(viewExt)
		return m, m.requestInspect()
	case "f":
		m.switchView(viewLargest)
		return m, m.requestInspect()
	case "t":
		m.switchView(viewTree)
		return m, m.requestInspect()
	case "Y":
		m.switchView(viewGrowth)
		return m, m.startGrowthAnalysis()
	case "O":
		m.switchView(viewOpenDeleted)
		return m, m.startOpenDeletedAnalysis()
	case "T":
		m.beginTargetInput()
		return m, nil
	case "r":
		switch m.view {
		case viewGrowth:
			return m, m.startGrowthAnalysis()
		case viewOpenDeleted:
			return m, m.startOpenDeletedAnalysis()
		case viewTree, viewExt, viewLargest, viewHelp:
			// force rescan; the placeholder tree refreshes as snapshots stream in
			return m, tea.Batch(m.startScan(), m.loadPressureCmd())
		}
	}
	if k.String() == " " && (m.view == viewTree || m.view == viewLargest) {
		if path := m.selectedAbsolutePath(); path != "" {
			m.marks[path] = !m.marks[path]
			if !m.marks[path] {
				delete(m.marks, path)
			}
		}
		return m, nil
	}

	switch m.view {
	case viewExt, viewLargest, viewGrowth, viewOpenDeleted:
		return m.moveOnly(k)
	case viewTree:
		return m.treeKeys(k)
	case viewHelp:
		return m, nil
	}
	return m, nil
}

func (m *model) handleManagementKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == keyCtrlC {
		if m.management == managementApplying || m.management == managementDryRun {
			if m.applyCancel != nil {
				m.applyCancel()
				m.managementError = "canceling…"
			}
			return m, nil
		}
		m.closeManagement()
		m.cancelScan()
		return m, tea.Quit
	}
	if m.management == managementApplying || m.management == managementDryRun {
		if k.String() == keyEscape && m.applyCancel != nil {
			m.applyCancel()
			m.managementError = "canceling…"
		}
		return m, nil
	}
	if m.management == managementDestination && strings.TrimSpace(m.managementInput) == "" {
		if handled, cmd := m.handleDestinationPickerKey(k); handled {
			return m, cmd
		}
	}
	if m.management == managementReview {
		switch k.String() {
		case "up":
			m.moveQueueCursor(-1)
			return m, nil
		case keyDown:
			m.moveQueueCursor(1)
			return m, nil
		case keyPageUp:
			m.moveQueueCursor(-m.reviewPageSize())
			return m, nil
		case keyPageDown:
			m.moveQueueCursor(m.reviewPageSize())
			return m, nil
		case "home":
			m.managementCursor = 0
			m.clampQueueReview()
			return m, nil
		case "end":
			m.managementCursor = max(0, len(m.queue)-1)
			m.clampQueueReview()
			return m, nil
		case "x", "delete":
			m.removeQueuedOperation()
			return m, nil
		case "[":
			m.reorderQueuedOperation(-1)
			return m, nil
		case "]":
			m.reorderQueuedOperation(1)
			return m, nil
		case "v":
			if len(m.queue) == 0 {
				m.managementError = queueEmptyError
				return m, nil
			}
			m.management, m.managementError, m.managementNote = managementDryRun, "", ""
			return m, m.dryRunCmd()
		case "e":
			if len(m.queue) == 0 {
				m.managementError = queueEmptyError
				return m, nil
			}
			if m.app.opts.ReadOnly {
				m.managementError = "read-only mode: plan export is disabled"
				return m, nil
			}
			m.management, m.managementInput, m.managementError = managementExport, "", ""
			return m, nil
		case "d":
			m.queue = nil
			m.closeManagement()
			return m, nil
		}
	}
	switch k.String() {
	case keyEscape:
		if m.management == managementExport {
			m.management, m.managementInput, m.managementError = managementReview, "", ""
			return m, nil
		}
		m.closeManagement()
		return m, nil
	case keyBackspace:
		if m.management == managementDestination || m.management == managementMkdir || m.management == managementExport || m.management == managementConfirm {
			r := []rune(m.managementInput)
			if len(r) > 0 {
				m.managementInput = string(r[:len(r)-1])
			}
		}
		return m, nil
	case keyEnter:
		switch m.management {
		case managementDestination:
			if strings.TrimSpace(m.managementInput) == "" {
				m.managementError = "destination is required"
				return m, nil
			}
			return m, m.stageCmd(m.managementAction, m.actionPaths(), m.managementInput)
		case managementMkdir:
			if strings.TrimSpace(m.managementInput) == "" {
				m.managementError = "directory path is required"
				return m, nil
			}
			return m, m.stageCmd(fsops.ActionMkdir, nil, m.managementInput)
		case managementExport:
			if strings.TrimSpace(m.managementInput) == "" {
				m.managementError = "export path is required"
				return m, nil
			}
			return m, m.exportPlanCmd(m.managementInput)
		case managementReview:
			if len(m.queue) == 0 {
				m.managementError = queueEmptyError
				return m, nil
			}
			if m.app.opts.ReadOnly {
				m.managementError = "read-only mode: apply is disabled"
				return m, nil
			}
			normalized, err := normalizeAndValidateQueue(m.rootAbs, m.queue)
			if err != nil {
				m.managementError = err.Error()
				return m, nil
			}
			if len(normalized) != len(m.queue) {
				m.queue = normalized
				m.resetQueueReview()
				m.managementError = "queue was normalized; review every remaining operation"
				return m, nil
			}
			if err := m.validateQueuePolicy(normalized); err != nil {
				m.managementError = err.Error()
				return m, nil
			}
			if m.managementSeen < len(m.queue) {
				m.managementError = fmt.Sprintf("review all %d operations before confirmation", len(m.queue))
				m.managementCursor = min(m.managementSeen, len(m.queue)-1)
				m.clampQueueReview()
				return m, nil
			}
			m.management, m.managementInput, m.managementError = managementConfirm, "", ""
			return m, nil
		case managementConfirm:
			if m.managementInput != applyConfirm {
				m.managementError = "type APPLY exactly to continue"
				return m, nil
			}
			m.pauseScanForApply()
			m.management, m.managementError = managementApplying, ""
			return m, m.applyCmd()
		case managementResult:
			m.closeManagement()
			return m, nil
		case managementNone, managementDryRun, managementApplying:
			return m, nil
		}
	}
	if k.Type == tea.KeyRunes && (m.management == managementDestination || m.management == managementMkdir || m.management == managementExport || m.management == managementConfirm) {
		m.managementInput += string(k.Runes)
	}
	return m, nil
}

// cycleView rotates the primary browser views (help is reached via '?').
func (m *model) cycleView() {
	switch m.view {
	case viewTree:
		m.switchView(viewExt)
	case viewExt:
		m.switchView(viewLargest)
	case viewLargest, viewGrowth, viewOpenDeleted, viewHelp:
		m.switchView(viewTree)
	}
}

func (m *model) cancelLoadingAnalysis() bool {
	if (m.view != viewGrowth || !m.growth.loading) &&
		(m.view != viewOpenDeleted || !m.openDeleted.loading) {
		return false
	}
	m.cancelAnalysis()
	m.analysisGeneration++
	m.growth.loading, m.openDeleted.loading = false, false
	m.managementError = "analysis canceled"
	return true
}

func (m *model) cycleViewBackward() {
	switch m.view {
	case viewTree:
		m.switchView(viewLargest)
	case viewLargest:
		m.switchView(viewExt)
	case viewExt, viewGrowth, viewOpenDeleted, viewHelp:
		m.switchView(viewTree)
	}
}

func (m *model) switchView(next viewMode) {
	if m.view == next {
		return
	}
	m.rememberSelection()
	m.view = next
	m.offset = 0
	m.restoreSelection()
	m.clampOffset()
	// A view transition can select a different object without moving the
	// cursor. Invalidate in-flight/visible inspection immediately; the caller
	// requests metadata for the new selection.
	m.inspectGeneration++
	m.inspectPath, m.inspectEntry, m.inspectPreview, m.inspectErr = "", fsinfo.Entry{}, nil, nil
}

// treeKeys handles navigation within the tree browser.
func (m *model) treeKeys(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.rows)
	if n == 0 {
		return m, nil
	}
	m.rememberSelection()
	switch k.String() {
	case keyDown, "j":
		m.cursor = min(m.cursor+1, n-1)
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = n - 1
	case keyPageDown, "ctrl+d":
		m.cursor = min(m.cursor+m.page(), n-1)
	case keyPageUp, "ctrl+u":
		m.cursor = max(m.cursor-m.page(), 0)
	case "l", "right", keyEnter:
		m.toggleExpand()
	case "h", "left", "H":
		m.collapseOrUp()
	}
	m.rememberSelection()
	m.clampOffset()
	return m, m.requestInspect()
}

// moveOnly handles the non-tree data views, which are scrollable lists.
func (m *model) moveOnly(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := m.visibleListLen()
	if n == 0 {
		return m, nil
	}
	m.rememberSelection()
	switch k.String() {
	case keyDown, "j":
		m.cursor = min(m.cursor+1, n-1)
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = n - 1
	case keyPageDown, "ctrl+d":
		m.cursor = min(m.cursor+m.page(), n-1)
	case keyPageUp, "ctrl+u":
		m.cursor = max(m.cursor-m.page(), 0)
	}
	m.rememberSelection()
	m.clampOffset()
	return m, m.requestInspect()
}

func (m *model) visibleListLen() int {
	switch m.view {
	case viewTree:
		return len(m.rows)
	case viewExt:
		return len(m.extRows)
	case viewLargest:
		return len(m.topRows)
	case viewGrowth:
		return len(m.growth.deltas)
	case viewOpenDeleted:
		return len(m.openDeleted.result.OpenDeleted)
	case viewHelp:
		return 0
	}
	return 0
}

// toggleExpand opens/closes the directory under the cursor.
func (m *model) toggleExpand() {
	r := m.currentRow()
	if r == nil || !r.node.IsDir {
		return
	}
	key := r.node.Path()
	m.expanded[key] = !m.expanded[key]
	m.rebuild()
}

// collapseOrUp closes the cursor's directory, or moves up to its parent.
func (m *model) collapseOrUp() {
	r := m.currentRow()
	if r == nil {
		return
	}
	if r.node.IsDir && m.expanded[r.node.Path()] {
		m.expanded[r.node.Path()] = false
		m.rebuild()
		return
	}
	// move to parent
	for i := m.cursor - 1; i >= 0; i-- {
		if m.rows[i].depth < r.depth {
			m.cursor = i
			return
		}
	}
}

func (m *model) currentRow() *row {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return nil
	}
	return &m.rows[m.cursor]
}

// page returns the number of rows in one scroll page.
func (m *model) page() int {
	h := m.availHeight()
	if h < 1 {
		return 1
	}
	return h
}

// availHeight is the number of rows available for list content (height minus
// header + footer chrome).
func (m *model) availHeight() int {
	if m.height <= headerLines+footerLines {
		return 1
	}
	return m.height - headerLines - footerLines
}

func nextSort(s tree.SortMode) tree.SortMode {
	order := []tree.SortMode{
		tree.SortSizeDesc, tree.SortCountDesc, tree.SortMTimeDesc, tree.SortNameAsc,
	}
	for i := range order {
		if order[i] == s {
			return order[(i+1)%len(order)]
		}
	}
	return tree.SortSizeDesc
}

func nextExtSort(s extSortMode) extSortMode {
	order := []extSortMode{extSortSize, extSortCount, extSortName}
	for i := range order {
		if order[i] == s {
			return order[(i+1)%len(order)]
		}
	}
	return extSortSize
}

// cacheSavedMsg signals a background cache write finished.
type cacheSavedMsg struct {
	generation uint64
	err        error
}

// saveCmd persists the freshly scanned tree to the cache off the main thread.
func saveCmd(ctx context.Context, saves *cacheSaveCoordinator, store *index.Store, rootAbs, fingerprint string, msg scanDoneMsg) tea.Cmd {
	snap := index.FromTree(msg.node, fingerprint, msg.stats.RootFS,
		msg.stats.Files, msg.stats.Dirs, msg.stats.Errors, msg.stats.Complete, time.Now())
	snap.Root = rootAbs
	return func() tea.Msg {
		_, err := saves.save(msg.generation, func() error { return store.SaveContext(ctx, snap) })
		return cacheSavedMsg{generation: msg.generation, err: err}
	}
}
