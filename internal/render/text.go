// Package render turns a measured tree.Node into human-facing output. The text
// renderer produces an enriched `du`-style listing: a proportional bar, the
// size, a percentage of the total, optional file/dir counts, and tree-shaped
// indentation — plus a by-extension breakdown and a summary footer.
//
// It writes to an io.Writer and is deterministic, so it is fully testable
// against strings.Builder. Colors are plain ANSI gated by TextOptions.Color so
// the output stays pipable and snapshot-stable.
package render

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/phillipod/go-dirstat/internal/agg"
	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/tree"
)

// TextOptions configures the text renderer.
type TextOptions struct {
	Depth    int           // max depth to print; 0 = unlimited
	Limit    int           // max children shown per directory; 0 = unlimited
	Sort     tree.SortMode // child ordering
	Size     tree.SizeMode // apparent vs on-disk
	Bar      bool          // render proportional bars
	BarWidth int           // bar width in cells
	Color    bool          // emit ANSI colors
	Counts   bool          // show file/dir counts
	Bytes    bool          // print raw byte counts instead of human units
	Files    bool          // include individual file rows (du -a); default shows directories only
}

// DefaultTextOptions returns the sensible defaults used by the CLI.
func DefaultTextOptions() TextOptions {
	return TextOptions{
		Sort: tree.SortSizeDesc, Size: tree.SizeOnDisk,
		Bar: true, BarWidth: 16, Color: true, Counts: true,
	}
}

// SummaryData is the small set of totals the footer needs; the CLI fills it
// from scan.Stats so render never depends on the scan package.
type SummaryData struct {
	Files   int
	Dirs    int
	Errors  int64
	Elapsed string
	RootFS  string
}

const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiBold   = "\x1b[1m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
)

// style is a tiny color helper that no-ops when colors are disabled.
type style struct{ on bool }

func (s style) wrap(code, str string) string {
	if !s.on || str == "" {
		return str
	}
	return code + str + ansiReset
}

// barColor returns a magnitude-appropriate ANSI code for a percentage.
func barColor(pct float64) string {
	switch {
	case pct >= 50:
		return ansiRed
	case pct >= 20:
		return ansiYellow
	case pct >= 5:
		return ansiCyan
	default:
		return ansiGreen
	}
}

// sizeValue renders a byte count in the selected raw or human-readable mode.
func (o TextOptions) sizeValue(bytes int64) string {
	if o.Bytes {
		return fmt.Sprintf("%d", bytes)
	}
	return format.Bytes(bytes)
}

// sizeField renders a node's size, right-aligned in a fixed-width column.
func (o TextOptions) sizeField(bytes int64) string {
	// right-align in a 7-wide field so the tree column lines up.
	return fmt.Sprintf("%7s", o.sizeValue(bytes))
}

// Tree writes the enriched du-style tree for root.
func Tree(w io.Writer, root *tree.Node, opts TextOptions) error {
	st := style{on: opts.Color}
	total := root.Size(opts.Size)

	root.Sort(opts.Sort, opts.Size)
	tr := &textRenderer{opts: opts, total: total, st: st}
	return tr.node(w, root, "", true, true, 0)
}

type textRenderer struct {
	opts  TextOptions
	total int64
	st    style
}

func (r *textRenderer) node(w io.Writer, n *tree.Node, prefix string, last, isRoot bool, depth int) error {
	var err error
	if isRoot {
		_, err = fmt.Fprintln(w, r.headerLine(n))
	} else {
		connector := "├── "
		if last {
			connector = "└── "
		}
		_, err = fmt.Fprintln(w, prefix+connector+r.entryLine(n))
	}
	if err != nil {
		return err
	}

	if r.opts.Depth > 0 && depth >= r.opts.Depth {
		return nil
	}

	// By default (du semantics) only directories are listed; the bytes of the
	// files beneath are already folded into each directory's aggregate. Files
	// are shown only when explicitly requested via Files (du -a).
	children := n.Children
	if !r.opts.Files {
		children = onlyDirs(children)
	}
	if r.opts.Limit > 0 && len(children) > r.opts.Limit {
		shown := children[:r.opts.Limit]
		hidden := children[r.opts.Limit:]
		if err := r.printChildren(w, shown, prefix, isRoot, last, depth, false); err != nil {
			return err
		}
		return r.printMore(w, hidden, prefix, isRoot, last)
	}
	return r.printChildren(w, children, prefix, isRoot, last, depth, true)
}

