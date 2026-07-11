package tui

import (
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/fsops"
	"github.com/phillipod/go-dirstat/internal/tree"
)

// applyCompletedOperations patches deterministic filesystem changes into the
// measured tree. It returns the completed operation IDs, whether the tree
// changed, and whether a scan is still needed to reconcile an ambiguous or
// non-delete operation.
func (m *model) applyCompletedOperations(results []fsops.Result) (map[string]bool, bool, bool) {
	operations := make(map[string]fsops.Operation, len(m.queue))
	for _, op := range m.queue {
		operations[op.ID] = op
	}

	completed := make(map[string]bool)
	changed, needsScan := false, !m.completeTree
	for _, result := range results {
		if result.Status != "ok" || result.DryRun {
			continue
		}
		completed[result.OperationID] = true
		op, ok := operations[result.OperationID]
		if !ok {
			needsScan = true
			continue
		}
		if op.Action != fsops.ActionDelete {
			needsScan = true
			continue
		}
		removed, exact := m.removeDeletedNode(op, result.FinishedAt)
		changed = changed || removed
		if !removed || !exact {
			needsScan = true
		}
	}
	if changed {
		// A pre-delete inspection may still be running. Make its eventual
		// message stale before rebuilding or requesting the new selection.
		m.inspectGeneration++
		m.rebuild()
	}
	return completed, changed, needsScan
}

// afterFilesystemMutation either starts a clean reconciliation scan or keeps
// the exact in-memory delta and persists it as the newest complete cache
// generation. Inspection is refreshed in both cases by the scan or directly.
func (m *model) afterFilesystemMutation(changed, needsScan bool) tea.Cmd {
	if needsScan {
		return m.startScan()
	}
	inspect := m.requestInspect()
	if !changed || m.store == nil || !m.completeTree {
		return inspect
	}

	m.scanGeneration++
	generation := m.scanGeneration
	m.cacheSaves.markSuccessful(generation)
	msg := scanDoneMsg{
		generation: generation,
		node:       m.root.Clone(),
		stats:      m.stats,
	}
	return tea.Batch(saveCmd(&m.cacheSaves, m.store, m.rootAbs, m.fingerprint, msg), inspect)
}

// auditMutationNeedsReconciliation reports whether fsops wrote its audit log
// inside the measured root. The normal CLI-provided audit path is outside the
// root; package callers that use fsops' in-root default need a scan so that the
// newly created or enlarged log is accounted for.
func (m *model) auditMutationNeedsReconciliation() bool {
	if m.app.opts.DisableAudit {
		return false
	}
	path := m.app.opts.AuditPath
	if path == "" {
		path = fsops.DefaultAuditPath(m.rootAbs)
	} else if !filepath.IsAbs(path) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return true
		}
		path = absolute
	}
	return filesystemPathContains(m.rootAbs, path)
}

// removeDeletedNode removes one successfully deleted path and updates every
// aggregate that the scanner would otherwise rebuild: sizes, counts, errors,
// modification times, derived selections, marks, and expansion state. The
// second return value is false when hardlink or followed-symlink ownership
// makes byte redistribution ambiguous and a reconciliation scan is required.
func (m *model) removeDeletedNode(op fsops.Operation, finished time.Time) (bool, bool) {
	node := m.findNode(op.Source)
	if node == nil || node.Parent() == nil {
		return false, false
	}

	parent := node.Parent()
	rel := node.Path()
	files, dirs, errs := deletedCounts(node)
	ambiguous := m.deleteNeedsReconciliation(node, op)

	m.rememberSelection()
	if pathContains(rel, m.selectedPath) {
		m.selectedPath = parent.Path()
	}
	if pathContains(rel, m.selectedFile) {
		m.selectedFile = ""
	}

	for i, child := range parent.Children {
		if child == node {
			parent.Children = append(parent.Children[:i], parent.Children[i+1:]...)
			break
		}
	}
	for ancestor := parent; ancestor != nil; ancestor = ancestor.Parent() {
		ancestor.Apparent = subtractFloor(ancestor.Apparent, node.Apparent)
		ancestor.Alloc = subtractFloor(ancestor.Alloc, node.Alloc)
		ancestor.FileCount = subtractFloorInt(ancestor.FileCount, files)
		ancestor.DirCount = subtractFloorInt(ancestor.DirCount, dirs)
		if !finished.IsZero() && finished.After(ancestor.ModTime) {
			ancestor.ModTime = finished
		}
	}
	m.stats.Files = subtractFloorInt(m.stats.Files, files)
	m.stats.Dirs = subtractFloorInt(m.stats.Dirs, dirs)
	m.stats.Errors -= int64(errs)
	if m.stats.Errors < 0 {
		m.stats.Errors = 0
	}

	for path := range m.marks {
		if filesystemPathContains(op.Source, path) {
			delete(m.marks, path)
		}
	}
	for path := range m.expanded {
		if pathContains(rel, path) {
			delete(m.expanded, path)
		}
	}
	if filesystemPathContains(op.Source, m.inspectPath) {
		m.inspectPath, m.inspectEntry, m.inspectPreview, m.inspectErr = "", fsinfo.Entry{}, nil, nil
	}
	return true, !ambiguous
}

func (m *model) findNode(path string) *tree.Node {
	want := filepath.Clean(path)
	var found *tree.Node
	m.root.Walk(func(node *tree.Node) bool {
		candidate := m.rootAbs
		if rel := node.Path(); rel != "" {
			candidate = filepath.Join(m.rootAbs, filepath.FromSlash(rel))
		}
		if filesystemPathEqual(candidate, want) {
			found = node
			return false
		}
		return found == nil
	})
	return found
}

func (m *model) deleteNeedsReconciliation(node *tree.Node, op fsops.Operation) bool {
	if m.app.policy.FollowSymlinks {
		return true
	}
	if !node.IsDir {
		if node.Hardlink {
			return false
		}
		return op.Expected == nil || op.Expected.Links > 1
	}
	hasHardlinks := false
	m.root.Walk(func(candidate *tree.Node) bool {
		if !candidate.IsDir && candidate.Hardlink {
			hasHardlinks = true
			return false
		}
		return !hasHardlinks
	})
	return hasHardlinks
}

func deletedCounts(node *tree.Node) (files, dirs, errs int) {
	if node.IsDir {
		files = node.FileCount
		dirs = node.DirCount + 1
	} else if node.Err == nil {
		files = 1
	}
	node.Walk(func(entry *tree.Node) bool {
		if entry.Err != nil {
			errs++
		}
		return true
	})
	return files, dirs, errs
}

func pathContains(parent, candidate string) bool {
	parent = strings.Trim(filepath.ToSlash(filepath.Clean(filepath.FromSlash(parent))), "/")
	candidate = strings.Trim(filepath.ToSlash(filepath.Clean(filepath.FromSlash(candidate))), "/")
	if parent == "" {
		return candidate == ""
	}
	return candidate == parent || strings.HasPrefix(candidate, parent+"/")
}

func filesystemPathContains(parent, candidate string) bool {
	if candidate == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(candidate))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func filesystemPathEqual(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func subtractFloor(value, amount int64) int64 {
	if amount >= value {
		return 0
	}
	return value - amount
}

func subtractFloorInt(value, amount int) int {
	if amount >= value {
		return 0
	}
	return value - amount
}
