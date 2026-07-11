package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/fsops"
	"github.com/phillipod/go-dirstat/internal/history"
	"github.com/phillipod/go-dirstat/internal/index"
	"github.com/phillipod/go-dirstat/internal/preview"
	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/tree"
)

// viewMode selects which screen the TUI shows.
type viewMode int

const (
	viewTree viewMode = iota
	viewExt
	viewLargest
	viewGrowth
	viewOpenDeleted
	viewHelp
)

// extSortMode names the order used by the extension view. It is deliberately
// separate from tree.SortMode: extension aggregates have a size, count, and
// name, but no meaningful modification time.
type extSortMode int

const (
	extSortSize extSortMode = iota
	extSortCount
	extSortName
)

func (m extSortMode) String() string {
	switch m {
	case extSortSize:
		return "size"
	case extSortCount:
		return "count"
	case extSortName:
		return "name"
	default:
		return unknownLabel
	}
}

// model is the Bubble Tea model. It holds the scanned tree plus purely
// presentational state (cursor, expansion, current view/size/sort).
type model struct {
	app *App

	program *tea.Program // set by Run; lets the scan stream progress messages in
	ctx     context.Context
	// Every rescan gets a fresh child context and monotonically increasing
	// generation so canceled scans cannot publish stale snapshots.
	scanCancel     context.CancelFunc
	scanGeneration uint64

	scanning         bool   // a background scan is in flight
	gotData          bool   // a real (non-placeholder) tree has arrived
	completeTree     bool   // root is a complete scan/cache tree, not a progress snapshot
	retainDuringScan bool   // keep prior results visible until a refresh completes
	cacheNote        string // e.g. "cached 3m ago, refreshing…" when shown from cache
	scanNote         string // e.g. "scan stopped" after retaining partial results
	cacheErr         error  // cache failures are visible without failing a good scan
	scanErr          error
	snapshotAt       time.Time

	volume          fsinfo.Volume
	volumeLoadedAt  time.Time
	volumeErr       error
	targetAvailable uint64
	targeting       bool
	targetInput     string

	analysisCancel     context.CancelFunc
	analysisGeneration uint64
	growth             growthViewState
	openDeleted        openDeletedViewState

	root  *tree.Node
	stats scan.Stats

	view         viewMode
	returnView   viewMode // view restored when help closes
	sort         tree.SortMode
	extSort      extSortMode
	sizeMode     tree.SizeMode
	selectedPath string          // stable tree selection across sorting/live snapshots
	selectedExt  string          // stable extension selection across sorting/mode changes
	selectedFile string          // stable largest-file selection across size-mode changes
	marks        map[string]bool // absolute paths selected for a batch action
	filter       string
	filterInput  string
	filtering    bool
	contextPanel bool

	management       managementMode
	managementInput  string
	managementError  string
	managementNote   string
	managementAction fsops.Action
	managementCursor int
	managementOffset int
	managementSeen   int
	managementDryRun bool
	destination      destinationPickerState
	queue            []fsops.Operation
	applyResults     []fsops.Result
	applyCancel      context.CancelFunc
	applyNeedsScan   bool // applying interrupted a scan that must be restarted
	nextOperation    uint64

	inspectGeneration uint64
	inspectPath       string
	inspectEntry      fsinfo.Entry
	inspectPreview    *preview.Result
	inspectErr        error

	expanded map[string]bool // keyed by node.Path(); root is ""

	// cache wiring
	store         *index.Store // nil when caching is disabled or unavailable
	cacheWritable bool
	rootAbs       string
	fingerprint   string
	historyDir    string
	historyErr    error
	cacheSaves    cacheSaveCoordinator

	rows   []row // flattened, visible rows for the tree view
	cursor int   // index into rows (tree) or extRows (ext view)
	offset int   // first visible row index

	extRows []extRow
	topRows []topRow

	width, height int
}

type row struct {
	node  *tree.Node
	depth int
}

// newModel constructs the initial model and starts in the tree view. The root is
// seeded as an empty placeholder so View can render the chrome immediately and
// fill in top-down as the streaming scan delivers progressMsg snapshots — there
// is never a "scanning…" loading screen.
func newModel(app *App) *model {
	abs, err := filepath.Abs(app.path)
	if err != nil {
		abs = app.path
	}
	historyDir, historyErr := history.DefaultStoreDir()
	if historyErr == nil && !app.opts.ReadOnly {
		maxRecords := app.opts.HistoryMax
		if maxRecords <= 0 {
			maxRecords = history.MaxRecords
		}
		_, historyErr = history.NewStoreAtWithPolicy(historyDir, maxRecords, tuiHistoryPolicy(app.opts))
	}
	if historyErr == nil {
		app.policy, _, _, historyErr = historyAwarePolicy(abs, app.policy, historyDir)
	}
	m := &model{
		app:             app,
		ctx:             context.Background(),
		scanning:        true,
		view:            viewTree,
		returnView:      viewTree,
		sort:            tree.SortSizeDesc,
		extSort:         extSortSize,
		sizeMode:        app.sizeMode,
		expanded:        map[string]bool{"": true}, // root expanded by default
		marks:           make(map[string]bool),
		contextPanel:    true,
		root:            &tree.Node{Name: filepath.Base(abs), IsDir: true, Depth: 0},
		rootAbs:         abs,
		fingerprint:     index.Fingerprint(abs, app.policy),
		historyDir:      historyDir,
		historyErr:      historyErr,
		targetAvailable: app.opts.TargetAvailableBytes,
	}
	if app.opts.UseCache {
		cacheDir, cacheDirErr := index.DefaultStoreDir()
		var st *index.Store
		if cacheDirErr == nil {
			if app.opts.ReadOnly {
				st, err = index.OpenStoreAtWithPolicy(cacheDir, tuiCachePolicy(app.opts))
			} else {
				st, err = index.NewStoreAtWithPolicy(cacheDir, tuiCachePolicy(app.opts))
			}
		} else {
			err = cacheDirErr
		}
		if err == nil {
			m.store = st
			m.cacheWritable = !app.opts.ReadOnly
		} else {
			m.cacheErr = fmt.Errorf("cache unavailable: %w", err)
		}
	}
	return m
}

