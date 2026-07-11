package tui

import (
	"fmt"
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
		if result.DryRun || (result.Status != fsops.ResultStatusOK && !result.MutationCompleted) {
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

// resultNeedsReconciliation consumes the schema-v2 mutation outcome contract.
// A partial status is authoritative even if a malformed producer omitted the
// matching flag; MayHaveMutated is also fail-safe if a producer attached it to
// an error or unknown status. Ordinary errors remain conservative because a
// canceled or failed recursive operation may predate schema-v2 evidence.
func resultNeedsReconciliation(result fsops.Result) bool {
	if result.DryRun {
		return false
	}
	if result.AuditStatus == fsops.AuditStatusFailed {
		return true
	}
	switch result.Status {
	case fsops.ResultStatusOK:
		return result.MayHaveMutated
	case fsops.ResultStatusPartial, fsops.ResultStatusError:
		return true
	default:
		return true
	}
}

func resultIndicatesPartialMutation(result fsops.Result) bool {
	return !result.DryRun && (result.Status == fsops.ResultStatusPartial || result.MayHaveMutated)
}

// applyFailureMessage preserves the outer Apply error when present, while
// still failing visibly if a schema-v2 non-ok result arrives without one.
func applyFailureMessage(results []fsops.Result, applyErr error) string {
	if applyErr != nil {
		return applyErr.Error()
	}
	for _, result := range results {
		if result.DryRun || result.Status == fsops.ResultStatusOK {
			continue
		}
		if result.Error != "" {
			return result.Error
		}
		return fmt.Sprintf("operation %q returned status %q", result.OperationID, result.Status)
	}
	return ""
}

// clearCompletedOperationMarks removes selections consumed by successful
// operations while retaining marks that still back failed, partial, or
// unattempted queue entries.
func (m *model) clearCompletedOperationMarks(completed map[string]bool) {
	for _, operation := range m.queue {
		if !completed[operation.ID] {
			continue
		}
		for path := range m.marks {
			removeDescendants := operation.Action == fsops.ActionDelete ||
				operation.Action == fsops.ActionMove || operation.Action == fsops.ActionRename
			if filesystemPathEqual(operation.Source, path) || removeDescendants && filesystemPathContains(operation.Source, path) {
				delete(m.marks, path)
			}
		}
	}
}

// afterFilesystemMutation either starts a clean reconciliation scan or keeps
// the exact in-memory delta and persists it as the newest complete cache
// generation. Inspection is refreshed in both cases by the scan or directly.
func (m *model) afterFilesystemMutation(changed, needsScan bool) tea.Cmd {
	if needsScan {
		return tea.Batch(m.startScan(), m.loadPressureCmd())
	}
	inspect := m.requestInspect()
	if !changed || m.store == nil || !m.cacheWritable || !m.completeTree {
		return tea.Batch(inspect, m.loadPressureCmd())
	}

	m.scanGeneration++
	generation := m.scanGeneration
	m.cacheSaves.markSuccessful(generation)
	msg := scanDoneMsg{
		generation: generation,
		node:       m.root.Clone(),
		stats:      m.stats,
	}
	return tea.Batch(saveCmd(m.ctx, &m.cacheSaves, m.store, m.rootAbs, m.fingerprint, msg), inspect, m.loadPressureCmd())
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
	parentSelfAlloc := directorySelfAllocation(parent)
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
	if !m.refreshParentFilesystemMetadata(parent, parentSelfAlloc) {
		ambiguous = true
	}
	m.stats.Files = subtractFloorInt(m.stats.Files, files)
	m.stats.Dirs = subtractFloorInt(m.stats.Dirs, dirs)
	m.stats.Errors -= int64(errs)
	if m.stats.Errors < 0 {
		m.stats.Errors = 0
	}
	m.stats.Complete = m.completeTree && m.stats.Errors == 0
	if !ambiguous {
		if finished.IsZero() {
			finished = time.Now().UTC()
		}
		m.snapshotAt = finished.UTC()
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

func directorySelfAllocation(node *tree.Node) int64 {
	self := node.Alloc
	for _, child := range node.Children {
		self = subtractFloor(self, child.Alloc)
	}
	return self
}

// refreshParentFilesystemMetadata closes the exact-delete accounting gap for
// the directory entry itself. Removing a child can change the direct parent's
// allocated directory blocks and mtime even when every removed descendant was
// measured exactly. Failure falls back to the normal reconciliation scan.
func (m *model) refreshParentFilesystemMetadata(parent *tree.Node, previousSelfAlloc int64) bool {
	parentPath := m.rootAbs
	if relative := parent.Path(); relative != "" {
		parentPath = filepath.Join(m.rootAbs, filepath.FromSlash(relative))
	}
	entry, err := fsinfo.Inspect(parentPath, true)
	if err != nil || entry.Kind != "directory" {
		return false
	}
	delta := entry.Allocated - previousSelfAlloc
	for ancestor := parent; ancestor != nil; ancestor = ancestor.Parent() {
		if delta < 0 {
			ancestor.Alloc = subtractFloor(ancestor.Alloc, -delta)
			continue
		}
		const maxInt64 = int64(^uint64(0) >> 1)
		if delta > maxInt64-ancestor.Alloc {
			return false
		}
		ancestor.Alloc += delta
	}
	parent.ModTime = entry.ModTime
	for _, child := range parent.Children {
		if child.ModTime.After(parent.ModTime) {
			parent.ModTime = child.ModTime
		}
	}
	for ancestor := parent.Parent(); ancestor != nil; ancestor = ancestor.Parent() {
		if parent.ModTime.After(ancestor.ModTime) {
			ancestor.ModTime = parent.ModTime
		}
	}
	return true
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

func (m *model) reconcileMarks() int {
	removed := 0
	for path := range m.marks {
		if m.findNode(path) == nil {
			delete(m.marks, path)
			removed++
		}
	}
	return removed
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

func filesystemPathContainedBy(parent, candidate string) (bool, string) {
	if candidate == "" {
		return false, ""
	}
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(candidate))
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, ""
	}
	return true, rel
}

func filesystemPathEqual(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == windowsOS {
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
