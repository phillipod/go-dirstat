// Package scan walks a filesystem path and builds a tree.Node tree of measured
// sizes. It is concurrent: each directory is processed by its own goroutine
// (so wide, deep trees parallelise across directories), with the number of
// active directory traversals and entry stat workers both bounded by
// Concurrency (GOMAXPROCS by default).
// Directory streams are consumed in bounded batches, so a very wide directory
// retains its authoritative nodes without also retaining full-width entry,
// stat-result, classification-plan, and recursion slices.
//
// The scan builds a single live tree whose aggregates are updated incrementally
// as each entry is measured, so ScanStream can hand a UI consistent, partial
// snapshots while the walk is still in flight. Scan is simply ScanStream with no
// progress callback.
//
// All filtering decisions are delegated to scope.Policy; this package only
// performs traversal and measurement. Errors on individual entries are
// recorded on the node and never abort the scan, matching `du` semantics.
package scan

import (
	"container/heap"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

// Options configures a scan.
type Options struct {
	Policy scope.Policy
	// Concurrency bounds active directory traversal and stat work. Defaults to
	// GOMAXPROCS.
	Concurrency int
	// directoryBatchSize is an internal test override. Production scans use the
	// bounded defaultDirectoryBatchSize.
	directoryBatchSize int
}

// DefaultOptions returns the safe defaults: scope.New() policy and GOMAXPROCS
// workers.
func DefaultOptions() Options {
	return Options{Policy: scope.New(), Concurrency: runtime.GOMAXPROCS(0)}
}

// WithPolicy returns Options derived from defaults with the given policy.
func WithPolicy(p scope.Policy) Options {
	return Options{Policy: p, Concurrency: runtime.GOMAXPROCS(0)}
}

// Stats summarises a completed scan.
type Stats struct {
	Files    int           // regular files measured
	Dirs     int           // directories measured (including root if dir)
	Errors   int64         // non-fatal per-entry errors
	Elapsed  time.Duration // wall-clock scan time
	RootFS   string        // filesystem type of the root, if known
	Complete bool          // true only when the final scan had no entry errors
}

// Scan walks root and returns the measured tree. It is ScanStream with no
// progress callback. A fatal error (e.g. root does not exist) is returned as
// error; per-entry problems land in Stats.Errors and on the offending node's Err.
func Scan(ctx context.Context, root string, opts Options) (*tree.Node, Stats, error) {
	return ScanStream(ctx, root, opts, Progress{})
}

// Progress configures optional incremental reporting during ScanStream.
type Progress struct {
	// OnTick is invoked with a snapshot of the in-progress tree and the running
	// counters, throttled to Period. Snapshots are delivered in scan order from a
	// single dedicated goroutine, so a caller may render each one directly. Nil
	// disables streaming (ScanStream then behaves exactly like Scan).
	OnTick func(node *tree.Node, stats Stats)
	// Period bounds how often OnTick fires. Defaults to ~60ms when zero or less.
	Period time.Duration
}

// ScanStream walks root and returns the measured tree, invoking p.OnTick with
// throttled snapshots as the tree is built so a UI can render progressively
// instead of waiting for the full walk. The returned tree is the complete,
// authoritative result; OnTick snapshots are partial views along the way.
//
// The scan maintains one live tree guarded by a mutex: every measured entry
// propagates its contribution up to the root, so each node's aggregates are
// always a correct partial total — never stale, never over-counted. Snapshots
// are bounded shallow clones handed to a single emitter goroutine, decoupling the scan from
// a slow consumer: the scan never blocks on delivery (it drops a snapshot only
// when the emitter falls behind, since a fresher one follows).
func ScanStream(ctx context.Context, root string, opts Options, p Progress) (*tree.Node, Stats, error) {
	if err := ctx.Err(); err != nil {
		return nil, Stats{}, err
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = max(1, runtime.GOMAXPROCS(0))
	}
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, Stats{}, err
	}
	rootDisplay := filepath.Clean(root)
	info, err := os.Lstat(rootAbs)
	if err != nil {
		return nil, Stats{}, err
	}
	rootFSPath := rootAbs
	if opts.Policy.FollowSymlinks {
		alias := followedAliasMode(info.Mode())
		if runtime.GOOS == windowsOS && !info.IsDir() {
			// Junctions can be reported as non-directories without an alias mode
			// bit. A followed Stat that changes the shape to a directory proves
			// this is a directory reparse point rather than a regular file root.
			if followed, statErr := os.Stat(rootAbs); statErr == nil && followed.IsDir() {
				info = followed
				alias = true
			}
		}
		if alias {
			if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
				info, err = os.Stat(rootAbs)
				if err != nil {
					return nil, Stats{}, err
				}
			}
			// Resolve the path for filesystem-type lookup. The tree keeps the
			// user-supplied symlink name, while traversal and accounting use the
			// target's metadata, just like followed symlinks below the root.
			if resolved, resolveErr := filepath.EvalSymlinks(rootAbs); resolveErr == nil {
				rootFSPath = resolved
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, Stats{}, err
	}
	rootFS := opts.Policy.FSOf(rootFSPath)
	if !opts.Policy.AllowsFilesystem(rootFS) {
		fsName := rootFS
		if fsName == "" {
			fsName = "unknown"
		}
		return nil, Stats{RootFS: rootFS}, fmt.Errorf(
			"scan root %s is on filesystem %q, which is not allowed by the filesystem policy",
			rootDisplay,
			fsName,
		)
	}
	if !opts.Policy.AllowsPath(rootAbs) {
		return nil, Stats{RootFS: rootFS}, fmt.Errorf(
			"scan root %s is not allowed by the path policy",
			rootDisplay,
		)
	}
	if rootFSPath != rootAbs && !opts.Policy.AllowsPath(rootFSPath) {
		return nil, Stats{RootFS: rootFS}, fmt.Errorf(
			"scan root symlink target %s is not allowed by the path policy",
			rootFSPath,
		)
	}
	if !info.IsDir() && !opts.Policy.File(info.Size()) {
		return nil, Stats{RootFS: rootFS}, fmt.Errorf(
			"scan root file %s does not satisfy the size policy",
			rootDisplay,
		)
	}
	s := &scanner{
		ctx:          ctx,
		opts:         opts,
		sem:          make(chan struct{}, opts.Concurrency),
		dirSlots:     make(chan struct{}, max(0, opts.Concurrency-1)),
		statJobs:     make(chan statTask, opts.Concurrency),
		rootFS:       rootFS,
		fileGroups:   make(map[fileKey]*fileGroup),
		dirBatchSize: opts.directoryBatchSize,
	}
	if opts.Policy.FollowSymlinks {
		s.loopMu = &sync.Mutex{}
		s.dirGroups = make(map[dirKey]*dirGroup)
		s.dirSelf = make(map[*tree.Node]dirMetadata)
	}
	period := p.Period
	if period <= 0 {
		period = 60 * time.Millisecond
	}
	lt := &liveTree{
		rootFS: rootFS,
		files:  &s.files,
		dirs:   &s.dirs,
		errors: &s.errors,
		onTick: p.OnTick,
		period: period,
	}
	if lt.onTick != nil {
		// A single emitter drains snapshots in scan order and calls onTick, so
		// delivery is ordered without the scan ever blocking on a slow consumer.
		lt.out = make(chan snapshot, snapshotBuffer)
		lt.done = make(chan struct{})
		go lt.pump()
	}

	start := time.Now()
	var node *tree.Node
	if info.IsDir() {
		s.startStatWorkers(opts.Concurrency)
		node = &tree.Node{Name: filepath.Base(rootAbs), IsDir: true, Depth: 0}
		var rootGroup *dirGroup
		if s.loopMu != nil {
			rootGroup = s.registerRootDirectory(rootFSPath, info, node)
		}
		lt.root = node
		s.scanDir(rootAbs, "", info, devOfPath(rootAbs, info), rootFS, 0, node, rootGroup, lt)
		s.stopStatWorkers()
		// Identity discovery is concurrent and therefore provisional. Select final
		// directory and file owners only after traversal, then rebuild aggregates
		// so the authoritative tree is independent of goroutine scheduling.
		lt.record(func() {
			if rootGroup != nil {
				if err := s.canonicalizeDirectoryOwners(rootGroup); err != nil {
					atomic.AddInt64(&s.errors, 1)
					if node.Err == nil {
						node.Err = err
					}
				}
			}
			s.normalizeFileOwners()
			if rootGroup != nil {
				s.recomputeDirectory(node)
			}
		})
	} else {
		node = s.makeFile(filepath.Base(rootAbs), info, 0)
		atomic.AddInt64(&s.files, 1)
		lt.root = node
	}
	// The root's relative path is "" by construction (Path() returns "" for a
	// parentless node), so nothing to canonicalise here.

	// Publish a final snapshot reflecting the complete tree, then stop the
	// emitter so no goroutine outlives the call and the final tree supersedes
	// every snapshot.
	if lt.out != nil {
		lt.emitFinal(s.ctx.Err() == nil && atomic.LoadInt64(&s.errors) == 0)
		close(lt.out)
		<-lt.done
	}

	scanErr := ctx.Err()
	errorCount := atomic.LoadInt64(&s.errors)
	stats := Stats{
		Files:    int(atomic.LoadInt64(&s.files)),
		Dirs:     int(atomic.LoadInt64(&s.dirs)),
		Errors:   errorCount,
		Elapsed:  time.Since(start),
		RootFS:   rootFS,
		Complete: scanErr == nil && errorCount == 0,
	}
	return node, stats, scanErr
}

type scanner struct {
	ctx        context.Context
	opts       Options
	sem        chan struct{} // bounds concurrent directory reads
	dirSlots   chan struct{} // bounds recursive directory goroutines
	statJobs   chan statTask // shared, process-wide entry-stat pool
	statWG     sync.WaitGroup
	rootFS     string
	files      int64
	dirs       int64
	errors     int64
	loopMu     *sync.Mutex // non-nil only when following symlinks
	dirGroups  map[dirKey]*dirGroup
	dirOrder   []*dirGroup
	dirSelf    map[*tree.Node]dirMetadata
	fileMu     sync.Mutex
	fileGroups map[fileKey]*fileGroup
	// openDir is nil in production. Tests and benchmarks may replace directory
	// contents without creating hundreds of thousands of real filesystem entries.
	openDir      func(string) (directoryReader, error)
	dirBatchSize int
	batchDone    func() // optional deterministic cancellation/measurement seam
}

type directoryReader interface {
	ReadDir(int) ([]os.DirEntry, error)
	Close() error
}

// defaultDirectoryBatchSize bounds transient directory-entry, stat-result,
// classification-plan, and recursion-record storage. The authoritative tree
// still retains one node per visible entry, but traversal overhead no longer
// scales with the width of a single directory.
const (
	defaultDirectoryBatchSize = 4096
	windowsOS                 = "windows"
)

// dirKey identifies one physical directory. Stable device/file identities are
// preferred; canonical paths keep loop protection and alias grouping available
// on platforms or filesystems that cannot expose them.
type dirKey struct {
	dev, ino uint64
	path     string
}

// dirGroup is one physical directory in the followed-directory graph. Exactly
// one node is scanned; edges retain every visible directory entry that reached
// an identity so a deterministic representative can be selected afterwards.
type dirGroup struct {
	key   dirKey
	node  *tree.Node
	edges []dirEdge
}

type dirEdge struct {
	name  string
	group *dirGroup
}

type dirMetadata struct {
	alloc   int64
	modTime time.Time
}

// fileKey identifies files that must contribute bytes only once. Unix file
// identities cover hardlinks and followed aliases; the canonical-path fallback
// covers followed aliases on platforms without stable device/inode metadata.
type fileKey struct {
	dev, ino uint64
	path     string
}

type fileGroup struct {
	nodes           []*tree.Node
	apparent, alloc int64
}

func (s *scanner) acquire() bool {
	select {
	case s.sem <- struct{}{}:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *scanner) release() { <-s.sem }

// fstypeOf resolves a path's filesystem type via the policy's mount table.
func (s *scanner) fstypeOf(abs string) string { return s.opts.Policy.FSOf(abs) }

// snapshot is one progress frame handed to the emitter: a consistent clone of the
// live tree plus the counters at clone time.
type snapshot struct {
	node  *tree.Node
	stats Stats
}

// snapshotBuffer bounds how many snapshots may queue between the scan and the
// emitter. A buffer of 1 means at most one outstanding snapshot exists at a
// time: a fresher one always supersedes a stale one, so there is no value in
// queuing several.
const snapshotBuffer = 1

// A progress snapshot is a ShallowClone — only the top levels a UI shows, never
// the leaves beneath — so these two constants bound every snapshot's cost
// regardless of total tree size. That is what lets progress keep streaming on a
// multi-million-entry tree instead of freezing (the old behaviour deep-cloned
// the whole tree every tick and hard-stopped past 250k entries, which looked
// like the tool had hung).
const (
	snapshotDepth = 2 // root + two levels of children
	snapshotCap   = 4096
)

// liveTree is the mutex-protected, incrementally-aggregated tree shared across
// scan goroutines. Structure changes (Adopt), aggregate updates, and snapshot
// clones all happen under mu, so the tree is race-free and every clone is a
// consistent moment-in-time view.
type liveTree struct {
	mu     sync.Mutex
	root   *tree.Node
	rootFS string
	files  *int64
	dirs   *int64
	errors *int64
	onTick func(*tree.Node, Stats)
	period time.Duration
	last   time.Time

	out  chan snapshot // scan -> emitter; nil when streaming is disabled
	done chan struct{} // closed when the emitter has exited
}

// record runs a locked mutation, then — at most once per period and only when
// the emitter has room — takes a bounded shallow snapshot and enqueues it.
// Everything happens under mu so snapshots are consistent and enqueue order
// matches scan order. Because each snapshot is a ShallowClone capped at
// snapshotCap nodes, this is cheap for any tree size, so progress never stops
// (it just throttles); the old per-node-limit hard stop made large scans look
// frozen.
func (lt *liveTree) record(mutate func()) {
	lt.mu.Lock()
	mutate()
	if lt.onTick != nil {
		now := time.Now()
		if now.Sub(lt.last) >= lt.period && len(lt.out) < cap(lt.out) {
			lt.last = now
			snap := snapshot{node: lt.root.ShallowClone(snapshotDepth, snapshotCap), stats: lt.stats()}
			select {
			case lt.out <- snap:
			default: // unreachable given the len check; guarded anyway
			}
		}
	}
	lt.mu.Unlock()
}

// emitFinal publishes one last shallow snapshot reflecting the complete tree, so
// the streamed view reaches its final total before the authoritative
// scanDoneMsg. It bypasses the throttle by design. Non-blocking: if the buffer
// is somehow full, scanDoneMsg supersedes it anyway.
func (lt *liveTree) emitFinal(complete bool) {
	if lt.onTick == nil {
		return
	}
	lt.mu.Lock()
	stats := lt.stats()
	stats.Complete = complete
	snap := snapshot{node: lt.root.ShallowClone(snapshotDepth, snapshotCap), stats: stats}
	lt.mu.Unlock()
	select {
	case lt.out <- snap:
	default:
	}
}

// pump is the single emitter: it drains snapshots in order and invokes onTick,
// so delivery is sequential and a blocking onTick never stalls the scan.
func (lt *liveTree) pump() {
	defer close(lt.done)
	for snap := range lt.out {
		lt.onTick(snap.node, snap.stats)
	}
}

func (lt *liveTree) stats() Stats {
	return Stats{
		Files:  int(atomic.LoadInt64(lt.files)),
		Dirs:   int(atomic.LoadInt64(lt.dirs)),
		Errors: atomic.LoadInt64(lt.errors),
		RootFS: lt.rootFS,
	}
}

// propagateSelf spreads a directory's own inode allocation and mtime up to its
// ancestors. The node's Alloc already holds its own inode allocation; only the
// ancestors need updating. Called once when a directory begins streaming.
func (*liveTree) propagateSelf(n *tree.Node) {
	alloc, mtime := n.Alloc, n.ModTime
	for p := n.Parent(); p != nil; p = p.Parent() {
		p.Alloc += alloc
		if mtime.After(p.ModTime) {
			p.ModTime = mtime
		}
	}
}

// propagateFile adds a freshly-adopted file/error leaf's contribution to every
// ancestor (its own fields are already final).
func (*liveTree) propagateFile(leaf *tree.Node) {
	apparent, alloc, mtime := leaf.Apparent, leaf.Alloc, leaf.ModTime
	for p := leaf.Parent(); p != nil; p = p.Parent() {
		p.Apparent += apparent
		p.Alloc += alloc
		// A failed stat is an error entry, not a measured file. Keep it visible
		// in the tree without inflating file totals relative to Stats.Files.
		if leaf.Err == nil {
			p.FileCount++
		}
		if mtime.After(p.ModTime) {
			p.ModTime = mtime
		}
	}
}

// bumpDirCount records that a new directory exists by incrementing every
// ancestor's DirCount. The directory's own subtree counts propagate later, as it
// is scanned, via propagateFile/bumpDirCount on each of its entries.
func (*liveTree) bumpDirCount(child *tree.Node) {
	for p := child.Parent(); p != nil; p = p.Parent() {
		p.DirCount++
	}
}

// scanDir populates an already-linked directory node n, propagating every
// measured size and count up to the live root and emitting throttled snapshots.
// n must already be attached to the live tree (its parent set); the root is the
// only node with no parent. Direct files are revealed immediately and directory
// children appear as zero-size placeholders that fill in as they are scanned, so
// the tree populates top-down and every node's total climbs monotonically.
func (s *scanner) scanDir(abs, rel string, info fs.FileInfo, dev uint64, fstype string, depth int, n *tree.Node, group *dirGroup, lt *liveTree) {
	atomic.AddInt64(&s.dirs, 1)
	// Directory identity, allocation, and mtime are known from the parent stat.
	// Publish them before ReadDir so a permission or race failure leaves an
	// explicitly incomplete node with its known self metadata instead of a
	// misleading zero-size placeholder.
	lt.record(func() {
		n.Alloc = allocBytes(info)
		n.ModTime = info.ModTime()
		if s.dirSelf != nil {
			s.dirSelf[n] = dirMetadata{alloc: n.Alloc, modTime: n.ModTime}
		}
		lt.propagateSelf(n)
	})
	// os.File.ReadDir(n) follows native directory order rather than os.ReadDir's
	// lexical order. Sort only the retained node pointers after the directory is
	// complete, preserving the historical deterministic tree without another
	// full-width entry/result/plan allocation.
	defer lt.record(func() {
		sort.Slice(n.Children, func(i, j int) bool {
			return n.Children[i].Name < n.Children[j].Name
		})
	})

	if err := s.ctx.Err(); err != nil {
		lt.record(func() { n.Err = err })
		return
	}
	// Read the directory under the I/O semaphore so directory-read concurrency
	// never exceeds Concurrency.
	if !s.acquire() {
		lt.record(func() { n.Err = s.ctx.Err() })
		return
	}
	if err := s.ctx.Err(); err != nil {
		s.release()
		lt.record(func() { n.Err = err })
		return
	}
	dir, rerr := s.openDirectory(abs)
	s.release()
	if rerr != nil {
		atomic.AddInt64(&s.errors, 1)
		lt.record(func() { n.Err = rerr })
		return
	}
	defer func() { _ = dir.Close() }()

	for {
		if err := s.ctx.Err(); err != nil {
			lt.record(func() { n.Err = err })
			return
		}
		if !s.acquire() {
			lt.record(func() { n.Err = s.ctx.Err() })
			return
		}
		entries, readErr := dir.ReadDir(s.directoryBatchSize())
		s.release()
		if len(entries) > 0 && !s.processDirectoryBatch(abs, rel, dev, fstype, depth, n, group, entries, lt) {
			return
		}
		if len(entries) > 0 && s.batchDone != nil {
			s.batchDone()
		}
		if readErr == nil && len(entries) > 0 {
			continue
		}
		if readErr == io.EOF {
			return
		}
		if readErr == nil {
			readErr = io.ErrNoProgress
		}
		atomic.AddInt64(&s.errors, 1)
		lt.record(func() { n.Err = readErr })
		return
	}
}

func (s *scanner) openDirectory(path string) (directoryReader, error) {
	if s.openDir != nil {
		return s.openDir(path)
	}
	return os.Open(path)
}

func (s *scanner) directoryBatchSize() int {
	if s.dirBatchSize > 0 {
		return s.dirBatchSize
	}
	return defaultDirectoryBatchSize
}

type entryPlan struct {
	node *tree.Node
	dir  *dirChild
}

type directoryRecursion struct {
	dir   dirChild
	child *tree.Node
}

// processDirectoryBatch owns every transient slice for one bounded batch. It
// filters entries in place, waits for the shared stat pool, classifies results,
// publishes final nodes/placeholders, and completes directory recursion before
// the caller reads another batch.
func (s *scanner) processDirectoryBatch(abs, rel string, dev uint64, fstype string, depth int, n *tree.Node, group *dirGroup, entries []os.DirEntry, lt *liveTree) bool {
	write := 0
	for _, entry := range entries {
		if s.ctx.Err() != nil {
			break
		}
		name := entry.Name()
		entryRel := joinRel(rel, name)
		if s.opts.Policy.Entry(entryRel, name, filepath.Join(abs, name)) {
			entries[write] = entry
			write++
		}
	}
	clear(entries[write:])
	if err := s.ctx.Err(); err != nil {
		lt.record(func() { n.Err = err })
		return false
	}

	results := s.statAll(abs, entries[:write])
	plans := make([]entryPlan, 0, len(results))
	for _, result := range results {
		if !result.complete || s.ctx.Err() != nil {
			break
		}
		node, dir := s.classifyEntry(abs, rel, dev, fstype, depth, result)
		plans = append(plans, entryPlan{node: node, dir: dir})
	}
	cancelErr := s.ctx.Err()

	var recursions []directoryRecursion
	lt.record(func() {
		if cancelErr != nil {
			n.Err = cancelErr
		}
		for _, plan := range plans {
			switch {
			case plan.node != nil:
				n.Adopt(plan.node)
				lt.propagateFile(plan.node)
			case plan.dir != nil:
				if group != nil {
					group.edges = append(group.edges, dirEdge{name: plan.dir.name, group: plan.dir.group})
				}
				if !plan.dir.scan {
					continue
				}
				child := &tree.Node{Name: plan.dir.name, IsDir: true, Depth: depth + 1}
				if plan.dir.group != nil {
					plan.dir.group.node = child
				}
				n.Adopt(child)
				lt.bumpDirCount(child)
				recursions = append(recursions, directoryRecursion{dir: *plan.dir, child: child})
			}
		}
	})
	if cancelErr != nil {
		return false
	}

	s.scanDirectoryChildren(recursions, depth, lt)
	if err := s.ctx.Err(); err != nil {
		lt.record(func() { n.Err = err })
		return false
	}
	return true
}

func (s *scanner) scanDirectoryChildren(recursions []directoryRecursion, depth int, lt *liveTree) {
	var wg sync.WaitGroup
	for _, recursion := range recursions {
		if err := s.ctx.Err(); err != nil {
			lt.record(func() { recursion.child.Err = err })
			continue
		}
		select {
		case s.dirSlots <- struct{}{}:
			wg.Add(1)
			go func(recursion directoryRecursion) {
				defer wg.Done()
				defer func() { <-s.dirSlots }()
				dir := recursion.dir
				s.scanDir(dir.abs, dir.rel, dir.info, dir.dev, dir.fs, depth+1, recursion.child, dir.group, lt)
			}(recursion)
		default:
			// All traversal slots are occupied. Recurse in this goroutine so
			// progress continues without creating an unbounded waiter set.
			dir := recursion.dir
			s.scanDir(dir.abs, dir.rel, dir.info, dir.dev, dir.fs, depth+1, recursion.child, dir.group, lt)
		}
	}
	wg.Wait()
}

// dirChild describes a directory entry selected for recursion. It carries
// everything scanDir needs to descend without re-statting.
type dirChild struct {
	abs, rel, name string
	info           fs.FileInfo
	dev            uint64
	fs             string
	group          *dirGroup
	scan           bool
}

// classifyEntry applies the scope policy to one stat result and returns either a
// complete leaf node (for files and stat errors) or a dirChild describing a
// directory to recurse into. Entries filtered out by policy yield both nil. It
// also bumps the scanner's file/error counters, so each result is counted once.
func (s *scanner) classifyEntry(abs, rel string, parentDev uint64, parentFS string, depth int, r statResult) (*tree.Node, *dirChild) {
	eabs := filepath.Join(abs, r.name)
	er := joinRel(rel, r.name)
	if r.err != nil {
		return s.errorNode(r.name, r.err, depth+1), nil
	}
	info := r.info
	targetPath := eabs
	if r.resolved != "" {
		targetPath = r.resolved
	}
	childDev := devOfPath(eabs, info)
	childFS := parentFS
	if childDev != parentDev || r.resolved != "" {
		childFS = s.fstypeOf(targetPath)
	}
	// Entry() already checked the visible alias. Apply the complete boundary
	// policy to the resolved target as well so followed file and directory
	// symlinks cannot escape include/exclude paths, cross-device rules, or
	// filesystem filters.
	if !s.opts.Policy.AllowsTarget(targetPath, childDev, parentDev, childFS) {
		return nil, nil
	}
	if info.IsDir() {
		dir := &dirChild{
			abs: eabs, rel: er, name: r.name, info: info, dev: childDev, fs: childFS,
			scan: true,
		}
		if s.loopMu != nil {
			dir.group, dir.scan = s.claimDirectory(eabs, info)
		}
		return nil, dir
	}
	if !s.opts.Policy.File(info.Size()) {
		return nil, nil
	}
	atomic.AddInt64(&s.files, 1)
	return s.deduplicatedFile(eabs, r.resolved, r.name, info, depth+1), nil
}

type statResult struct {
	name     string
	info     fs.FileInfo
	resolved string // followed symlink target, empty for ordinary entries
	err      error
	complete bool
}

type statTask struct {
	parent string
	entry  os.DirEntry
	index  int
	out    []statResult
	done   *sync.WaitGroup
}

// statAll submits every entry under parent to the scanner-wide worker pool.
// A single global pool prevents concurrent wide directories from multiplying
// Concurrency workers per directory.
func (s *scanner) statAll(parent string, entries []os.DirEntry) []statResult {
	n := len(entries)
	out := make([]statResult, n)
	if n == 0 {
		return out
	}
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		task := statTask{parent: parent, entry: entry, index: i, out: out, done: &wg}
		select {
		case s.statJobs <- task:
		case <-s.ctx.Done():
			wg.Done()
			wg.Wait()
			return out
		}
	}
	wg.Wait()
	return out
}

func (s *scanner) startStatWorkers(count int) {
	for range count {
		s.statWG.Add(1)
		go func() {
			defer s.statWG.Done()
			for task := range s.statJobs {
				s.runStatTask(task)
			}
		}()
	}
}

func (s *scanner) stopStatWorkers() {
	close(s.statJobs)
	s.statWG.Wait()
}

func (s *scanner) runStatTask(task statTask) {
	defer task.done.Done()
	if s.ctx.Err() != nil {
		return
	}
	info, alias, err := s.statEntry(task.parent, task.entry)
	resolved := ""
	if err == nil && s.opts.Policy.FollowSymlinks && alias {
		resolved, _ = filepath.EvalSymlinks(filepath.Join(task.parent, task.entry.Name()))
	}
	if s.ctx.Err() != nil {
		return
	}
	task.out[task.index] = statResult{
		name: task.entry.Name(), info: info, resolved: resolved, err: err, complete: true,
	}
}

// statEntry stats a single entry, following symlinks when configured. The
// returned alias bit records whether the visible path was a link/reparse alias
// so callers can resolve the target for boundary and identity checks without
// canonicalizing ordinary Windows paths (which may change long/short spelling).
func (s *scanner) statEntry(parent string, e os.DirEntry) (fs.FileInfo, bool, error) {
	p := filepath.Join(parent, e.Name())
	if !s.opts.Policy.FollowSymlinks {
		info, err := e.Info()
		return info, false, err
	}
	if runtime.GOOS != windowsOS {
		alias := followedAliasEntry(e)
		if alias {
			info, err := os.Stat(p)
			return info, true, err
		}
		info, err := e.Info()
		return info, false, err
	}

	// Windows directory enumeration does not consistently expose mount-point
	// reparse tags through DirEntry.Type. Lstat gives us the un-followed mode,
	// while Stat supplies the target shape needed to classify junctions as
	// directories. Keep the alias bit separate so regular files retain their
	// lexical include-path spelling.
	lstat, err := os.Lstat(p)
	if err != nil {
		return nil, false, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, false, err
	}
	alias := followedAliasMode(lstat.Mode()) || (!lstat.IsDir() && info.IsDir())
	return info, alias, nil
}

func followedAliasEntry(e os.DirEntry) bool {
	return followedAliasMode(e.Type())
}

func followedAliasMode(mode fs.FileMode) bool {
	typ := mode
	if typ&fs.ModeSymlink != 0 {
		return true
	}
	// Go exposes Windows mount-point junctions as ModeIrregular because their
	// reparse tag is a name surrogate but not an ordinary symbolic link.
	return runtime.GOOS == windowsOS && typ&fs.ModeIrregular != 0
}

// registerRootDirectory seeds the followed-directory graph before traversal.
// A later edge back to the root therefore remains an alias edge and is never
// recursed into, preserving loop protection without relying on discovery order.
func (s *scanner) registerRootDirectory(path string, info fs.FileInfo, node *tree.Node) *dirGroup {
	key := directoryIdentity(path, info)
	group := &dirGroup{key: key, node: node}
	s.dirGroups[key] = group
	s.dirOrder = append(s.dirOrder, group)
	return group
}

// claimDirectory returns the identity group for path and whether this caller
// must scan it. The first claimant does the I/O; all later claimants still add
// graph edges so final display ownership can be selected deterministically.
func (s *scanner) claimDirectory(path string, info fs.FileInfo) (*dirGroup, bool) {
	key := directoryIdentity(path, info)
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	if group, ok := s.dirGroups[key]; ok {
		return group, false
	}
	group := &dirGroup{key: key}
	s.dirGroups[key] = group
	s.dirOrder = append(s.dirOrder, group)
	return group, true
}

func directoryIdentity(path string, info fs.FileInfo) dirKey {
	// Prefer the native file identity first. Windows deliberately leaves
	// same-volume junctions unchanged from EvalSymlinks (including recursive
	// junctions), so path canonicalization alone cannot collapse a junction back
	// to the root. Opening the followed path gives GetFileInformationByHandle
	// the target's volume/file identity and keeps the alias edge non-recursive.
	if dev, ino, ok := identityOfPath(path, info); ok {
		return dirKey{dev: dev, ino: ino}
	}
	if runtime.GOOS == windowsOS {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return dirKey{path: canonicalPath(resolved)}
		}
	}
	return fallbackDirectoryIdentity(path)
}

