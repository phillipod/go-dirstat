// Package tree defines the measured-filesystem data model shared by the
// scanner, aggregators, and both renderers. It is the lowest layer of the
// tool and depends only on the standard library so that every higher layer
// (scan → agg → render/tui → cli) can build on it without import cycles.
package tree

import (
	"sort"
	"strings"
	"time"
)

// SizeMode selects which of a node's two measured byte counts to present.
// Both are always captured during the scan; the mode only changes display.
type SizeMode int

const (
	// SizeApparent is the logical file length in bytes (du --apparent-size).
	SizeApparent SizeMode = iota
	// SizeOnDisk is allocated 512-byte blocks (the number plain `du` prints).
	SizeOnDisk
)

// SortMode names an ordering for a node's children. Shared by the text and
// TUI renderers so the same enum drives sorting everywhere.
type SortMode int

const (
	// SortSizeDesc is the default: biggest first.
	SortSizeDesc SortMode = iota
	SortSizeAsc
	SortCountDesc
	SortMTimeDesc
	SortNameAsc
)

// String returns the flag-friendly name of a sort mode.
func (m SortMode) String() string {
	switch m {
	case SortSizeAsc:
		return "size-asc"
	case SortCountDesc:
		return "count"
	case SortMTimeDesc:
		return "mtime"
	case SortNameAsc:
		return "name"
	default:
		return "size"
	}
}

// ParseSort resolves a flag string to a SortMode, defaulting to SortSizeDesc.
func ParseSort(s string) SortMode {
	switch s {
	case "size", "size-desc", "":
		return SortSizeDesc
	case "size-asc":
		return SortSizeAsc
	case "count", "count-desc":
		return SortCountDesc
	case "mtime", "mtime-desc":
		return SortMTimeDesc
	case "name", "name-asc":
		return SortNameAsc
	default:
		return SortSizeDesc
	}
}

// Node is one measured filesystem entry: a file or a directory. Directory
// nodes carry aggregated totals for their subtree (summed bottom-up by the
// scanner), so callers never re-walk to compute a directory's size.
type Node struct {
	Name  string // basename
	IsDir bool
	Depth int

	// Measured sizes — both always populated.
	Apparent int64 // sum of file lengths in the subtree
	Alloc    int64 // sum of allocated 512-byte blocks in the subtree

	// Subtree counts (directories exclude self).
	FileCount int
	DirCount  int

	ModTime time.Time // most recent mtime in the subtree, zero if none

	// Hardlink marks a file identity already counted elsewhere under a different
	// name (a hardlink or followed alias). Such nodes stay visible for navigation
	// but carry zero size so the underlying bytes are counted exactly once.
	Hardlink bool

	// Err carries a non-fatal per-entry error (e.g. permission denied). The
	// scan continues past these; renderers surface them inline.
	Err error

	Children []*Node

	parent *Node
}

// Size returns the byte count selected by mode for this node.
func (n *Node) Size(m SizeMode) int64 {
	if m == SizeOnDisk {
		return n.Alloc
	}
	return n.Apparent
}

// Parent returns the parent node, or nil for the root.
func (n *Node) Parent() *Node { return n.parent }

// Path returns the node's path relative to the scan root ("" for the root
// itself), reconstructed by joining the basenames of its ancestors. It replaces
// a per-node stored relative path: a tree may hold millions of file nodes, and
// caching the full path on each one dominated memory (the basename is already
// stored in Name, and the rest is recoverable from the parent chain). Paths are
// only needed at the edges — for display of a few rows — so computing them
// lazily keeps the steady-state footprint small without losing any information.
func (n *Node) Path() string {
	if n == nil || n.parent == nil {
		return "" // root, or a not-yet-attached node
	}
	parts := []string{n.Name}
	for p := n.parent; p.parent != nil; p = p.parent {
		parts = append(parts, p.Name)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "/")
}

// AddChild attaches c as a child, setting its parent and depth.
func (n *Node) AddChild(c *Node) {
	c.parent = n
	c.Depth = n.Depth + 1
	n.Children = append(n.Children, c)
}

