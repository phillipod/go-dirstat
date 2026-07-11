package history

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/phillipod/go-dirstat/internal/index"
)

// Change classifies a path's transition between two snapshots.
type Change string

const (
	ChangeNew     Change = "new"
	ChangeGrown   Change = "grown"
	ChangeShrunk  Change = "shrunk"
	ChangeRemoved Change = "removed"
)

// Delta describes one changed path. Byte deltas are after minus before, so
// shrinkage and removal have negative values.
type Delta struct {
	Path           string    `json:"path"`
	Depth          int       `json:"depth"`
	IsDir          bool      `json:"is_directory"`
	Change         Change    `json:"change"`
	BeforeApparent int64     `json:"before_apparent_bytes"`
	AfterApparent  int64     `json:"after_apparent_bytes"`
	ApparentDelta  int64     `json:"apparent_delta_bytes"`
	BeforeAlloc    int64     `json:"before_allocated_bytes"`
	AfterAlloc     int64     `json:"after_allocated_bytes"`
	AllocatedDelta int64     `json:"allocated_delta_bytes"`
	BeforeModTime  time.Time `json:"before_modified_at,omitempty"`
	AfterModTime   time.Time `json:"after_modified_at,omitempty"`
}

// DeltaKind selects file, directory, or all history changes.
type DeltaKind string

const (
	DeltaKindAll       DeltaKind = "all"
	DeltaKindFile      DeltaKind = "file"
	DeltaKindDirectory DeltaKind = "directory"
)

// DeltaFilter controls operational history output without changing the
// underlying snapshots. MaxDepth is relative to the scan root; -1 is
// unlimited. LeafOnly suppresses every changed path that has a changed
// descendant, avoiding simultaneous ancestor and descendant aggregates.
type DeltaFilter struct {
	Kind     DeltaKind
	MaxDepth int
	Limit    int
	LeafOnly bool
}

type measuredNode struct {
	path string
	node index.FlatNode
}

// Compare reports new, grown, shrunk, and removed paths. Unchanged paths are
// omitted. Snapshots must describe the same root and scope fingerprint.
func Compare(previous, current *index.Snapshot) ([]Delta, error) {
	if previous == nil || current == nil {
		return nil, errors.New("history: two snapshots are required")
	}
	if previous.Root != current.Root || previous.Fingerprint != current.Fingerprint {
		return nil, errors.New("history: snapshots have different roots or fingerprints")
	}
	before, err := flattenPaths(previous)
	if err != nil {
		return nil, err
	}
	after, err := flattenPaths(current)
	if err != nil {
		return nil, err
	}
	deltas := make([]Delta, 0)
	for path, old := range before {
		newNode, exists := after[path]
		if !exists {
			deltas = append(deltas, makeDelta(path, old, measuredNode{}, ChangeRemoved))
			continue
		}
		delete(after, path)
		if old.node.Apparent == newNode.node.Apparent && old.node.Alloc == newNode.node.Alloc {
			continue
		}
		change := ChangeGrown
		if newNode.node.Alloc < old.node.Alloc || (newNode.node.Alloc == old.node.Alloc && newNode.node.Apparent < old.node.Apparent) {
			change = ChangeShrunk
		}
		deltas = append(deltas, makeDelta(path, old, newNode, change))
	}
	for path, node := range after {
		deltas = append(deltas, makeDelta(path, measuredNode{}, node, ChangeNew))
	}
	for i := range deltas {
		depth, err := relativeDepth(previous.Root, deltas[i].Path)
		if err != nil {
			return nil, err
		}
		deltas[i].Depth = depth
	}
	sort.Slice(deltas, func(i, j int) bool {
		ai, aj := abs64(deltas[i].AllocatedDelta), abs64(deltas[j].AllocatedDelta)
		if ai != aj {
			return ai > aj
		}
		return deltas[i].Path < deltas[j].Path
	})
	return deltas, nil
}

