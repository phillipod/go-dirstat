package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/phillipod/go-dirstat/internal/format"
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
	body = strings.TrimRight(body, "\n")
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
	return joinLine(m.width, title+"  "+stats, strings.Join(badges, " "))
}

func (m *model) footerView() string {
	line1 := truncate(m.detailLine(), m.width)
	keys := "? close help · q quit"
	switch m.view {
	case viewTree:
		keys = "↑/↓ move · space expand · h/l collapse/open · e ext · f files · s sort · m mode · r rescan · c stop · ? help · q quit"
	case viewExt:
		keys = "↑/↓ move · t tree · f files · s sort · m mode · r rescan · c stop · ? help · q quit"
	case viewLargest:
		keys = "↑/↓ move · t tree · e ext · m mode · r rescan · c stop · ? help · q quit"
	}
	line2 := helpStyle.Render(keys)
	return line1 + "\n" + truncate(line2, m.width)
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
	barW := barWidth(m.width)
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

	left := indent + glyph + " "
	mid := size + " " + barColorVal(format.Pct(n.Size(m.sizeMode), m.root.Size(m.sizeMode)), bar) + " " + pct + "  "
	// budget for the name column so long names do not wrap.
	budget := m.width - lipgloss.Width(left) - lipgloss.Width(size) - barW - lipgloss.Width(pct) - 4
	if budget > 4 {
		name = truncate(name, budget)
	}
	line := truncate(left+mid+name, m.width)
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
	barW := barWidth(m.width)
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
		line := truncate(size+" "+bar+" "+pct+"  "+dimStyle.Render(fmt.Sprintf("%6s", format.Count(r.ext.Count)))+"  "+extStyle.Render(format.SafeText(r.ext.Ext)), m.width)
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
	barW := barWidth(m.width)
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
		line := truncate(size+" "+bar+" "+pct+"  "+format.SafeText(path), m.width)
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
		{"space / l", "expand or collapse directory"},
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
