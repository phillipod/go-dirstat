package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/fsops"
	"github.com/phillipod/go-dirstat/internal/tree"
	"github.com/phillipod/go-dirstat/internal/version"
)

// Lines of chrome above and below the scrolling list. availHeight subtracts
// these from the window height.
const (
	headerLines = 1
	footerLines = 2
)

func (m *model) View() string {
	// No data ever arrived and the scan failed outright (e.g. bad path): show the
	// error instead of an empty tree.
	if m.scanErr != nil && !m.gotData {
		return "dirstat: " + format.SafeText(m.scanErr.Error()) + "\n"
	}
	// Not sized yet (before the first WindowSizeMsg): render a minimal title line.
	if m.width == 0 {
		return titleStyle.Render("dirstat ") + dirStyle.Render(format.SafeText(m.app.path)) + "\n"
	}
	var b strings.Builder
	b.WriteString(m.headerView())
	b.WriteString("\n")
	var body string
	if m.management != managementNone {
		body = m.managementBody()
	} else {
		switch m.view {
		case viewExt:
			body = m.extBody()
		case viewLargest:
			body = m.topBody()
		case viewHelp:
			body = m.helpBody()
		default:
			body = m.treeBody()
		}
	}
	body = strings.TrimRight(body, "\n")
	if m.management == managementNone && m.showContextPanel() && m.view != viewHelp {
		body = renderColumns(body, m.contextBody(), m.bodyWidth(), m.contextWidth())
	}
	if body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	}
	b.WriteString(m.footerView())
	return b.String()
}

func (m *model) headerView() string {
	total := format.Bytes(m.root.Size(m.sizeMode))
	title := titleStyle.Render("dirstat ") + dirStyle.Render(format.SafeText(m.app.path))
	stats := dimStyle.Render(fmt.Sprintf("%s total · %s files · %s dirs",
		total, format.Count(m.stats.Files), format.Count(m.stats.Dirs)))

	badges := []string{dimStyle.Render("[" + viewName(m.view) + "]")}
	switch m.view {
	case viewTree:
		badges = append(badges, dimStyle.Render("sort:"+m.sort.String()), dimStyle.Render(modeLabel(m.sizeMode)))
	case viewExt:
		badges = append(badges, dimStyle.Render("sort:"+m.extSort.String()), dimStyle.Render(modeLabel(m.sizeMode)))
	case viewLargest:
		badges = append(badges, dimStyle.Render("top:100"), dimStyle.Render(modeLabel(m.sizeMode)))
	}
	if m.scanning {
		badges = append(badges, badgeStyle.Render("scanning…"))
	}
	if m.scanNote != "" {
		badges = append(badges, badgeStyle.Render(m.scanNote))
	}
	if m.cacheNote != "" {
		badges = append(badges, badgeStyle.Render(m.cacheNote))
	}
	if m.scanErr != nil {
		badges = append(badges, errStyle.Render("scan failed: "+format.SafeText(m.scanErr.Error())))
	}
	if m.cacheErr != nil {
		badges = append(badges, errStyle.Render(format.SafeText(m.cacheErr.Error())))
	}
	if m.stats.Errors > 0 {
		badges = append(badges, errStyle.Render(fmt.Sprintf("%d err", m.stats.Errors)))
	}
	if len(m.marks) > 0 {
		badges = append(badges, badgeStyle.Render(fmt.Sprintf("marked:%d", len(m.marks))))
	}
	if m.app.opts.ReadOnly {
		badges = append(badges, dimStyle.Render("read-only"))
	}
	if len(m.queue) > 0 {
		badges = append(badges, badgeStyle.Render(fmt.Sprintf("queued:%d", len(m.queue))))
	}
	if m.filter != "" {
		badges = append(badges, dimStyle.Render("filter:"+format.SafeText(m.filter)))
	}
	return joinLine(m.width, title+"  "+stats, strings.Join(badges, " "))
}