func fallbackDirectoryIdentity(path string) dirKey {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	return dirKey{path: canonicalPath(resolved)}
}

func canonicalPath(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == windowsOS {
		return strings.ToLower(path)
	}
	return path
}

type dirPathCandidate struct {
	path   string
	name   string
	parent *dirGroup
	group  *dirGroup
}

type dirPathHeap []dirPathCandidate

func (h dirPathHeap) Len() int { return len(h) }

func (h dirPathHeap) Less(i, j int) bool {
	if h[i].path != h[j].path {
		return h[i].path < h[j].path
	}
	return dirKeyLess(h[i].group.key, h[j].group.key)
}

func (h dirPathHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *dirPathHeap) Push(value any) {
	*h = append(*h, value.(dirPathCandidate))
}

func (h *dirPathHeap) Pop() any {
	old := *h
	n := len(old)
	value := old[n-1]
	old[n-1] = dirPathCandidate{}
	*h = old[:n-1]
	return value
}

func dirKeyLess(a, b dirKey) bool {
	if a.path != b.path {
		return a.path < b.path
	}
	if a.dev != b.dev {
		return a.dev < b.dev
	}
	return a.ino < b.ino
}

// canonicalizeDirectoryOwners selects the lexicographically first reachable
// path to each physical directory. It is a best-first traversal of the complete
// alias graph: a group is assigned only once, so back-edges and cross-links can
// never create a cycle in the materialized tree.
func (s *scanner) canonicalizeDirectoryOwners(root *dirGroup) error {
	if root == nil {
		return nil
	}
	assigned := map[*dirGroup]bool{root: true}
	frontier := &dirPathHeap{}
	heap.Init(frontier)
	pushDirectoryEdges(frontier, root, "")
	for frontier.Len() > 0 {
		candidate := heap.Pop(frontier).(dirPathCandidate)
		if assigned[candidate.group] {
			continue
		}
		if candidate.group.node == nil {
			// Cancellation can leave a claimed identity without a started scan.
			// The scan is already incomplete; there is no subtree to relocate.
			assigned[candidate.group] = true
			continue
		}
		if candidate.parent == nil || candidate.parent.node == nil {
			return fmt.Errorf("scan: directory alias %q has no measured parent", candidate.path)
		}
		node := candidate.group.node
		if node.Parent() != candidate.parent.node || node.Name != candidate.name {
			if !node.MoveTo(candidate.parent.node, candidate.name) {
				return fmt.Errorf("scan: cannot select directory alias %q", candidate.path)
			}
		}
		assigned[candidate.group] = true
		pushDirectoryEdges(frontier, candidate.group, candidate.path)
	}
	for _, group := range s.dirOrder {
		if group.node != nil && !assigned[group] {
			return fmt.Errorf("scan: measured directory identity is unreachable from root")
		}
	}
	return nil
}

