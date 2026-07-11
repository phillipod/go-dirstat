package tui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phillipod/go-dirstat/internal/diagnose"
	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/fsops"
	"github.com/phillipod/go-dirstat/internal/history"
	"github.com/phillipod/go-dirstat/internal/index"
	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

const (
	defaultQueueMaxOperations = 1000
	maxAnalyticalRows         = 10000
	maxSignedInt64            = int64(^uint64(0) >> 1)
)

type pressureLoadedMsg struct {
	volume   fsinfo.Volume
	loadedAt time.Time
	err      error
}

type growthViewState struct {
	loading          bool
	loadedAt         time.Time
	currentAt        time.Time
	baselineAt       time.Time
	currentComplete  bool
	baselineMissing  bool
	baselineRecorded bool
	deltas           []history.Delta
	truncated        bool
	err              error
}

type growthLoadedMsg struct {
	generation uint64
	state      growthViewState
}

type openDeletedViewState struct {
	loading  bool
	loadedAt time.Time
	result   diagnose.Result
	err      error
}

type openDeletedLoadedMsg struct {
	generation uint64
	state      openDeletedViewState
}

type destinationEntry struct {
	name string
	path string
}

type destinationPickerState struct {
	generation uint64
	loading    bool
	path       string
	entries    []destinationEntry
	cursor     int
	volume     fsinfo.Volume
	err        error
}

type destinationLoadedMsg struct {
	generation uint64
	state      destinationPickerState
}

type pressureForecast struct {
	queued              uint64
	availableAfter      uint64
	callerPressureAfter float64
	targetGap           uint64
	targetGapAfter      uint64
}

func (m *model) loadPressureCmd() tea.Cmd {
	root := m.rootAbs
	return func() tea.Msg {
		volume, err := fsinfo.VolumeFor(root)
		return pressureLoadedMsg{volume: volume, loadedAt: time.Now().UTC(), err: err}
	}
}

func (m *model) pressureForecast() pressureForecast {
	queued := nonnegativeUint64(m.queuedReclaimBytes())
	availableAfter := saturatingAdd(m.volume.Available, queued)
	if m.volume.CallerCapacity > 0 && availableAfter > m.volume.CallerCapacity {
		availableAfter = m.volume.CallerCapacity
	}
	physicalAfter := subtractFloorUint64(m.volume.PhysicalUsed, queued)
	pressureAfter := float64(0)
	if m.volume.CallerCapacity > 0 {
		pressureAfter = float64(physicalAfter) * 100 / float64(m.volume.CallerCapacity)
	}
	return pressureForecast{
		queued:              queued,
		availableAfter:      availableAfter,
		callerPressureAfter: pressureAfter,
		targetGap:           subtractFloorUint64(m.targetAvailable, m.volume.Available),
		targetGapAfter:      subtractFloorUint64(m.targetAvailable, availableAfter),
	}
}

func (m *model) beginTargetInput() {
	m.targeting = true
	m.targetInput = ""
}

func (m *model) applyTargetInput() error {
	input := strings.TrimSpace(m.targetInput)
	if input == "" {
		return errors.New("target available bytes are required")
	}
	target, err := parseTargetBytes(input)
	if err != nil {
		return fmt.Errorf("target available: %w", err)
	}
	m.targetAvailable = target
	m.targeting = false
	m.targetInput = ""
	return nil
}

func parseTargetBytes(input string) (uint64, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, errors.New("expected bytes or an IEC size such as 20G")
	}
	multiplier := uint64(1)
	last := input[len(input)-1]
	switch last {
	case 'b', 'B':
		input = input[:len(input)-1]
	case 'k', 'K':
		multiplier, input = 1<<10, input[:len(input)-1]
	case 'm', 'M':
		multiplier, input = 1<<20, input[:len(input)-1]
	case 'g', 'G':
		multiplier, input = 1<<30, input[:len(input)-1]
	case 't', 'T':
		multiplier, input = 1<<40, input[:len(input)-1]
	case 'p', 'P':
		multiplier, input = 1<<50, input[:len(input)-1]
	case 'e', 'E':
		multiplier, input = 1<<60, input[:len(input)-1]
	}
	if input == "" {
		return 0, errors.New("expected a non-negative integer before the size suffix")
	}
	value, err := strconv.ParseUint(input, 10, 64)
	if err != nil {
		return 0, errors.New("expected a non-negative integer with optional B/K/M/G/T/P/E suffix")
	}
	if value > ^uint64(0)/multiplier {
		return 0, errors.New("size exceeds the maximum supported value")
	}
	return value * multiplier, nil
}