func tuiCachePolicy(opts Options) index.Policy {
	policy := index.DefaultPolicy()
	if opts.CacheMaxBytes > 0 {
		policy.MaxBytes = opts.CacheMaxBytes
	}
	if opts.CacheMaxAge > 0 {
		policy.MaxAge = opts.CacheMaxAge
	}
	return policy
}

func tuiHistoryPolicy(opts Options) history.Policy {
	policy := history.DefaultPolicy()
	if opts.HistoryMaxBytes > 0 {
		policy.MaxBytes = opts.HistoryMaxBytes
	}
	if opts.HistoryMaxAge > 0 {
		policy.MaxAge = opts.HistoryMaxAge
	}
	return policy
}

// Init loads a cached snapshot instantly (if one exists and caching is on), then
// kicks off a background refresh scan that streams updates regardless.
func (m *model) Init() tea.Cmd {
	if m.store != nil {
		if snap, err := m.store.LoadContext(m.ctx, m.rootAbs, m.fingerprint); err == nil {
			if node := snap.ToTree(); node != nil {
				m.root = node
				m.stats = scan.Stats{Files: snap.Files, Dirs: snap.Dirs, Errors: snap.Errors, RootFS: snap.RootFS, Complete: snap.Complete}
				m.gotData = true
				m.completeTree = snap.Complete
				m.cacheNote = "cached " + format.Age(index.Age(snap)) + ", refreshing…"
				m.snapshotAt = snap.ScannedAt
				m.rebuild()
				return tea.Batch(m.startScan(), m.loadPressureCmd())
			}
		} else if !index.IsMissing(err) {
			m.cacheErr = fmt.Errorf("cache load failed: %w", err)
		}
	}
	return tea.Batch(m.startScan(), m.loadPressureCmd())
}

// progressMsg carries a partial snapshot of the in-progress tree, delivered in
// scan order (oldest first) while a ScanStream is running.
type progressMsg struct {
	generation uint64
	node       *tree.Node
	stats      scan.Stats
}

// scanDoneMsg is delivered when a scan (initial or background refresh) finishes.
type scanDoneMsg struct {
	generation uint64
	node       *tree.Node
	stats      scan.Stats
	err        error
}

type inspectMsg struct {
	generation uint64
	path       string
	entry      fsinfo.Entry
	preview    *preview.Result
	err        error
}

type stagedMsg struct {
	operations []fsops.Operation
	err        error
}

type appliedMsg struct {
	results []fsops.Result
	err     error
}

type dryRunMsg struct {
	results []fsops.Result
	err     error
}

type exportedPlanMsg struct {
	path string
	err  error
}

type externalDoneMsg struct {
	kind string
	err  error
}

func (m *model) selectedAbsolutePath() string {
	var rel string
	switch m.dataView() {
	case viewLargest:
		if m.cursor >= 0 && m.cursor < len(m.topRows) {
			rel = m.topRows[m.cursor].file.Rel
		}
	case viewTree:
		if r := m.currentRow(); r != nil {
			rel = r.node.Path()
		}
	case viewExt, viewGrowth, viewOpenDeleted, viewHelp:
		return ""
	}
	if rel == "" || rel == "." {
		return m.rootAbs
	}
	return filepath.Join(m.rootAbs, filepath.FromSlash(rel))
}

func (m *model) requestInspect() tea.Cmd {
	path := m.selectedAbsolutePath()
	if path == "" {
		m.inspectPath, m.inspectEntry, m.inspectPreview, m.inspectErr = "", fsinfo.Entry{}, nil, nil
		return nil
	}
	m.inspectGeneration++
	generation := m.inspectGeneration
	return func() tea.Msg {
		entry, err := fsinfo.Inspect(path, false)
		msg := inspectMsg{generation: generation, path: path, entry: entry, err: err}
		if err == nil && entry.Kind == "file" {
			if got, readErr := preview.Read(path, preview.Options{Limit: 8 * 1024}); readErr == nil {
				msg.preview = &got
			}
		}
		return msg
	}
}
