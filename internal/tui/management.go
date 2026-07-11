package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/fsops"
)

type managementMode int

const (
	managementNone managementMode = iota
	managementDestination
	managementMkdir
	managementReview
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

func (m *model) startInput(action fsops.Action) {
	if m.app.opts.ReadOnly {
		m.managementError = "read-only mode: filesystem actions are disabled"
		return
	}
	if len(m.actionPaths()) == 0 {
		m.managementError = "no path selected"
		return
	}
	m.managementAction, m.managementInput, m.managementError = action, "", ""
	if action == fsops.ActionMkdir {
		m.management = managementMkdir
	} else {
		m.management = managementDestination
	}
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
			if action == fsops.ActionDelete && entry.Kind == "directory" {
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

func (m *model) applyCmd() tea.Cmd {
	ctx, cancel := context.WithCancel(m.ctx)
	m.applyCancel = cancel
	plan := fsops.Plan{Header: fsops.PlanHeader{Version: fsops.PlanVersion, Root: m.rootAbs, CreatedAt: time.Now().UTC()}, Operations: append([]fsops.Operation(nil), m.queue...)}
	return func() tea.Msg {
		results, err := fsops.Apply(ctx, plan, fsops.ApplyOptions{AuditPath: m.app.opts.AuditPath, DisableAudit: m.app.opts.DisableAudit})
		return appliedMsg{results: results, err: err}
	}
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
	m.management, m.managementInput, m.managementError = managementNone, "", ""
}