// Adopt attaches an already-measured child, (re)linking its parent pointer and
// reconciling its depth. Used by the scanner when stitching children it built in
// parallel back onto their parent.
func (n *Node) Adopt(c *Node) {
	c.parent = n
	c.Depth = n.Depth + 1
	n.Children = append(n.Children, c)
}

// Sort orders this node's children (and recurses) according to mode.
func (n *Node) Sort(mode SortMode, sm SizeMode) {
	sort.SliceStable(n.Children, func(i, j int) bool {
		return less(n.Children[i], n.Children[j], mode, sm)
	})
	for _, c := range n.Children {
		if c.IsDir {
			c.Sort(mode, sm)
		}
	}
}

func less(a, b *Node, mode SortMode, sm SizeMode) bool {
	switch mode {
	case SortSizeAsc:
		return a.Size(sm) < b.Size(sm)
	case SortCountDesc:
		if a.FileCount+a.DirCount != b.FileCount+b.DirCount {
			return a.FileCount+a.DirCount > b.FileCount+b.DirCount
		}
	case SortMTimeDesc:
		return a.ModTime.After(b.ModTime)
	case SortNameAsc:
		return a.Name < b.Name
	}
	// SortSizeDesc default.
	return a.Size(sm) > b.Size(sm)
}

// Clone returns a deep copy of n and its subtree with parent pointers relinked
// within the copy. It is used to snapshot a tree that a concurrent scan is still
// mutating, so a reader (e.g. the TUI) gets a stable, owned view.
func (n *Node) Clone() *Node {
	return n.clone(nil)
}

func (n *Node) clone(parent *Node) *Node {
	c := n.copyFields(parent)
	c.Children = make([]*Node, len(n.Children))
	for i, ch := range n.Children {
		c.Children[i] = ch.clone(c)
	}
	return c
}

// copyFields duplicates n's scalar (non-structural) state onto a fresh node with
// the given parent and no children. Shared by Clone and ShallowClone.
func (n *Node) copyFields(parent *Node) *Node {
	return &Node{
		Name: n.Name, IsDir: n.IsDir, Depth: n.Depth,
		Apparent: n.Apparent, Alloc: n.Alloc,
		FileCount: n.FileCount, DirCount: n.DirCount,
		ModTime: n.ModTime, Hardlink: n.Hardlink, Err: n.Err,
		parent: parent,
	}
}

// ShallowClone returns a copy of n's subtree truncated to maxDepth levels of
// children (maxDepth 0 = n alone; 1 = n plus its direct children; and so on),
// producing at most cap nodes total. Each copied node carries its current
// aggregate state, so a partial snapshot reads correct running totals even
// though its deeper descendants are omitted.
//
// This is the key to streaming progress on huge trees without freezing or
// OOM: a progress snapshot only needs the few top levels a UI actually shows,
// never the millions of leaves beneath, so its cost is bounded by cap rather
// than by tree size.
func (n *Node) ShallowClone(maxDepth, cap int) *Node {
	if n == nil || cap <= 0 {
		return nil
	}
	root := n.copyFields(nil)
	used := 1
	type frame struct {
		src, dst *Node
		depth    int
	}
	stack := []frame{{src: n, dst: root, depth: 0}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.depth >= maxDepth {
			continue
		}
		for _, ch := range f.src.Children {
			if used >= cap {
				return root
			}
			dc := ch.copyFields(f.dst)
			f.dst.Children = append(f.dst.Children, dc)
			used++
			stack = append(stack, frame{src: ch, dst: dc, depth: f.depth + 1})
		}
	}
	return root
}

// Walk visits n then its subtree depth-first. The callback returns false to
// stop recursing into the current node's children.
func (n *Node) Walk(fn func(*Node) bool) {
	if n == nil {
		return
	}
	if !fn(n) {
		return
	}
	for _, c := range n.Children {
		c.Walk(fn)
	}
}
