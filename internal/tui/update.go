package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phillipod/go-dirstat/internal/index"
	"github.com/phillipod/go-dirstat/internal/tree"
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
		m.scanning = true
		m.scanNote = ""
		m.scanErr = nil
		m.rebuild()
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
		m.scanErr = nil
		m.rebuild()
		m.cacheSaves.markSuccessful(msg.generation)
		if m.store != nil {
			return m, saveCmd(&m.cacheSaves, m.store, m.rootAbs, m.fingerprint, msg)
		}
		return m, nil

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
	// Global keys work everywhere.
	switch k.String() {
	case "ctrl+c", "q":
		m.cancelScan()
		return m, tea.Quit
	case "c":
		m.stopScan()
		return m, nil
	case "esc":
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
		default:
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

	switch m.view {
	case viewExt, viewLargest:
		return m.moveOnly(k)
	default:
		return m.treeKeys(k)
	}
}

// cycleView rotates tree -> extensions -> largest files -> tree (help is
// reached via '?').
func (m *model) cycleView() {
	switch m.view {
	case viewTree:
		m.switchView(viewExt)
	case viewExt:
		m.switchView(viewLargest)
	default:
		m.switchView(viewTree)
	}
}

func (m *model) cycleViewBackward() {
	switch m.view {
	case viewTree:
		m.switchView(viewLargest)
	case viewLargest:
		m.switchView(viewExt)
	default:
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
	case " ", "l", "right", "enter":
		m.toggleExpand()
	case "h", "left", "H":
		m.collapseOrUp()
	}
	m.rememberSelection()
	m.clampOffset()
	return m, nil
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
	return m, nil
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