func (m *model) queueOperationLimit() int {
	if m.app.opts.QueueMaxOperations > 0 {
		return m.app.opts.QueueMaxOperations
	}
	return defaultQueueMaxOperations
}

func (m *model) validateQueuePolicy(operations []fsops.Operation) error {
	if len(operations) > m.queueOperationLimit() {
		return fmt.Errorf("queue policy allows at most %d operations", m.queueOperationLimit())
	}
	if limit := m.app.opts.QueueMaxReclaimBytes; limit > 0 {
		reclaim := m.queueReclaimBytes(operations)
		if reclaim > limit {
			return fmt.Errorf("queue reclaim estimate %d bytes exceeds policy cap %d bytes", reclaim, limit)
		}
	}
	return nil
}

func (m *model) queueReclaimBytes(operations []fsops.Operation) int64 {
	deletes := make([]fsops.Operation, 0, len(operations))
	for _, operation := range operations {
		if operation.Action == fsops.ActionDelete {
			deletes = append(deletes, operation)
		}
	}
	sort.SliceStable(deletes, func(i, j int) bool {
		left, right := filepath.Clean(deletes[i].Source), filepath.Clean(deletes[j].Source)
		if len(left) != len(right) {
			return len(left) < len(right)
		}
		return left < right
	})
	var total int64
	seen := make([]string, 0, len(deletes))
	for _, operation := range deletes {
		path := filepath.Clean(operation.Source)
		nested := false
		for _, parent := range seen {
			relative, err := filepath.Rel(parent, path)
			if err == nil && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				nested = true
				break
			}
		}
		if nested {
			continue
		}
		seen = append(seen, path)
		amount := int64(0)
		if node := m.findNode(path); node != nil {
			amount = node.Alloc
		} else if operation.Expected != nil {
			amount = operation.Expected.Allocated
		}
		if amount <= 0 {
			continue
		}
		if total > maxSignedInt64-amount {
			total = maxSignedInt64
		} else {
			total += amount
		}
	}
	return total
}

func (m *model) cancelAnalysis() {
	if m.analysisCancel != nil {
		m.analysisCancel()
		m.analysisCancel = nil
	}
}

func (m *model) startGrowthAnalysis() tea.Cmd {
	m.cancelAnalysis()
	m.analysisGeneration++
	generation := m.analysisGeneration
	ctx, cancel := context.WithCancel(m.ctx)
	m.analysisCancel = cancel
	m.growth.loading, m.growth.err = true, nil
	root := m.root.Clone()
	stats := m.stats
	complete := m.completeTree
	currentAt := m.snapshotAt
	app := m.app
	return growthAnalysisCmd(ctx, generation, app, m.rootAbs, m.fingerprint, m.historyDir, m.historyErr, root, stats, complete, currentAt)
}

func growthAnalysisCmd(
	ctx context.Context,
	generation uint64,
	app *App,
	rootAbs string,
	fingerprint string,
	historyDir string,
	historyErr error,
	root *tree.Node,
	stats scan.Stats,
	complete bool,
	currentAt time.Time,
) tea.Cmd {
	return func() tea.Msg {
		state := growthViewState{loadedAt: time.Now().UTC(), currentAt: currentAt, currentComplete: complete}
		if err := ctx.Err(); err != nil {
			state.err = err
			return growthLoadedMsg{generation: generation, state: state}
		}
		if !complete || root == nil || currentAt.IsZero() {
			state.err = errors.New("growth requires a complete current scan")
			return growthLoadedMsg{generation: generation, state: state}
		}
		maxRecords := app.opts.HistoryMax
		if maxRecords <= 0 {
			maxRecords = history.MaxRecords
		}
		if historyErr != nil {
			state.err = historyErr
			return growthLoadedMsg{generation: generation, state: state}
		}
		store, err := history.OpenStoreAtWithPolicy(historyDir, maxRecords, tuiHistoryPolicy(app.opts))
		if err != nil {
			state.err = err
			return growthLoadedMsg{generation: generation, state: state}
		}
		previous, err := store.PreviousContext(ctx, rootAbs, fingerprint, time.Time{})
		if errors.Is(err, fs.ErrNotExist) {
			previous, err = nil, nil
		}
		if err != nil {
			state.err = err
			return growthLoadedMsg{generation: generation, state: state}
		}
		current := index.FromTree(root, fingerprint, stats.RootFS, stats.Files, stats.Dirs, stats.Errors, complete, currentAt)
		current.Root = rootAbs
		if !app.opts.ReadOnly {
			if _, err := store.RecordSnapshotContext(ctx, current); err != nil {
				state.err = err
				return growthLoadedMsg{generation: generation, state: state}
			}
		}
		if previous == nil {
			state.baselineMissing = true
			state.baselineRecorded = !app.opts.ReadOnly
			return growthLoadedMsg{generation: generation, state: state}
		}
		state.baselineAt = previous.ScannedAt
		state.deltas, err = history.Compare(previous, current)
		if err != nil {
			state.err = err
		} else if len(state.deltas) > maxAnalyticalRows {
			state.deltas = state.deltas[:maxAnalyticalRows]
			state.truncated = true
		}
		if cancelErr := ctx.Err(); cancelErr != nil {
			state.err = cancelErr
			state.deltas = nil
		}
		return growthLoadedMsg{generation: generation, state: state}
	}
}