func pushDirectoryEdges(frontier *dirPathHeap, parent *dirGroup, parentPath string) {
	for _, edge := range parent.edges {
		if edge.group == nil {
			continue
		}
		path := filepath.ToSlash(edge.name)
		if parentPath != "" {
			path = parentPath + "/" + path
		}
		heap.Push(frontier, dirPathCandidate{
			path: path, name: edge.name, parent: parent, group: edge.group,
		})
	}
}

// recomputeDirectory rebuilds a final directory's aggregate fields after alias
// relocation and file-owner normalization. Directory self metadata is kept
// separately because the live ModTime and Alloc fields already include children.
func (s *scanner) recomputeDirectory(node *tree.Node) {
	self := s.dirSelf[node]
	apparent, alloc := int64(0), self.alloc
	files, dirs := 0, 0
	modTime := self.modTime
	for _, child := range node.Children {
		if child.IsDir {
			s.recomputeDirectory(child)
			dirs += child.DirCount + 1
			files += child.FileCount
		} else if child.Err == nil {
			files++
		}
		apparent += child.Apparent
		alloc += child.Alloc
		if child.ModTime.After(modTime) {
			modTime = child.ModTime
		}
	}
	node.Apparent = apparent
	node.Alloc = alloc
	node.FileCount = files
	node.DirCount = dirs
	node.ModTime = modTime
}

