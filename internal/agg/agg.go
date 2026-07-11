// Package agg derives the "dirstat goodness" — the secondary views that turn a
// raw measured tree into insight: breakdowns by file extension, the largest
// files, and a per-depth size profile. Everything here is a pure fold over a
// tree.Node, so it is equally usable by the text renderer and the TUI.
package agg

import (
	"container/heap"
	"sort"
	"strings"

	"github.com/phillipod/go-dirstat/internal/tree"
)

// ExtStat summarises all files sharing one extension.
type ExtStat struct {
	Ext      string // lowercased with leading dot, "(none)" if none, "(dir)" for directories
	Count    int
	Apparent int64
	Alloc    int64
}

// Size picks Apparent or Alloc according to the display size mode.
func (e ExtStat) Size(sm tree.SizeMode) int64 {
	if sm == tree.SizeOnDisk {
		return e.Alloc
	}
	return e.Apparent
}

// FileRef points at a single file with its measured sizes.
type FileRef struct {
	Rel      string // path relative to the scan root
	Name     string
	Apparent int64
	Alloc    int64
}

// Size picks Apparent or Alloc according to the display size mode.
func (f FileRef) Size(sm tree.SizeMode) int64 {
	if sm == tree.SizeOnDisk {
		return f.Alloc
	}
	return f.Apparent
}

// Report bundles the derived views for one tree.
type Report struct {
	Extensions    []ExtStat     // sorted by size descending
	TopFiles      []FileRef     // largest files, biggest first
	ByDepth       map[int]int64 // apparent bytes at each depth
	FileCount     int
	DirCount      int
	TotalApparent int64
	TotalAlloc    int64
}

// Extensions walks root and groups regular files by extension, sorted by the
// selected size mode descending. Directories are aggregated into a "(dir)" bucket
// so the on-disk overhead of directory inodes is visible but separable.
func Extensions(root *tree.Node, sm tree.SizeMode) []ExtStat {
	m := make(map[string]*ExtStat)
	bucket := func(key string) *ExtStat {
		e := m[key]
		if e == nil {
			e = &ExtStat{Ext: key}
			m[key] = e
		}
		return e
	}
	root.Walk(func(n *tree.Node) bool {
		if n.IsDir {
			d := bucket("(dir)")
			d.Count++
			// The directory's own inode overhead is its alloc minus what its
			// children already account for; clamp at 0 for incompletely
			// aggregated (e.g. filtered) trees.
			overhead := n.Alloc - dirSubtreeAlloc(n)
			if overhead > 0 {
				d.Alloc += overhead
			}
			return true
		}
		if n.Err != nil {
			return true
		}
		ext := classifyExt(n.Name)
		e := bucket(ext)
		e.Count++
		e.Apparent += n.Apparent
		e.Alloc += n.Alloc
		return true
	})

	out := make([]ExtStat, 0, len(m))
	for _, e := range m {
		out = append(out, *e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Size(sm) != out[j].Size(sm) {
			return out[i].Size(sm) > out[j].Size(sm)
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Ext < out[j].Ext
	})
	return out
}

// dirSubtreeAlloc returns the alloc contributed by a directory's children, so
// the "(dir)" bucket counts only the directory's own inode overhead.
func dirSubtreeAlloc(n *tree.Node) int64 {
	var sum int64
	for _, c := range n.Children {
		sum += c.Alloc
	}
	return sum
}

// classifyExt returns the lowercased extension with a leading dot, or
// "(none)" when the name has no extension.
func classifyExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 && i < len(name)-1 {
		// avoid treating dotfiles like ".gitignore" as ext ".gitignore"
		return strings.ToLower(name[i:])
	}
	return "(none)"
}

// TopFiles returns the n largest files in root by the given size mode. It keeps
// only n candidates (evicting the smallest whenever the set is full) rather than
// materialising every file, so memory stays bounded on very large trees. The
// relative paths of the few survivors are resolved from the parent chain only
// at the end, so the millions of rejected files never allocate a path string.
func TopFiles(root *tree.Node, sm tree.SizeMode, n int) []FileRef {
	if n <= 0 {
		return nil
	}
	top := &nodeMinHeap{sizeMode: sm, nodes: make([]*tree.Node, 0, n)}
	heap.Init(top)
	root.Walk(func(node *tree.Node) bool {
		if node.IsDir {
			return true
		}
		if node.Err != nil || node.Hardlink {
			return true
		}
		if top.Len() < n {
			heap.Push(top, node)
			return true
		}
		if node.Size(sm) > top.nodes[0].Size(sm) {
			top.nodes[0] = node
			heap.Fix(top, 0)
		}
		return true
	})
	out := make([]FileRef, len(top.nodes))
	for i, node := range top.nodes {
		rel := node.Path()
		if rel == "" {
			rel = "."
		}
		out[i] = FileRef{
			Rel: rel, Name: node.Name,
			Apparent: node.Apparent, Alloc: node.Alloc,
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Size(sm) != out[j].Size(sm) {
			return out[i].Size(sm) > out[j].Size(sm)
		}
		return out[i].Rel < out[j].Rel
	})
	return out
}

type nodeMinHeap struct {
	sizeMode tree.SizeMode
	nodes    []*tree.Node
}

func (h nodeMinHeap) Len() int { return len(h.nodes) }
func (h nodeMinHeap) Less(i, j int) bool {
	return h.nodes[i].Size(h.sizeMode) < h.nodes[j].Size(h.sizeMode)
}
func (h nodeMinHeap) Swap(i, j int) { h.nodes[i], h.nodes[j] = h.nodes[j], h.nodes[i] }
func (h *nodeMinHeap) Push(value any) {
	h.nodes = append(h.nodes, value.(*tree.Node))
}
func (h *nodeMinHeap) Pop() any {
	last := len(h.nodes) - 1
	value := h.nodes[last]
	h.nodes[last] = nil
	h.nodes = h.nodes[:last]
	return value
}

// ReportFor computes the full derived report for root using sm for ranked
// views. topN bounds the largest files list.
func ReportFor(root *tree.Node, sm tree.SizeMode, topN int) Report {
	r := Report{
		Extensions:    Extensions(root, sm),
		TopFiles:      TopFiles(root, sm, topN),
		ByDepth:       make(map[int]int64),
		FileCount:     root.FileCount,
		DirCount:      root.DirCount,
		TotalApparent: root.Apparent,
		TotalAlloc:    root.Alloc,
	}
	root.Walk(func(n *tree.Node) bool {
		if !n.IsDir {
			r.ByDepth[n.Depth] += n.Apparent
		}
		return true
	})
	return r
}
