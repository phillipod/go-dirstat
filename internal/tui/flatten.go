package tui

import (
	"sort"
	"strings"

	"github.com/phillipod/go-dirstat/internal/agg"
	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/tree"
)

// extRow is one line of the by-extension view.
type extRow struct {
	ext  agg.ExtStat
	frac float64
	pct  float64
}

// topRow is one entry in the largest-files view. Fractions are relative to the
// whole scanned root so its bar and percentage align with the tree view.
type topRow struct {
	file agg.FileRef
	frac float64
	pct  float64
}

const largestFilesLimit = 100

// rebuild recomputes all derived view state (flattened tree rows and the
// extension rows) from the current tree, then keeps the cursor in range. It
// never sorts the measured tree in place: cache persistence may read a finished
// tree concurrently, so presentation ordering is derived from copied child
// slices instead of mutating shared scan data.
func (m *model) rebuild() {
	if m.root == nil {
		return
	}
	m.rows = flatten(m.root, m.expanded, m.sort, m.sizeMode)
	if m.filter != "" {
		m.rows = filterTreeRows(m.rows, m.filter)
	}
	m.extRows = buildExtRows(m.root, m.sizeMode, m.extSort)
	m.topRows = buildTopRows(m.root, m.sizeMode, largestFilesLimit)
	m.restoreSelection()
	m.clampOffset()
}

func filterTreeRows(rows []row, query string) []row {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return rows
	}
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		path := r.node.Path()
		if path == "" {
			path = r.node.Name
		}
		if strings.Contains(strings.ToLower(path), query) {
			out = append(out, r)
		}
	}
	return out
}

func buildTopRows(root *tree.Node, sm tree.SizeMode, limit int) []topRow {
	files := agg.TopFiles(root, sm, limit)
	total := root.Size(sm)
	rows := make([]topRow, len(files))
	for i, file := range files {
		size := file.Size(sm)
		rows[i] = topRow{
			file: file,
			frac: format.Frac(size, total),
			pct:  format.Pct(size, total),
		}
	}
	return rows
}

// flatten produces the visible tree rows: a node is shown, and its children are
// shown only when the node is expanded (root is always expanded).
func flatten(root *tree.Node, expanded map[string]bool, mode tree.SortMode, sm tree.SizeMode) []row {
	var rows []row
	var walk func(n *tree.Node, depth int)
	walk = func(n *tree.Node, depth int) {
		rows = append(rows, row{n, depth})
		if n.IsDir && expanded[n.Path()] {
			children := append([]*tree.Node(nil), n.Children...)
			sort.SliceStable(children, func(i, j int) bool {
				return treeNodeLess(children[i], children[j], mode, sm)
			})
			for _, c := range children {
				walk(c, depth+1)
			}
		}
	}
	walk(root, 0)
	return rows
}

func treeNodeLess(a, b *tree.Node, mode tree.SortMode, sm tree.SizeMode) bool {
	switch mode {
	case tree.SortSizeAsc:
		return a.Size(sm) < b.Size(sm)
	case tree.SortCountDesc:
		ac, bc := a.FileCount+a.DirCount, b.FileCount+b.DirCount
		if ac != bc {
			return ac > bc
		}
	case tree.SortMTimeDesc:
		return a.ModTime.After(b.ModTime)
	case tree.SortNameAsc:
		return a.Name < b.Name
	}
	return a.Size(sm) > b.Size(sm)
}

// buildExtRows precomputes the extension breakdown rows with their fractions.
func buildExtRows(root *tree.Node, sm tree.SizeMode, mode extSortMode) []extRow {
	exts := agg.Extensions(root, sm)
	var total int64
	for _, e := range exts {
		total += e.Size(sm)
	}
	out := make([]extRow, 0, len(exts))
	for _, e := range exts {
		size := e.Size(sm)
		out = append(out, extRow{
			ext:  e,
			frac: format.Frac(size, total),
			pct:  format.Pct(size, total),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].ext, out[j].ext
		switch mode {
		case extSortCount:
			if a.Count != b.Count {
				return a.Count > b.Count
			}
		case extSortName:
			return a.Ext < b.Ext
		default:
			if a.Size(sm) != b.Size(sm) {
				return a.Size(sm) > b.Size(sm)
			}
		}
		return a.Ext < b.Ext
	})
	return out
}

// dataView returns the list whose cursor remains active while help is open.
func (m *model) dataView() viewMode {
	if m.view == viewHelp {
		return m.returnView
	}
	return m.view
}

// rememberSelection captures identity rather than row number. Live snapshots
// and sort changes can reorder rows; retaining only the cursor index would make
// the highlight silently jump to a different file or directory.
func (m *model) rememberSelection() {
	switch m.dataView() {
	case viewExt:
		if m.cursor >= 0 && m.cursor < len(m.extRows) {
			m.selectedExt = m.extRows[m.cursor].ext.Ext
		}
	case viewLargest:
		if m.cursor >= 0 && m.cursor < len(m.topRows) {
			m.selectedFile = m.topRows[m.cursor].file.Rel
		}
	default:
		if r := m.currentRow(); r != nil {
			m.selectedPath = r.node.Path()
		}
	}
}

func (m *model) restoreSelection() {
	switch m.dataView() {
	case viewExt:
		m.restoreExtSelection()
	case viewLargest:
		m.restoreFileSelection()
	default:
		m.restoreTreeSelection()
	}
}

func (m *model) restoreFileSelection() {
	if len(m.topRows) == 0 {
		m.cursor = 0
		return
	}
	for i := range m.topRows {
		if m.topRows[i].file.Rel == m.selectedFile {
			m.cursor = i
			return
		}
	}
	m.cursor = 0
	if m.selectedFile == "" {
		m.selectedFile = m.topRows[0].file.Rel
	}
}

func (m *model) restoreExtSelection() {
	if len(m.extRows) == 0 {
		m.cursor = 0
		return
	}
	for i := range m.extRows {
		if m.extRows[i].ext.Ext == m.selectedExt {
			m.cursor = i
			return
		}
	}
	m.cursor = 0
	if m.selectedExt == "" {
		m.selectedExt = m.extRows[0].ext.Ext
	}
}

func (m *model) restoreTreeSelection() {
	if len(m.rows) == 0 {
		m.cursor = 0
		return
	}
	// A shallow progress snapshot may not contain the selected descendant yet.
	// Select its nearest visible ancestor for now without forgetting the desired
	// path; the exact row is restored automatically when a fuller snapshot lands.
	for target := m.selectedPath; ; target = parentPath(target) {
		for i := range m.rows {
			if m.rows[i].node.Path() == target {
				m.cursor = i
				return
			}
		}
		if target == "" {
			break
		}
	}
	m.cursor = 0
}

func parentPath(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

// clampOffset scrolls just enough to keep the cursor visible.
func (m *model) clampOffset() {
	avail := m.availHeight()
	if avail <= 0 {
		return
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+avail {
		m.offset = m.cursor - avail + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}