// deduplicatedFile builds a file node and registers identities that can appear
// under more than one path. Without symlink following, only files whose link
// count proves a hardlink needs the map. With following enabled, every file is
// registered so a one-link target and any number of symlink aliases still
// contribute their bytes only once.
func (s *scanner) deduplicatedFile(abs, resolved, name string, info fs.FileInfo, depth int) *tree.Node {
	key, deduplicate := s.fileIdentity(abs, resolved, info)
	if !deduplicate {
		return s.makeFile(name, info, depth)
	}

	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if group, ok := s.fileGroups[key]; ok {
		node := s.hardlinkNode(name, info, depth)
		group.nodes = append(group.nodes, node)
		return node
	}

	node := s.makeFile(name, info, depth)
	s.fileGroups[key] = &fileGroup{
		nodes:    []*tree.Node{node},
		apparent: node.Apparent,
		alloc:    node.Alloc,
	}
	return node
}

func (s *scanner) fileIdentity(abs, resolved string, info fs.FileInfo) (fileKey, bool) {
	if dev, ino, ok := identityOfPath(abs, info); ok {
		if s.opts.Policy.FollowSymlinks || linkCountPath(abs, info) > 1 {
			return fileKey{dev: dev, ino: ino}, true
		}
		return fileKey{}, false
	}
	if !s.opts.Policy.FollowSymlinks {
		return fileKey{}, false
	}

	target := resolved
	if target == "" {
		var err error
		target, err = filepath.EvalSymlinks(abs)
		if err != nil {
			target = abs
		}
	}
	return fileKey{path: canonicalPath(target)}, true
}