func (r *textRenderer) printChildren(w io.Writer, children []*tree.Node, prefix string, isRoot, parentLast bool, depth int, noMore bool) error {
	childPrefix := prefix
	if !isRoot {
		if parentLast {
			childPrefix += "    "
		} else {
			childPrefix += "│   "
		}
	}
	for i, c := range children {
		cLast := noMore && i == len(children)-1
		if err := r.node(w, c, childPrefix, cLast, false, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// onlyDirs returns the directory children of nodes, dropping file leaves. Used
// for the default du-style listing (directories only).
func onlyDirs(nodes []*tree.Node) []*tree.Node {
	out := nodes[:0:0]
	for _, n := range nodes {
		if n.IsDir {
			out = append(out, n)
		}
	}
	return out
}

func (r *textRenderer) printMore(w io.Writer, hidden []*tree.Node, prefix string, isRoot, parentLast bool) error {
	var sum int64
	for _, c := range hidden {
		sum += c.Size(r.opts.Size)
	}
	childPrefix := prefix
	if !isRoot {
		if parentLast {
			childPrefix += "    "
		} else {
			childPrefix += "│   "
		}
	}
	more := fmt.Sprintf("… %d more entries, %s total", len(hidden), r.opts.sizeValue(sum))
	_, err := fmt.Fprintln(w, childPrefix+"└── "+r.st.wrap(ansiDim, more))
	return err
}

// headerLine renders the root row (no connector).
func (r *textRenderer) headerLine(n *tree.Node) string {
	return r.columns(n, ".")
}

// entryLine renders one non-root row's content (after the tree connector).
func (r *textRenderer) entryLine(n *tree.Node) string {
	name := format.SafeText(n.Name)
	if n.IsDir {
		name += "/"
	}
	return r.columns(n, name)
}

// columns assembles the size / bar / pct / counts / name fields for a node.
func (r *textRenderer) columns(n *tree.Node, name string) string {
	size := n.Size(r.opts.Size)
	pct := format.Pct(size, r.total)
	frac := format.Frac(size, r.total)

	var b strings.Builder
	b.WriteString(r.opts.sizeField(size))

	if r.opts.Bar {
		bar := format.Bar(frac, r.opts.BarWidth)
		b.WriteString("  ")
		b.WriteString(r.st.wrap(barColor(pct), bar))
	}

	b.WriteString("  ")
	pctText := strconv.FormatFloat(pct, 'f', 1, 64)
	b.WriteString(strings.Repeat(" ", max(0, 5-len(pctText))))
	b.WriteString(pctText)
	b.WriteByte('%')

	if r.opts.Counts && n.IsDir {
		b.WriteString("  ")
		b.WriteString(r.st.wrap(ansiDim, r.countField(n)))
	} else if r.opts.Counts && !n.IsDir {
		b.WriteString("  ")
		b.WriteString(strings.Repeat(" ", len(r.countField(n))))
	}

	b.WriteString("  ")
	nameColor := ansiBold
	if !n.IsDir {
		nameColor = "" // files: default weight
	}
	if n.Err != nil {
		name = name + " " + r.st.wrap(ansiRed, "[error]")
	}
	if n.Hardlink {
		name = name + " " + r.st.wrap(ansiDim, "↪") // inode counted under another name
	}
	b.WriteString(r.st.wrap(nameColor, name))
	return b.String()
}

// countField renders "12d 1,204f"-style subtree counts.
func (*textRenderer) countField(n *tree.Node) string {
	if !n.IsDir {
		return ""
	}
	var parts []string
	if n.DirCount > 0 {
		parts = append(parts, format.Count(n.DirCount)+"d")
	}
	if n.FileCount > 0 {
		parts = append(parts, format.Count(n.FileCount)+"f")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// Summary writes the footer with totals, counts, and any scan metadata.
func Summary(w io.Writer, root *tree.Node, s SummaryData, opts TextOptions) error {
	st := style{on: opts.Color}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	total := root.Size(opts.Size)
	_, err := fmt.Fprintf(w, "%s  %s  %s  %s  %s  %s\n",
		st.wrap(ansiBold, "Total: "+opts.sizeValue(total)),
		st.wrap(ansiDim, fmt.Sprintf("%d files", s.Files)),
		st.wrap(ansiDim, fmt.Sprintf("%d dirs", s.Dirs)),
		st.wrap(ansiDim, "in "+s.Elapsed),
		errField(s.Errors, st),
		fsField(s.RootFS, st),
	)
	return err
}

func errField(n int64, st style) string {
	if n == 0 {
		return ""
	}
	return st.wrap(ansiRed, fmt.Sprintf("%d errors", n)) + "  "
}

func fsField(fs string, st style) string {
	if fs == "" {
		return ""
	}
	return st.wrap(ansiBlue, fs)
}

// TopFiles writes a list of the largest files, each with a bar scaled to the
// largest entry in the list.
func TopFiles(w io.Writer, files []agg.FileRef, sm tree.SizeMode, opts TextOptions) error {
	st := style{on: opts.Color}
	if len(files) == 0 {
		return nil
	}
	var maxv int64
	for _, f := range files {
		if f.Size(sm) > maxv {
			maxv = f.Size(sm)
		}
	}
	if _, err := fmt.Fprintln(w, st.wrap(ansiBold, "Largest files")); err != nil {
		return err
	}
	for _, f := range files {
		size := f.Size(sm)
		frac := format.Frac(size, maxv)
		pct := format.Pct(size, maxv)
		bar := ""
		if opts.Bar {
			bar = "  " + st.wrap(barColor(pct), format.Bar(frac, opts.BarWidth))
		}
		if _, err := fmt.Fprintf(w, "%s%s  %s\n", opts.sizeField(size), bar, format.SafeText(f.Rel)); err != nil {
			return err
		}
	}
	return nil
}

// Extensions writes a by-extension breakdown table.
func Extensions(w io.Writer, exts []agg.ExtStat, opts TextOptions) error {
	st := style{on: opts.Color}
	var total int64
	visible := false
	for _, e := range exts {
		if e.Ext != "(dir)" {
			visible = true
			total += e.Size(opts.Size)
		}
	}
	if !visible {
		return nil
	}
	if _, err := fmt.Fprintln(w, st.wrap(ansiBold, "By extension")); err != nil {
		return err
	}
	for _, e := range exts {
		if e.Ext == "(dir)" {
			continue // show file types only in this view
		}
		size := e.Size(opts.Size)
		pct := format.Pct(size, total)
		frac := format.Frac(size, total)
		bar := ""
		if opts.Bar {
			bar = "  " + st.wrap(barColor(pct), format.Bar(frac, opts.BarWidth))
		}
		count := ""
		if opts.Counts {
			count = "  " + st.wrap(ansiDim, fmt.Sprintf("%6s", format.Count(e.Count)))
		}
		if _, err := fmt.Fprintf(w, "%s%s  %5.1f%%%s  %s\n",
			opts.sizeField(size), bar, pct, count, st.wrap(ansiCyan, format.SafeText(e.Ext))); err != nil {
			return err
		}
	}
	return nil
}