func (m *model) footerView() string {
	if m.management != managementNone {
		line1 := m.managementError
		if line1 == "" {
			line1 = m.detailLine()
		}
		return truncate(line1, m.width) + "\n" + truncate(helpStyle.Render(m.managementHelp()), m.width)
	}
	if m.managementError != "" {
		return truncate(errStyle.Render(m.managementError), m.width) + "\n" + truncate(helpStyle.Render("F3 preview · F4 edit · F5 copy · F6 move · F7 mkdir · F8 delete · a review"), m.width)
	}
	line1 := truncate(m.detailLine(), m.width)
	keys := "? close help · q quit"
	if m.filtering {
		return truncate(m.detailLine(), m.width) + "\n" + truncate(helpStyle.Render("filter: "+m.filterInput+"█  Enter apply · Esc cancel"), m.width)
	}
	switch m.view {
	case viewTree:
		keys = "↑/↓ move · F3 preview · F4 edit · o pager · ! shell · F5 copy · F6 move · F7 mkdir · F8 delete · a review"
	case viewExt:
		keys = "↑/↓ move · / search · t tree · f files · s sort · m mode · r rescan · ? help · q quit"
	case viewLargest:
		keys = "↑/↓ move · Space mark · / search · i inspect · F8 delete · t/e views · ? help"
	}
	line2 := helpStyle.Render(keys)
	return line1 + "\n" + truncate(line2, m.width)
}

func (m *model) managementBody() string {
	w := m.bodyWidth()
	lines := []string{titleStyle.Render("Filesystem actions"), ""}
	switch m.management {
	case managementDestination:
		lines = append(lines, fmt.Sprintf("%s %d selected path(s)", m.managementAction, len(m.actionPaths())), "", "Destination:", m.managementInput+"█")
	case managementMkdir:
		lines = append(lines, "Create directory", "", "Path (relative to scan root or absolute):", m.managementInput+"█")
	case managementReview:
		if len(m.queue) == 0 {
			lines = append(lines, dimStyle.Render("Queue is empty."))
			break
		}
		for i, op := range m.queue {
			line := fmt.Sprintf("%2d  %-7s %s", i+1, op.Action, format.SafeText(op.Source))
			if op.Destination != "" {
				line += " → " + format.SafeText(op.Destination)
			}
			lines = append(lines, truncate(line, w))
		}
	case managementConfirm:
		lines = append(lines, errStyle.Render(fmt.Sprintf("Apply %d guarded operation(s)?", len(m.queue))), "", "Type APPLY exactly:", m.managementInput+"█")
	case managementApplying:
		lines = append(lines, badgeStyle.Render(fmt.Sprintf("Applying %d operation(s)…", len(m.queue))), "", dimStyle.Render("Each source is revalidated against its captured identity, size, and modification time."))
	case managementResult:
		if m.managementError == "" {
			status := fmt.Sprintf("Applied %d operation(s). View updated.", successfulApplyCount(m.applyResults))
			if m.scanning {
				status = fmt.Sprintf("Applied %d operation(s). Reconciling…", successfulApplyCount(m.applyResults))
			}
			lines = append(lines, badgeStyle.Render(status))
		} else {
			lines = append(lines, errStyle.Render("Apply stopped: "+format.SafeText(m.managementError)))
			if successfulApplyCount(m.applyResults) > 0 {
				status := "Successful changes are reflected in the current view."
				if m.scanning {
					status = "Successful changes are reflected; reconciling in the background."
				}
				lines = append(lines, dimStyle.Render(status))
			}
		}
		for _, result := range m.applyResults {
			line := fmt.Sprintf("%-7s %-7s %s", result.Status, result.Action, result.OperationID)
			if result.Error != "" {
				line += " · " + format.SafeText(result.Error)
			}
			lines = append(lines, truncate(line, w))
		}
	}
	if m.managementError != "" && m.management != managementResult {
		lines = append(lines, "", errStyle.Render(format.SafeText(m.managementError)))
	}
	if len(lines) > m.availHeight() {
		lines = lines[:m.availHeight()]
	}
	for i := range lines {
		lines[i] = truncate(lines[i], w)
	}
	return strings.Join(lines, "\n")
}

func successfulApplyCount(results []fsops.Result) int {
	count := 0
	for _, result := range results {
		if result.Status == "ok" && !result.DryRun {
			count++
		}
	}
	return count
}

func (m *model) managementHelp() string {
	switch m.management {
	case managementDestination, managementMkdir, managementConfirm:
		return "Enter continue · Esc cancel"
	case managementReview:
		return "Enter apply · d discard queue · Esc return"
	case managementApplying:
		return "Esc cancel between entries"
	case managementResult:
		return "Enter/Esc close"
	default:
		return "Esc close"
	}
}