func historyAwarePolicy(root string, policy scope.Policy, storeDir string) (scope.Policy, string, bool, error) {
	root = filepath.Clean(root)
	store, err := filepath.Abs(filepath.Clean(storeDir))
	if err != nil {
		return policy, "", false, err
	}
	rootResolved := resolvedOrClean(root)
	storeResolved := resolvedOrClean(store)
	visibleStoreUnderRoot, visibleRelative := filesystemPathContainedBy(root, store)
	resolvedStoreUnderRoot, resolvedRelative := filesystemPathContainedBy(rootResolved, storeResolved)
	visibleRootUnderStore, _ := filesystemPathContainedBy(store, root)
	resolvedRootUnderStore, _ := filesystemPathContainedBy(storeResolved, rootResolved)
	if (visibleStoreUnderRoot && visibleRelative == ".") ||
		(resolvedStoreUnderRoot && resolvedRelative == ".") ||
		(visibleRootUnderStore && !visibleStoreUnderRoot) ||
		(resolvedRootUnderStore && !resolvedStoreUnderRoot) {
		return policy, "", false, errors.New("history store must not be the scan root or contain the scan root")
	}
	contained := visibleStoreUnderRoot || resolvedStoreUnderRoot
	policy.ExcludePaths = append([]string(nil), policy.ExcludePaths...)
	if contained {
		policy.ExcludePaths = append(policy.ExcludePaths, store, storeResolved)
		if visibleStoreUnderRoot && visibleRelative != "." {
			policy.ExcludePaths = append(policy.ExcludePaths, filepath.Join(root, visibleRelative))
		}
		if resolvedStoreUnderRoot && resolvedRelative != "." {
			policy.ExcludePaths = append(policy.ExcludePaths, filepath.Join(root, resolvedRelative))
		}
	}
	return policy, index.Fingerprint(root, policy), contained, nil
}

func (m *model) startOpenDeletedAnalysis() tea.Cmd {
	m.cancelAnalysis()
	m.analysisGeneration++
	generation := m.analysisGeneration
	ctx, cancel := context.WithCancel(m.ctx)
	m.analysisCancel = cancel
	m.openDeleted.loading, m.openDeleted.err = true, nil
	root := m.rootAbs
	return func() tea.Msg {
		result := diagnose.Gather(ctx, []string{root})
		state := openDeletedViewState{loadedAt: time.Now().UTC(), result: result}
		if err := ctx.Err(); err != nil {
			state.err = err
		}
		return openDeletedLoadedMsg{generation: generation, state: state}
	}
}

func (m *model) startDestinationPicker() tea.Cmd {
	m.destination.generation++
	generation := m.destination.generation
	m.destination.loading = true
	m.destination.err = nil
	path := m.destination.path
	if path == "" {
		path = m.rootAbs
	}
	return destinationPickerCmd(m.ctx, generation, m.rootAbs, path)
}

