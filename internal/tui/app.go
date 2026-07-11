// Package tui implements the full-screen interactive directory browser built
// on Bubble Tea. It owns only presentation and input; measurement comes from
// scan, derived views from agg, and formatting from format — so the TUI stays
// free of filesystem and arithmetic concerns.
package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

// Options configures TUI-specific behavior.
type Options struct {
	// UseCache enables the persistent scan cache (instant open + background
	// refresh).
	UseCache bool
	// ReadOnly disables every action that can modify filesystem contents.
	ReadOnly bool
	// Editor is an exact executable argv. The selected path is appended as the
	// final argument; no shell is involved.
	Editor []string
	// Pager is an exact executable argv. The selected path is appended.
	Pager []string
	// Shell is an exact executable argv. It runs in the selected directory and
	// never receives an interpolated path argument.
	Shell []string
	// AuditPath overrides fsops' default audit destination. DisableAudit is the
	// explicit opt-out for environments that do not permit audit persistence.
	AuditPath    string
	DisableAudit bool
}

// App is the configured, runnable TUI. Build one with New, then call Run.
type App struct {
	path     string
	policy   scope.Policy
	sizeMode tree.SizeMode
	jobs     int
	opts     Options
}

// New returns a configured App.
func New(path string, p scope.Policy, sm tree.SizeMode, jobs int, o Options) *App {
	return &App{path: path, policy: p, sizeMode: sm, jobs: jobs, opts: o}
}

// Run launches the full-screen program and blocks until it exits. The scan runs
// in the background and streams partial trees so the view populates
// progressively instead of showing a loading screen; cancelling ctx when Run
// returns stops the scan so no goroutine outlives the UI.
func (a *App) Run(parent context.Context) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	m := newModel(a)
	m.ctx = ctx
	// Mouse tracking is intentionally not enabled until the model implements
	// click and wheel events; enabling terminal mouse reporting without handlers
	// steals normal selection/scroll behavior from the user's terminal.
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	m.program = p
	defer m.cancelScan()
	_, err := p.Run()
	return err
}

// scanStreamCmd runs a streaming scan in a goroutine. As the tree is built,
// partial snapshots are sent to the program as progressMsg so the UI renders
// progressively; the final, complete tree arrives as scanDoneMsg.
func scanStreamCmd(app *App, prog *tea.Program, ctx context.Context, generation uint64) tea.Cmd {
	return func() tea.Msg {
		progress := scan.Progress{
			Period: 60 * time.Millisecond,
			OnTick: func(node *tree.Node, stats scan.Stats) {
				if prog != nil {
					prog.Send(progressMsg{generation: generation, node: node, stats: stats})
				}
			},
		}
		node, stats, err := scan.ScanStream(ctx, app.path,
			scan.Options{Policy: app.policy, Concurrency: app.jobs}, progress)
		return scanDoneMsg{generation: generation, node: node, stats: stats, err: err}
	}
}

// startScan cancels any scan still in flight and starts a new generation.
// Generation-tagged messages keep a canceled scan from overwriting newer data.
func (m *model) startScan() tea.Cmd {
	m.cancelScan()
	m.scanGeneration++
	ctx, cancel := context.WithCancel(m.ctx)
	m.scanCancel = cancel
	m.scanning = true
	m.retainDuringScan = m.gotData
	m.scanNote = ""
	m.scanErr = nil
	return scanStreamCmd(m.app, m.program, ctx, m.scanGeneration)
}

// stopScan cancels the active generation while retaining the latest cached or
// partial tree. Incrementing the generation makes any completion already in
// flight stale, so a context-canceled result cannot replace the retained view.
func (m *model) stopScan() {
	if !m.scanning {
		return
	}
	m.cancelScan()
	m.scanGeneration++
	m.scanning = false
	m.retainDuringScan = false
	m.cacheNote = ""
	m.scanNote = "scan stopped"
}

func (m *model) cancelScan() {
	if m.scanCancel == nil {
		return
	}
	m.scanCancel()
	m.scanCancel = nil
}