// detailLine shows rich info for the selected node (the "dirstat goodness").
func (m *model) detailLine() string {
	if m.view == viewTree {
		if r := m.currentRow(); r != nil {
			return m.nodeDetail(r.node)
		}
	}
	if m.view == viewExt && m.cursor >= 0 && m.cursor < len(m.extRows) {
		r := m.extRows[m.cursor]
		kind := "files"
		if r.ext.Ext == "(dir)" {
			kind = "directories"
		}
		return dimStyle.Render(fmt.Sprintf("%s · %s · %s %s · %.1f%% of total",
			format.SafeText(r.ext.Ext), format.Bytes(r.ext.Size(m.sizeMode)), format.Count(r.ext.Count), kind, r.pct))
	}
	if m.view == viewLargest && m.cursor >= 0 && m.cursor < len(m.topRows) {
		r := m.topRows[m.cursor]
		path := r.file.Rel
		if path == "" {
			path = "."
		}
		return dimStyle.Render(fmt.Sprintf("%s · %s · %.1f%% of total",
			format.SafeText(path), format.Bytes(r.file.Size(m.sizeMode)), r.pct))
	}
	return ""
}

func (m *model) nodeDetail(n *tree.Node) string {
	size := format.Bytes(n.Size(m.sizeMode))
	pct := fmt.Sprintf("%.1f%%", format.Pct(n.Size(m.sizeMode), m.root.Size(m.sizeMode)))
	rel := n.Path()
	if rel == "" {
		rel = "."
	}
	lead := dirStyle.Render(format.SafeText(rel)) + "  " + dimStyle.Render(size+" · "+pct)
	if n.Err != nil {
		return truncate(lead+" "+errStyle.Render("· "+format.SafeText(n.Err.Error())), m.width)
	}
	if !n.IsDir {
		return truncate(lead, m.width)
	}
	return truncate(lead+dimStyle.Render(fmt.Sprintf(" · %s files · %s subdirs", format.Count(n.FileCount), format.Count(n.DirCount))), m.width)
}