// normalizeFileOwners makes the lexicographically first relative path the
// byte-owning entry for every duplicate identity. Discovery is concurrent, so
// its first claimant is intentionally provisional; moving the contribution
// after traversal makes repeated scans structurally stable while preserving
// the already-correct grand total.
func (s *scanner) normalizeFileOwners() {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	for _, group := range s.fileGroups {
		if len(group.nodes) < 2 {
			continue
		}
		owner := group.nodes[0]
		for _, node := range group.nodes[1:] {
			if node.Path() < owner.Path() {
				owner = node
			}
		}
		for _, node := range group.nodes {
			apparent, alloc := int64(0), int64(0)
			if node == owner {
				apparent, alloc = group.apparent, group.alloc
			}
			deltaApparent := apparent - node.Apparent
			deltaAlloc := alloc - node.Alloc
			node.Apparent = apparent
			node.Alloc = alloc
			node.Hardlink = node != owner
			for parent := node.Parent(); parent != nil; parent = parent.Parent() {
				parent.Apparent += deltaApparent
				parent.Alloc += deltaAlloc
			}
		}
	}
}

// makeFile builds a leaf file node from its stat info.
func (*scanner) makeFile(name string, info fs.FileInfo, depth int) *tree.Node {
	return &tree.Node{
		Name:     name,
		IsDir:    false,
		Depth:    depth,
		Apparent: info.Size(),
		Alloc:    allocBytes(info),
		ModTime:  info.ModTime(),
	}
}

// hardlinkNode builds a zero-size leaf for an already-counted file identity: it
// keeps the name and mtime so a hardlink or followed alias remains visible and
// sortable without contributing the same bytes twice.
func (*scanner) hardlinkNode(name string, info fs.FileInfo, depth int) *tree.Node {
	return &tree.Node{
		Name:     name,
		IsDir:    false,
		Depth:    depth,
		ModTime:  info.ModTime(),
		Hardlink: true,
	}
}

// errorNode builds a leaf node carrying a non-fatal stat error.
func (s *scanner) errorNode(name string, err error, depth int) *tree.Node {
	atomic.AddInt64(&s.errors, 1)
	return &tree.Node{Name: name, Depth: depth, Err: err}
}

// joinRel builds a relative path component, keeping the root's own rel as "".
func joinRel(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + string(filepath.Separator) + name
}