// FilterDeltas applies kind, depth, leaf, and count controls while preserving
// Compare's deterministic magnitude/path ordering.
func FilterDeltas(deltas []Delta, root string, filter DeltaFilter) ([]Delta, error) {
	if filter.Kind == "" {
		filter.Kind = DeltaKindAll
	}
	if filter.Kind != DeltaKindAll && filter.Kind != DeltaKindFile && filter.Kind != DeltaKindDirectory {
		return nil, fmt.Errorf("history: unsupported delta kind %q", filter.Kind)
	}
	if filter.MaxDepth < -1 {
		return nil, errors.New("history: maximum depth must be -1 or greater")
	}
	if filter.Limit < 0 {
		return nil, errors.New("history: limit cannot be negative")
	}
	nonLeaves := make(map[string]bool)
	depths := make([]int, len(deltas))
	for i := range deltas {
		depth, err := relativeDepth(root, deltas[i].Path)
		if err != nil {
			return nil, err
		}
		depths[i] = depth
		if depth > 0 {
			for parent := filepath.Dir(filepath.Clean(deltas[i].Path)); ; parent = filepath.Dir(parent) {
				nonLeaves[parent] = true
				if parent == filepath.Clean(root) {
					break
				}
			}
		}
	}
	result := make([]Delta, 0, len(deltas))
	for i, delta := range deltas {
		if filter.MaxDepth >= 0 && depths[i] > filter.MaxDepth {
			continue
		}
		if filter.LeafOnly && nonLeaves[filepath.Clean(delta.Path)] {
			continue
		}
		if filter.Kind == DeltaKindFile && delta.IsDir || filter.Kind == DeltaKindDirectory && !delta.IsDir {
			continue
		}
		delta.Depth = depths[i]
		result = append(result, delta)
		if filter.Limit > 0 && len(result) == filter.Limit {
			break
		}
	}
	return result, nil
}

func relativeDepth(root, path string) (int, error) {
	root = filepath.Clean(root)
	relative, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return 0, fmt.Errorf("history: delta path %q escapes root %q", path, root)
	}
	if relative == "." {
		return 0, nil
	}
	return len(strings.Split(relative, string(filepath.Separator))), nil
}

func flattenPaths(snap *index.Snapshot) (map[string]measuredNode, error) {
	if snap.Root == "" || len(snap.Nodes) == 0 || snap.Nodes[0].Parent != -1 {
		return nil, errors.New("history: invalid snapshot layout")
	}
	paths := make([]string, len(snap.Nodes))
	paths[0] = filepath.Clean(snap.Root)
	result := make(map[string]measuredNode, len(snap.Nodes))
	result[paths[0]] = measuredNode{path: paths[0], node: snap.Nodes[0]}
	for i := 1; i < len(snap.Nodes); i++ {
		node := snap.Nodes[i]
		if node.Parent < 0 || node.Parent >= i || node.Depth != snap.Nodes[node.Parent].Depth+1 {
			return nil, fmt.Errorf("history: invalid parent for node %d", i)
		}
		paths[i] = filepath.Join(paths[node.Parent], node.Name)
		result[paths[i]] = measuredNode{path: paths[i], node: node}
	}
	return result, nil
}

func makeDelta(path string, before, after measuredNode, change Change) Delta {
	isDir := before.node.IsDir
	if change == ChangeNew {
		isDir = after.node.IsDir
	}
	return Delta{
		Path: path, IsDir: isDir, Change: change,
		BeforeApparent: before.node.Apparent, AfterApparent: after.node.Apparent,
		ApparentDelta: after.node.Apparent - before.node.Apparent,
		BeforeAlloc:   before.node.Alloc, AfterAlloc: after.node.Alloc,
		AllocatedDelta: after.node.Alloc - before.node.Alloc,
		BeforeModTime:  before.node.ModTime, AfterModTime: after.node.ModTime,
	}
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