// treeBody renders the visible window of flattened tree rows.
func (m *model) treeBody() string {
	w := m.bodyWidth()
	barW := barWidth(w)
	avail := m.availHeight()
	var b strings.Builder
	end := m.offset + avail
	if end > len(m.rows) {
		end = len(m.rows)
	}
	for i := m.offset; i < end; i++ {
		b.WriteString(m.renderTreeRow(m.rows[i], i == m.cursor, barW))
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (m *model) renderTreeRow(r row, selected bool, barW int) string {
	w := m.bodyWidth()
	n := r.node
	indent := strings.Repeat("  ", r.depth)
	glyph := " "
	if n.IsDir {
		if m.expanded[n.Path()] {
			glyph = "▾"
		} else {
			glyph = "▸"
		}
	}
	size := fmt.Sprintf("%6s", format.Bytes(n.Size(m.sizeMode)))
	pct := fmt.Sprintf("%5.1f%%", format.Pct(n.Size(m.sizeMode), m.root.Size(m.sizeMode)))
	bar := format.Bar(format.Frac(n.Size(m.sizeMode), m.root.Size(m.sizeMode)), barW)

	name := format.SafeText(n.Name)
	if n.IsDir {
		name += "/"
		if n.Err == nil {
			name = dirStyle.Render(name)
		}
	}
	if n.Err != nil {
		name = errStyle.Render(name + " [error]")
	}
	if n.Hardlink {
		name = name + " " + dimStyle.Render("↪") // inode counted under another name
	}

	mark := " "
	abs := m.absoluteNodePath(n)
	if m.marks[abs] {
		mark = "●"
	}
	left := mark + indent + glyph + " "
	mid := size + " " + barColorVal(format.Pct(n.Size(m.sizeMode), m.root.Size(m.sizeMode)), bar) + " " + pct + "  "
	// budget for the name column so long names do not wrap.
	budget := w - lipgloss.Width(left) - lipgloss.Width(size) - barW - lipgloss.Width(pct) - 4
	if budget > 4 {
		name = truncate(name, budget)
	}
	line := truncate(left+mid+name, w)
	if selected {
		line = cursorBg.Render(line)
	}
	return line
}

// barColorVal colors a rendered bar string by magnitude.
func barColorVal(pct float64, bar string) string {
	return lipgloss.NewStyle().Foreground(barColor(pct)).Render(bar)
}

// extBody renders the by-extension list.
func (m *model) extBody() string {
	w := m.bodyWidth()
	barW := barWidth(w)
	avail := m.availHeight()
	var b strings.Builder
	end := m.offset + avail
	if end > len(m.extRows) {
		end = len(m.extRows)
	}
	for i := m.offset; i < end; i++ {
		r := m.extRows[i]
		size := fmt.Sprintf("%6s", format.Bytes(r.ext.Size(m.sizeMode)))
		pct := fmt.Sprintf("%5.1f%%", r.pct)
		bar := barColorVal(r.pct, format.Bar(r.frac, barW))
		line := truncate(size+" "+bar+" "+pct+"  "+dimStyle.Render(fmt.Sprintf("%6s", format.Count(r.ext.Count)))+"  "+extStyle.Render(format.SafeText(r.ext.Ext)), w)
		if i == m.cursor {
			line = cursorBg.Render(line)
		}
		b.WriteString(line)
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// topBody renders the largest files, scaled against the whole scan so bars are
// directly comparable with directory rows rather than only with one another.
func (m *model) topBody() string {
	w := m.bodyWidth()
	barW := barWidth(w)
	avail := m.availHeight()
	var b strings.Builder
	end := min(m.offset+avail, len(m.topRows))
	for i := m.offset; i < end; i++ {
		r := m.topRows[i]
		size := fmt.Sprintf("%6s", format.Bytes(r.file.Size(m.sizeMode)))
		pct := fmt.Sprintf("%5.1f%%", r.pct)
		bar := barColorVal(r.pct, format.Bar(r.frac, barW))
		path := r.file.Rel
		if path == "" {
			path = "."
		}
		mark := " "
		abs := m.absoluteRelPath(path)
		if m.marks[abs] {
			mark = "●"
		}
		line := truncate(mark+size+" "+bar+" "+pct+"  "+format.SafeText(path), w)
		if i == m.cursor {
			line = cursorBg.Render(line)
		}
		b.WriteString(line)
		if i < end-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (m *model) helpBody() string {
	rows := []struct{ key, desc string }{
		{"↑/↓ or k/j", "move selection"},
		{"Enter / l", "expand or collapse directory"},
		{"Space", "mark or unmark selected path"},
		{"/ / Ctrl+L", "search paths / clear filter"},
		{"i / p", "toggle metadata/content context pane"},
		{"F3 / F4", "preview pane / configured external editor"},
		{"o / !", "configured pager / shell in selected directory"},
		{"F5 / F6", "queue guarded copy / move"},
		{"F7 / F8", "queue mkdir / staged delete"},
		{"a", "review queue and apply with typed confirmation"},
		{"h / ←", "collapse, or jump to parent"},
		{"g / G", "jump to top / bottom"},
		{"PgUp/PgDn", "scroll by a page"},
		{"s (tree)", "cycle sort: size → count → mtime → name"},
		{"s (extensions)", "cycle sort: size → count → name"},
		{"m", "toggle on-disk / apparent size"},
		{"e / t", "extensions view / tree view"},
		{"f", "largest files view (top 100)"},
		{"Tab / Shift+Tab", "cycle views forward / backward"},
		{"r", "rescan (force refresh)"},
		{"c / Esc", "stop active scan; retain current results"},
		{"?", "toggle this help"},
		{"q / Ctrl+C", "quit"},
	}
	lines := []string{
		truncate(titleStyle.Render("dirstat ")+dimStyle.Render(version.Info()), m.width),
		"",
	}
	for _, r := range rows {
		lines = append(lines, truncate(fmt.Sprintf("  %-14s %s", titleStyle.Render(r.key), helpStyle.Render(r.desc)), m.width))
	}
	if avail := m.availHeight(); len(lines) > avail {
		lines = lines[:avail]
	}
	return strings.Join(lines, "\n")
}

func (m *model) showContextPanel() bool { return m.contextPanel && m.width >= 120 }

func (m *model) contextWidth() int {
	if !m.showContextPanel() {
		return 0
	}
	return max(36, m.width/3)
}

func (m *model) bodyWidth() int {
	if m.management != managementNone {
		return m.width
	}
	if !m.showContextPanel() {
		return m.width
	}
	return m.width - m.contextWidth() - 1
}

func (m *model) absoluteRelPath(rel string) string {
	if rel == "" || rel == "." {
		return m.rootAbs
	}
	return filepath.Join(m.rootAbs, filepath.FromSlash(rel))
}

func (m *model) absoluteNodePath(n *tree.Node) string {
	if n == nil {
		return ""
	}
	return m.absoluteRelPath(n.Path())
}

func (m *model) contextBody() string {
	w := m.contextWidth()
	if m.view == viewExt {
		return truncate(titleStyle.Render("Extension analysis"), w) + "\n\n" + truncate(m.detailLine(), w)
	}
	if m.inspectPath == "" {
		return truncate(titleStyle.Render("Inspect"), w) + "\n\n" + dimStyle.Render("Move selection to load metadata")
	}
	var lines []string
	lines = append(lines, truncate(titleStyle.Render("Inspect"), w), truncate(format.SafeText(m.inspectPath), w), "")
	if m.inspectErr != nil {
		lines = append(lines, truncate(errStyle.Render(format.SafeText(m.inspectErr.Error())), w))
		return strings.Join(lines, "\n")
	}
	e := m.inspectEntry
	lines = append(lines,
		truncate(fmt.Sprintf("%s  %s", e.Kind, e.ModeText), w),
		truncate(fmt.Sprintf("size %s · allocated %s", format.Bytes(e.Size), format.Bytes(e.Allocated)), w),
		truncate(fmt.Sprintf("owner %s:%s · links %d", e.Owner, e.Group, e.Links), w),
		truncate("modified "+e.ModTime.Format("2006-01-02 15:04:05"), w),
	)
	if e.Symlink != "" {
		lines = append(lines, truncate("→ "+format.SafeText(e.Symlink), w))
	}
	if m.inspectPreview != nil {
		lines = append(lines, "", dimStyle.Render("preview"))
		body := m.inspectPreview.Text
		if m.inspectPreview.Binary {
			body = m.inspectPreview.Hex
		}
		for _, line := range strings.Split(body, "\n") {
			if len(lines) >= m.availHeight() {
				break
			}
			lines = append(lines, truncate(format.SafeText(line), w))
		}
	}
	return strings.Join(lines, "\n")
}

func renderColumns(left, right string, leftWidth, rightWidth int) string {
	ll, rr := strings.Split(left, "\n"), strings.Split(right, "\n")
	n := max(len(ll), len(rr))
	lines := make([]string, n)
	for i := 0; i < n; i++ {
		l, r := "", ""
		if i < len(ll) {
			l = truncate(ll[i], leftWidth)
		}
		if i < len(rr) {
			r = truncate(rr[i], rightWidth)
		}
		lines[i] = l + strings.Repeat(" ", max(0, leftWidth-lipgloss.Width(l))) + "│" + r
	}
	return strings.Join(lines, "\n")
}

// helpers ----------------------------------------------------------------

func viewName(v viewMode) string {
	switch v {
	case viewExt:
		return "extensions"
	case viewLargest:
		return "largest files"
	case viewHelp:
		return "help"
	default:
		return "tree"
	}
}

func modeLabel(sm tree.SizeMode) string {
	if sm == tree.SizeOnDisk {
		return "on-disk"
	}
	return "apparent"
}

func barWidth(w int) int {
	if w <= 60 {
		return 8
	}
	if w >= 140 {
		return 24
	}
	return 14
}

func joinLine(width int, left, right string) string {
	if width <= 0 {
		return left + right
	}
	if right == "" {
		return truncate(left, width)
	}
	rightWidth := lipgloss.Width(right)
	if rightWidth >= width {
		return truncate(right, width)
	}
	leftWidth := width - rightWidth - 1
	if leftWidth <= 0 {
		return strings.Repeat(" ", width-rightWidth) + right
	}
	left = truncate(left, leftWidth)
	return left + strings.Repeat(" ", width-lipgloss.Width(left)-rightWidth) + right
}

func truncate(s string, max int) string {
	if max <= 0 {
		return s
	}
	return ansi.Truncate(s, max, "…")
}
