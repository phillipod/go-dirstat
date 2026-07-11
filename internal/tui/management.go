package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/fsops"
)

type managementMode int

type applyPlanFunc func(context.Context, fsops.Plan, fsops.ApplyOptions) ([]fsops.Result, error)

const (
	managementNone managementMode = iota
	managementDestination
	managementMkdir
	managementReview
	managementExport
	managementDryRun
	managementConfirm
	managementApplying
	managementResult
)

func (m *model) actionPaths() []string {
	if len(m.marks) == 0 {
		if path := m.selectedAbsolutePath(); path != "" {
			return []string{path}
		}
		return nil
	}
	paths := make([]string, 0, len(m.marks))
	for path := range m.marks {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func (m *model) startInput(action fsops.Action) tea.Cmd {
	if m.app.opts.ReadOnly {
		m.managementError = "read-only mode: filesystem actions are disabled"
		return nil
	}
	if len(m.actionPaths()) == 0 {
		m.managementError = "no path selected"
		return nil
	}
	m.managementAction, m.managementInput, m.managementError = action, "", ""
	if action == fsops.ActionMkdir {
		m.management = managementMkdir
		return nil
	}
	m.management = managementDestination
	m.destination = destinationPickerState{path: m.rootAbs}
	return m.startDestinationPicker()
}

func (m *model) stageDelete() tea.Cmd {
	if m.app.opts.ReadOnly {
		m.managementError = "read-only mode: filesystem actions are disabled"
		return nil
	}
	paths := m.actionPaths()
	if len(paths) == 0 {
		m.managementError = "no path selected"
		return nil
	}
	for _, path := range paths {
		if filepath.Clean(path) == filepath.Clean(m.rootAbs) {
			m.managementError = "the scan root cannot be staged for deletion"
			return nil
		}
	}
	return m.stageCmd(fsops.ActionDelete, paths, "")
}

func (m *model) stageCmd(action fsops.Action, paths []string, destination string) tea.Cmd {
	root := m.rootAbs
	startID := m.nextOperation
	m.nextOperation += uint64(max(1, len(paths)))
	return func() tea.Msg {
		ops := make([]fsops.Operation, 0, max(1, len(paths)))
		if action == fsops.ActionMkdir {
			target := destination
			if !filepath.IsAbs(target) {
				target = filepath.Join(root, target)
			}
			ops = append(ops, fsops.Operation{ID: fmt.Sprintf("tui-%d", startID+1), Action: action, Source: filepath.Clean(target)})
			return stagedMsg{operations: ops}
		}
		multiple := len(paths) > 1
		for i, path := range paths {
			entry, err := fsinfo.Inspect(path, false)
			if err != nil {
				return stagedMsg{err: fmt.Errorf("inspect %q: %w", path, err)}
			}
			op := fsops.Operation{ID: fmt.Sprintf("tui-%d", startID+uint64(i)+1), Action: action, Source: path, Expected: &entry}
			if action == fsops.ActionDelete && entry.Kind == kindDirectory {
				op.Recursive = true
			}
			if action == fsops.ActionCopy || action == fsops.ActionMove {
				dst := destination
				if !filepath.IsAbs(dst) {
					dst = filepath.Join(root, dst)
				}
				if multiple {
					dst = filepath.Join(dst, filepath.Base(path))
				}
				op.Destination = filepath.Clean(dst)
			}
			ops = append(ops, op)
		}
		return stagedMsg{operations: ops}
	}
}

func normalizeAndValidateQueue(root string, operations []fsops.Operation) ([]fsops.Operation, error) {
	normalized := make([]fsops.Operation, 0, len(operations))
	seenOperation := make(map[string]bool, len(operations))
	seenID := make(map[string]bool, len(operations))
	for _, op := range operations {
		if strings.TrimSpace(op.ID) == "" {
			return nil, errors.New("queued operation ID is required")
		}
		if seenID[op.ID] {
			return nil, fmt.Errorf("duplicate queued operation ID %q", op.ID)
		}
		seenID[op.ID] = true
		if strings.TrimSpace(op.Source) == "" || !filepath.IsAbs(op.Source) {
			return nil, fmt.Errorf("operation %q has a non-absolute source", op.ID)
		}
		op.Source = filepath.Clean(op.Source)
		if !filesystemPathContains(root, op.Source) {
			return nil, fmt.Errorf("operation %q source escapes the scan root", op.ID)
		}
		if filesystemPathEqual(root, op.Source) &&
			(op.Action == fsops.ActionDelete || op.Action == fsops.ActionMove || op.Action == fsops.ActionRename) {
			return nil, fmt.Errorf("operation %q cannot mutate the scan root itself", op.ID)
		}
		if op.Destination != "" {
			if !filepath.IsAbs(op.Destination) {
				return nil, fmt.Errorf("operation %q has a non-absolute destination", op.ID)
			}
			op.Destination = filepath.Clean(op.Destination)
			if !filesystemPathContains(root, op.Destination) {
				return nil, fmt.Errorf("operation %q destination escapes the scan root", op.ID)
			}
		}
		key := string(op.Action) + "\x00" + queuePathKey(op.Source) + "\x00" + queuePathKey(op.Destination)
		if seenOperation[key] {
			continue
		}
		seenOperation[key] = true
		normalized = append(normalized, op)
	}

	// A recursive parent deletion subsumes queued descendant deletions. Collapse
	// them independent of staging order so reclaim totals and apply results are
	// deterministic.
	keep := make([]bool, len(normalized))
	for i := range keep {
		keep[i] = true
	}
	for i, parent := range normalized {
		if parent.Action != fsops.ActionDelete || !parent.Recursive {
			continue
		}
		for j, child := range normalized {
			if i != j && child.Action == fsops.ActionDelete && queuePathContains(parent.Source, child.Source) {
				keep[j] = false
			}
		}
	}
	compacted := normalized[:0]
	for i, op := range normalized {
		if keep[i] {
			compacted = append(compacted, op)
		}
	}
	normalized = compacted

	destinations := make(map[string]string, len(normalized))
	for i, left := range normalized {
		if target := queuedOperationTarget(left); target != "" {
			if left.Destination != "" && queuePathKey(target) == queuePathKey(left.Source) {
				return nil, fmt.Errorf("operation %q has the same source and destination %q", left.ID, target)
			}
			if left.Destination != "" && queuePathContains(left.Source, target) {
				return nil, fmt.Errorf("operation %q targets a descendant of its source", left.ID)
			}
			key := queuePathKey(target)
			if prior, exists := destinations[key]; exists {
				return nil, fmt.Errorf("queued operations %q and %q target the same destination %q", prior, left.ID, target)
			}
			destinations[key] = left.ID
		}
		for j := i + 1; j < len(normalized); j++ {
			right := normalized[j]
			if queuePathKey(left.Source) == queuePathKey(right.Source) &&
				(left.Action != fsops.ActionCopy || right.Action != fsops.ActionCopy) {
				return nil, fmt.Errorf("queued operations %q and %q conflict on source %q", left.ID, right.ID, left.Source)
			}
			leftContainsRight := queuePathContains(left.Source, right.Source)
			rightContainsLeft := queuePathContains(right.Source, left.Source)
			if (leftContainsRight || rightContainsLeft) &&
				(left.Action == fsops.ActionDelete || left.Action == fsops.ActionMove || left.Action == fsops.ActionRename ||
					right.Action == fsops.ActionDelete || right.Action == fsops.ActionMove || right.Action == fsops.ActionRename) {
				return nil, fmt.Errorf("queued operations %q and %q have conflicting ancestor sources", left.ID, right.ID)
			}
		}
	}
	for _, op := range normalized {
		target := queuedOperationTarget(op)
		if target == "" {
			continue
		}
		for _, other := range normalized {
			if other.Action == fsops.ActionDelete && queuePathContains(other.Source, target) {
				return nil, fmt.Errorf("operation %q targets %q inside queued deletion %q", op.ID, target, other.Source)
			}
			if other.ID != op.ID && queuePathKey(target) == queuePathKey(other.Source) {
				return nil, fmt.Errorf("operation %q targets source %q used by operation %q", op.ID, target, other.ID)
			}
		}
	}
	return normalized, nil
}

func queuedOperationTarget(op fsops.Operation) string {
	if op.Destination != "" {
		return op.Destination
	}
	if op.Action == fsops.ActionMkdir || op.Action == fsops.ActionTouch {
		return op.Source
	}
	return ""
}

func queuePathKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == windowsOS {
		return strings.ToLower(path)
	}
	return path
}

func queuePathContains(parent, child string) bool {
	if queuePathKey(parent) == queuePathKey(child) {
		return false
	}
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (m *model) applyCmd() tea.Cmd {
	return m.runPlanCmd(false)
}

func (m *model) dryRunCmd() tea.Cmd {
	return m.runPlanCmd(true)
}

func (m *model) runPlanCmd(dryRun bool) tea.Cmd {
	ctx, cancel := context.WithCancel(m.ctx)
	m.applyCancel = cancel
	plan := m.operationPlan()
	apply := fsops.Apply
	if m.app.applyPlan != nil {
		apply = m.app.applyPlan
	}
	return func() tea.Msg {
		results, err := apply(ctx, plan, fsops.ApplyOptions{
			DryRun: dryRun, AuditPath: m.app.opts.AuditPath,
			DisableAudit: dryRun || m.app.opts.DisableAudit,
		})
		if dryRun {
			return dryRunMsg{results: results, err: err}
		}
		return appliedMsg{results: results, err: err}
	}
}

func (m *model) operationPlan() fsops.Plan {
	return fsops.Plan{
		Header:     fsops.PlanHeader{Version: fsops.PlanVersion, Root: m.rootAbs, CreatedAt: time.Now().UTC()},
		Operations: append([]fsops.Operation(nil), m.queue...),
	}
}

func (m *model) exportPlanCmd(path string) tea.Cmd {
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.rootAbs, path)
	}
	path = filepath.Clean(path)
	plan := m.operationPlan()
	return func() tea.Msg {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return exportedPlanMsg{path: path, err: err}
		}
		keep := false
		defer func() {
			_ = file.Close()
			if !keep {
				_ = os.Remove(path)
			}
		}()
		if err := fsops.WritePlan(file, plan); err != nil {
			return exportedPlanMsg{path: path, err: err}
		}
		if err := file.Sync(); err != nil {
			return exportedPlanMsg{path: path, err: err}
		}
		if err := file.Close(); err != nil {
			return exportedPlanMsg{path: path, err: err}
		}
		keep = true
		return exportedPlanMsg{path: path}
	}
}

