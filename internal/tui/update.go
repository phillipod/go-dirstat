package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phillipod/go-dirstat/internal/fsops"
	"github.com/phillipod/go-dirstat/internal/index"
	"github.com/phillipod/go-dirstat/internal/tree"
)

const (
	keyCtrlC  = "ctrl+c"
	keyEscape = "esc"
	keyEnter  = "enter"
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
		m.completeTree = true
		m.scanErr = nil
		m.rebuild()
		m.cacheSaves.markSuccessful(msg.generation)
		inspect := m.requestInspect()
		if m.store != nil {
			return m, tea.Batch(saveCmd(&m.cacheSaves, m.store, m.rootAbs, m.fingerprint, msg), inspect)
		}
		return m, inspect

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
		m.queue = append(m.queue, msg.operations...)
		m.management, m.managementInput, m.managementError = managementReview, "", ""
		return m, nil

	case appliedMsg:
		if m.applyCancel != nil {
			m.applyCancel()
			m.applyCancel = nil
		}
		m.applyResults = msg.results
		completed, changed, needsScan := m.applyCompletedOperations(msg.results)
		needsScan = needsScan || m.applyNeedsScan || m.auditMutationNeedsReconciliation()
		m.applyNeedsScan = false
		if len(completed) > 0 {
			remaining := m.queue[:0]
			for _, op := range m.queue {
				if !completed[op.ID] {
					remaining = append(remaining, op)
				}
			}
			m.queue, m.marks = remaining, make(map[string]bool)
		}
		if msg.err != nil {
			m.management, m.managementError = managementResult, msg.err.Error()
			return m, m.afterFilesystemMutation(changed, needsScan)
		}
		m.queue, m.marks = nil, make(map[string]bool)
		m.management, m.managementError = managementResult, ""
		return m, m.afterFilesystemMutation(changed, needsScan)

	case externalDoneMsg:
		if msg.err != nil {
			m.managementError = msg.kind + " failed: " + msg.err.Error()
			return m, nil
		}
		m.managementError = ""
		if msg.kind == "pager" {
			return m, nil
		}
		return m, m.startScan()

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampOffset()
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
		case "backspace":
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
		m.cancelScan()
		return m, tea.Quit
	case "c":
		m.stopScan()
		return m, nil
	case keyEscape:
		if m.view == viewHelp {
			m.switchView(m.returnView)
		} else {
			m.stopScan()
		}
		return m, nil
	case "?":
		if m.view == viewHelp {
			m.switchView(m.returnView)
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
		return m, nil
	case "shift+tab":
		if m.view == viewHelp {
			return m, nil
		}
		m.cycleViewBackward()
		return m, nil
	}
	// Help is modal: navigation and mode keys must not change the hidden view.
	if m.view == viewHelp {
		return m, nil
	}
	switch k.String() {
	case "f4":
		return m, m.externalEditorCmd()
	case "f5":
		m.startInput(fsops.ActionCopy)
		return m, nil
	case "f6":
		m.startInput(fsops.ActionMove)
		return m, nil
	case "f7":
		m.startInput(fsops.ActionMkdir)
		return m, nil
	case "f8":
		return m, m.stageDelete()
	case "a":
		m.management, m.managementError = managementReview, ""
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
		case viewLargest, viewHelp:
			return m, nil
		}
		m.rebuild()
		return m, nil
	case "e":
		m.switchView(viewExt)
		return m, nil
	case "f":
		m.switchView(viewLargest)
		return m, nil
	case "t":
		m.switchView(viewTree)
		return m, nil
	case "r":
		// force rescan; the placeholder tree refreshes as snapshots stream in
		return m, m.startScan()
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
	case viewExt, viewLargest:
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
		if m.management == managementApplying {
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
	if m.management == managementApplying {
		if k.String() == keyEscape && m.applyCancel != nil {
			m.applyCancel()
			m.managementError = "canceling…"
		}
		return m, nil
	}
	switch k.String() {
	case keyEscape:
		m.closeManagement()
		return m, nil
	case "backspace":
		if m.management == managementDestination || m.management == managementMkdir || m.management == managementConfirm {
			r := []rune(m.managementInput)
			if len(r) > 0 {
				m.managementInput = string(r[:len(r)-1])
			}
		}
		return m, nil
	case "d":
		if m.management == managementReview {
			m.queue = nil
			m.closeManagement()
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
		case managementReview:
			if len(m.queue) == 0 {
				m.managementError = "queue is empty"
				return m, nil
			}
			if m.app.opts.ReadOnly {
				m.managementError = "read-only mode: apply is disabled"
				return m, nil
			}
			m.management, m.managementInput, m.managementError = managementConfirm, "", ""
			return m, nil
		case managementConfirm:
			if m.managementInput != "APPLY" {
				m.managementError = "type APPLY exactly to continue"
				return m, nil
			}
			m.pauseScanForApply()
			m.management, m.managementError = managementApplying, ""
			return m, m.applyCmd()
		case managementResult:
			m.closeManagement()
			return m, nil
		case managementNone, managementApplying:
			return m, nil
		}
	}
	if k.Type == tea.KeyRunes && (m.management == managementDestination || m.management == managementMkdir || m.management == managementConfirm) {
		m.managementInput += string(k.Runes)
	}
	return m, nil
}

// cycleView rotates tree -> extensions -> largest files -> tree (help is
// reached via '?').
func (m *model) cycleView() {
	switch m.view {
	case viewTree:
		m.switchView(viewExt)
	case viewExt:
		m.switchView(viewLargest)
	case viewLargest, viewHelp:
		m.switchView(viewTree)
	}
}

func (m *model) cycleViewBackward() {
	switch m.view {
	case viewTree:
		m.switchView(viewLargest)
	case viewLargest:
		m.switchView(viewExt)
	case viewExt, viewHelp:
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
}

// treeKeys handles navigation within the tree browser.
func (m *model) treeKeys(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(m.rows)
	if n == 0 {
		return m, nil
	}
	m.rememberSelection()
	switch k.String() {
	case "down", "j":
		m.cursor = min(m.cursor+1, n-1)
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = n - 1
	case "pgdown", "ctrl+d":
		m.cursor = min(m.cursor+m.page(), n-1)
	case "pgup", "ctrl+u":
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
	case "down", "j":
		m.cursor = min(m.cursor+1, n-1)
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = n - 1
	case "pgdown", "ctrl+d":
		m.cursor = min(m.cursor+m.page(), n-1)
	case "pgup", "ctrl+u":
		m.cursor = max(m.cursor-m.page(), 0)
	}
	m.rememberSelection()
	m.clampOffset()
	return m, m.requestInspect()
}

func (m *model) visibleListLen() int {
	if m.view == viewLargest {
		return len(m.topRows)
	}
	return len(m.extRows)
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
func saveCmd(saves *cacheSaveCoordinator, store *index.Store, rootAbs, fingerprint string, msg scanDoneMsg) tea.Cmd {
	return func() tea.Msg {
		snap := index.FromTree(msg.node, fingerprint, msg.stats.RootFS,
			msg.stats.Files, msg.stats.Dirs, msg.stats.Errors, time.Now())
		snap.Root = rootAbs
		_, err := saves.save(msg.generation, func() error { return store.Save(snap) })
		return cacheSavedMsg{generation: msg.generation, err: err}
	}
}