func destinationPickerCmd(ctx context.Context, generation uint64, root, path string) tea.Cmd {
	return func() tea.Msg {
		state := destinationPickerState{generation: generation, path: path}
		if err := ctx.Err(); err != nil {
			state.err = err
			return destinationLoadedMsg{generation: generation, state: state}
		}
		safePath, err := confinedDestinationDirectory(root, path)
		if err != nil {
			state.err = err
			return destinationLoadedMsg{generation: generation, state: state}
		}
		state.path = safePath
		entries, err := os.ReadDir(safePath)
		if err != nil {
			state.err = err
			return destinationLoadedMsg{generation: generation, state: state}
		}
		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			candidate := filepath.Join(safePath, entry.Name())
			if _, err := confinedDestinationDirectory(root, candidate); err == nil {
				state.entries = append(state.entries, destinationEntry{name: entry.Name(), path: candidate})
			}
		}
		sort.Slice(state.entries, func(i, j int) bool {
			return strings.ToLower(state.entries[i].name) < strings.ToLower(state.entries[j].name)
		})
		state.volume, state.err = fsinfo.VolumeFor(safePath)
		return destinationLoadedMsg{generation: generation, state: state}
	}
}

func confinedDestinationDirectory(root, path string) (string, error) {
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	pathAbs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("resolve destination root: %w", err)
	}
	pathResolved, err := filepath.EvalSymlinks(pathAbs)
	if err != nil {
		return "", fmt.Errorf("resolve destination: %w", err)
	}
	if !filesystemPathContains(rootResolved, pathResolved) {
		return "", errors.New("destination escapes the scan root")
	}
	info, err := os.Stat(pathResolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("destination picker requires a directory")
	}
	return pathAbs, nil
}

func (m *model) destinationParentCmd() tea.Cmd {
	rootResolved := resolvedOrClean(m.rootAbs)
	currentResolved := resolvedOrClean(m.destination.path)
	if filesystemPathEqual(rootResolved, currentResolved) {
		return nil
	}
	parent := filepath.Dir(m.destination.path)
	if _, err := confinedDestinationDirectory(m.rootAbs, parent); err != nil {
		parent = m.rootAbs
	}
	m.destination.path = parent
	return m.startDestinationPicker()
}

func (m *model) openDestinationEntryCmd() tea.Cmd {
	if m.destination.cursor < 0 || m.destination.cursor >= len(m.destination.entries) {
		return nil
	}
	m.destination.path = m.destination.entries[m.destination.cursor].path
	m.destination.cursor = 0
	return m.startDestinationPicker()
}

func (m *model) selectedDestination() string {
	paths := m.actionPaths()
	if len(paths) == 1 {
		return filepath.Join(m.destination.path, filepath.Base(paths[0]))
	}
	return m.destination.path
}

func (m *model) handleDestinationPickerKey(k tea.KeyMsg) (bool, tea.Cmd) {
	switch k.String() {
	case keyDown:
		m.destination.cursor = min(m.destination.cursor+1, max(0, len(m.destination.entries)-1))
		return true, nil
	case "up":
		m.destination.cursor = max(0, m.destination.cursor-1)
		return true, nil
	case keyPageDown:
		m.destination.cursor = min(m.destination.cursor+max(1, m.availHeight()-7), max(0, len(m.destination.entries)-1))
		return true, nil
	case keyPageUp:
		m.destination.cursor = max(0, m.destination.cursor-max(1, m.availHeight()-7))
		return true, nil
	case "left", keyBackspace:
		return true, m.destinationParentCmd()
	case "right":
		return true, m.openDestinationEntryCmd()
	case keyEnter:
		if len(m.destination.entries) > 0 {
			return true, m.openDestinationEntryCmd()
		}
		return true, nil
	case "tab":
		m.managementError = ""
		return true, m.stageCmd(m.managementAction, m.actionPaths(), m.selectedDestination())
	}
	return false, nil
}

func (m *model) activateViewCmd() tea.Cmd {
	switch m.view {
	case viewTree, viewExt, viewLargest, viewHelp:
		return m.requestInspect()
	case viewGrowth:
		if m.growth.loadedAt.IsZero() && !m.growth.loading {
			return m.startGrowthAnalysis()
		}
	case viewOpenDeleted:
		if m.openDeleted.loadedAt.IsZero() && !m.openDeleted.loading {
			return m.startOpenDeletedAnalysis()
		}
	}
	return m.requestInspect()
}

func resolvedOrClean(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(resolved)
}

func nonnegativeUint64(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func saturatingAdd(left, right uint64) uint64 {
	if ^uint64(0)-left < right {
		return ^uint64(0)
	}
	return left + right
}

func subtractFloorUint64(left, right uint64) uint64 {
	if right >= left {
		return 0
	}
	return left - right
}