func (m *model) resetQueueReview() {
	m.managementCursor = 0
	m.managementOffset = 0
	m.managementSeen = min(len(m.queue), m.reviewPageSize())
	m.managementDryRun = false
	m.managementNote = ""
}

func (m *model) reviewPageSize() int {
	return max(1, m.availHeight()-6)
}

func (m *model) clampQueueReview() {
	if len(m.queue) == 0 {
		m.managementCursor, m.managementOffset, m.managementSeen = 0, 0, 0
		return
	}
	m.managementCursor = min(max(0, m.managementCursor), len(m.queue)-1)
	page := m.reviewPageSize()
	maxOffset := max(0, len(m.queue)-page)
	if m.managementCursor < m.managementOffset {
		m.managementOffset = m.managementCursor
	}
	if m.managementCursor >= m.managementOffset+page {
		m.managementOffset = m.managementCursor - page + 1
	}
	m.managementOffset = min(max(0, m.managementOffset), maxOffset)
	visibleEnd := min(len(m.queue), m.managementOffset+page)
	if m.managementOffset <= m.managementSeen {
		m.managementSeen = max(m.managementSeen, visibleEnd)
	}
}

func (m *model) moveQueueCursor(delta int) {
	m.managementCursor += delta
	m.clampQueueReview()
}

func (m *model) removeQueuedOperation() {
	if len(m.queue) == 0 {
		return
	}
	i := m.managementCursor
	m.queue = append(m.queue[:i], m.queue[i+1:]...)
	m.resetQueueReview()
	m.managementNote = "Removed queued operation; review and dry-run state reset."
}

func (m *model) reorderQueuedOperation(delta int) {
	to := m.managementCursor + delta
	if to < 0 || to >= len(m.queue) {
		return
	}
	m.queue[m.managementCursor], m.queue[to] = m.queue[to], m.queue[m.managementCursor]
	m.resetQueueReview()
	m.managementNote = "Queue order changed; review and dry-run state reset."
}

func (m *model) queuedReclaimBytes() int64 {
	return m.queueReclaimBytes(m.queue)
}

func (m *model) externalEditorCmd() tea.Cmd {
	if m.app.opts.ReadOnly {
		m.managementError = "read-only mode: external editor is disabled"
		return nil
	}
	path := m.selectedAbsolutePath()
	cmd, err := pathCommand(m.app.opts.Editor, path)
	if err != nil {
		m.managementError = err.Error()
		return nil
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return externalDoneMsg{kind: "editor", err: err} })
}

func (m *model) pagerCmd() tea.Cmd {
	cmd, err := pathCommand(m.app.opts.Pager, m.selectedAbsolutePath())
	if err != nil {
		m.managementError = "pager: " + err.Error()
		return nil
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return externalDoneMsg{kind: "pager", err: err} })
}

func (m *model) shellCmd() tea.Cmd {
	if m.app.opts.ReadOnly {
		m.managementError = "read-only mode: shell is disabled"
		return nil
	}
	dir := m.selectedWorkingDirectory()
	cmd, err := workingDirectoryCommand(m.app.opts.Shell, dir)
	if err != nil {
		m.managementError = "shell: " + err.Error()
		return nil
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return externalDoneMsg{kind: "shell", err: err} })
}

func (m *model) selectedWorkingDirectory() string {
	path := m.selectedAbsolutePath()
	if m.dataView() == viewTree {
		if row := m.currentRow(); row != nil && row.node.IsDir {
			return path
		}
	}
	if path != "" {
		return filepath.Dir(path)
	}
	return ""
}

func validateExecutable(argv []string) error {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return errors.New("no command configured")
	}
	if strings.EqualFold(filepath.Base(argv[0]), "sudo") {
		return errors.New("sudo is not permitted from the TUI")
	}
	return nil
}

func pathCommand(argv []string, path string) (*exec.Cmd, error) {
	if err := validateExecutable(argv); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("no path selected")
	}
	args := append(append([]string(nil), argv[1:]...), path)
	return exec.Command(argv[0], args...), nil
}

func workingDirectoryCommand(argv []string, dir string) (*exec.Cmd, error) {
	if err := validateExecutable(argv); err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, errors.New("no directory selected")
	}
	cmd := exec.Command(argv[0], append([]string(nil), argv[1:]...)...)
	cmd.Dir = dir
	return cmd, nil
}

func (m *model) closeManagement() {
	if m.applyCancel != nil {
		m.applyCancel()
		m.applyCancel = nil
	}
	m.management, m.managementInput, m.managementError, m.managementNote = managementNone, "", "", ""
}
