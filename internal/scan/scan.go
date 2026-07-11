// Package scan walks a filesystem path and builds a tree.Node tree of measured
// sizes. It is concurrent: each directory is processed by its own goroutine
// (so wide, deep trees parallelise across directories), with the number of
// active directory traversals and entry stat workers both bounded by
// Concurrency (GOMAXPROCS by default).
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
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	Files   int           // regular files measured
	Dirs    int           // directories measured (including root if dir)
	Errors  int64         // non-fatal per-entry errors
	Elapsed time.Duration // wall-clock scan time
	RootFS  string        // filesystem type of the root, if known
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
	if opts.Policy.FollowSymlinks && info.Mode()&fs.ModeSymlink != 0 {
		info, err = os.Stat(rootAbs)
		if err != nil {
			return nil, Stats{}, err
		}
		// Resolve the path for filesystem-type lookup. The tree keeps the
		// user-supplied symlink name, while traversal and accounting use the
		// target's metadata, just like followed symlinks below the root.
		if resolved, resolveErr := filepath.EvalSymlinks(rootAbs); resolveErr == nil {
			rootFSPath = resolved
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
		visited:      make(map[devIno]struct{}),
		visitedPaths: make(map[string]struct{}),
		fileGroups:   make(map[fileKey]*fileGroup),
	}
	if opts.Policy.FollowSymlinks {
		s.loopMu = &sync.Mutex{}
		if info.IsDir() {
			// Seed the target inode before walking. Otherwise a symlink from a
			// descendant back to the root is accepted once and the entire root
			// subtree is measured a second time before the loop is noticed.
			if dev, ino, ok := identityOfPath(rootFSPath, info); ok {
				s.visited[devIno{dev, ino}] = struct{}{}
			} else {
				s.visitedPaths[canonicalPath(rootFSPath)] = struct{}{}
			}
		}
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
		lt.root = node
		s.scanDir(rootAbs, "", info, devOfPath(rootAbs, info), rootFS, 0, node, lt)
		s.stopStatWorkers()
		// Directory scans can discover the same file identity from concurrent
		// branches (hardlinks or followed aliases). Streaming counts it once as
		// soon as it is seen; this final pass makes the owning display path
		// deterministic without changing the total.
		lt.record(s.normalizeFileOwners)
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
		lt.emitFinal()
		close(lt.out)
		<-lt.done
	}

	stats := Stats{
		Files:   int(atomic.LoadInt64(&s.files)),
		Dirs:    int(atomic.LoadInt64(&s.dirs)),
		Errors:  atomic.LoadInt64(&s.errors),
		Elapsed: time.Since(start),
		RootFS:  rootFS,
	}
	return node, stats, ctx.Err()
}

type devIno struct{ dev, ino uint64 }

type scanner struct {
	ctx          context.Context
	opts         Options
	sem          chan struct{} // bounds concurrent directory reads
	dirSlots     chan struct{} // bounds recursive directory goroutines
	statJobs     chan statTask // shared, process-wide entry-stat pool
	statWG       sync.WaitGroup
	rootFS       string
	files        int64
	dirs         int64
	errors       int64
	visited      map[devIno]struct{}
	visitedPaths map[string]struct{}
	loopMu       *sync.Mutex // non-nil only when following symlinks
	fileMu       sync.Mutex
	fileGroups   map[fileKey]*fileGroup
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
func (lt *liveTree) emitFinal() {
	if lt.onTick == nil {
		return
	}
	lt.mu.Lock()
	snap := snapshot{node: lt.root.ShallowClone(snapshotDepth, snapshotCap), stats: lt.stats()}
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
func (lt *liveTree) propagateSelf(n *tree.Node) {
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
func (lt *liveTree) propagateFile(leaf *tree.Node) {
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
func (lt *liveTree) bumpDirCount(child *tree.Node) {
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
func (s *scanner) scanDir(abs, rel string, info fs.FileInfo, dev uint64, fstype string, depth int, n *tree.Node, lt *liveTree) {
	atomic.AddInt64(&s.dirs, 1)

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
	entries, rerr := os.ReadDir(abs)
	s.release()
	if rerr != nil {
		atomic.AddInt64(&s.errors, 1)
		lt.record(func() { n.Err = rerr })
		return
	}

	// Filter by name/path/glob policy first, then stat the survivors.
	survivors := make([]os.DirEntry, 0, len(entries))
	for _, e := range entries {
		if s.ctx.Err() != nil {
			break
		}
		en := e.Name()
		er := joinRel(rel, en)
		if s.opts.Policy.Entry(er, en, filepath.Join(abs, en)) {
			survivors = append(survivors, e)
		}
	}
	stats := s.statAll(abs, survivors)

	type plan struct {
		node *tree.Node
		dir  *dirChild
	}
	plans := make([]plan, 0, len(stats))
	for _, r := range stats {
		if !r.complete || s.ctx.Err() != nil {
			break
		}
		node, dc := s.classifyEntry(abs, rel, dev, fstype, depth, r)
		plans = append(plans, plan{node, dc})
	}
	cancelErr := s.ctx.Err()

	// Adopt files and directory placeholders under the lock in one batch,
	// propagating contributions up to the root. Directory placeholders are
	// recursed into afterwards; they fill in (and their totals climb) as their
	// own scans run.
	type rec struct {
		dir   dirChild
		child *tree.Node
	}
	var recs []rec
	lt.record(func() {
		n.Alloc = allocBytes(info) // a directory contributes its own inode allocation
		n.ModTime = info.ModTime()
		if cancelErr != nil {
			n.Err = cancelErr
		}
		lt.propagateSelf(n)
		for _, pl := range plans {
			switch {
			case pl.node != nil:
				n.Adopt(pl.node)
				lt.propagateFile(pl.node)
			case pl.dir != nil:
				child := &tree.Node{Name: pl.dir.name, IsDir: true, Depth: depth + 1}
				n.Adopt(child)
				lt.bumpDirCount(child)
				recs = append(recs, rec{*pl.dir, child})
			}
		}
	})
	if cancelErr != nil {
		return
	}

	if len(recs) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, r := range recs {
		if err := s.ctx.Err(); err != nil {
			lt.record(func() { r.child.Err = err })
			continue
		}
		select {
		case s.dirSlots <- struct{}{}:
			wg.Add(1)
			go func(r rec) {
				defer wg.Done()
				defer func() { <-s.dirSlots }()
				s.scanDir(r.dir.abs, r.dir.rel, r.dir.info, r.dir.dev, r.dir.fs, depth+1, r.child, lt)
			}(r)
		default:
			// All traversal slots are occupied. Recurse in this goroutine so
			// progress continues without creating an unbounded waiter set.
			s.scanDir(r.dir.abs, r.dir.rel, r.dir.info, r.dir.dev, r.dir.fs, depth+1, r.child, lt)
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
		if s.loopMu != nil && s.seenDirectory(eabs, info) {
			return nil, nil // symlink loop or already-seen real dir
		}
		return nil, &dirChild{abs: eabs, rel: er, name: r.name, info: info, dev: childDev, fs: childFS}
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
	info, err := s.statEntry(task.parent, task.entry)
	resolved := ""
	if err == nil && s.opts.Policy.FollowSymlinks && task.entry.Type()&fs.ModeSymlink != 0 {
		resolved, _ = filepath.EvalSymlinks(filepath.Join(task.parent, task.entry.Name()))
	}
	if s.ctx.Err() != nil {
		return
	}
	task.out[task.index] = statResult{
		name: task.entry.Name(), info: info, resolved: resolved, err: err, complete: true,
	}
}

// statEntry stats a single entry, following symlinks when configured.
func (s *scanner) statEntry(parent string, e os.DirEntry) (fs.FileInfo, error) {
	p := filepath.Join(parent, e.Name())
	if s.opts.Policy.FollowSymlinks && e.Type()&fs.ModeSymlink != 0 {
		return os.Stat(p) // follow
	}
	return os.Lstat(p)
}

// markVisited records a directory's (dev,ino) and reports whether it was
// already seen (a symlink loop or a hardlinked duplicate). Used only when
// following symlinks.
func (s *scanner) markVisited(dev, ino uint64) bool {
	k := devIno{dev, ino}
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	if _, ok := s.visited[k]; ok {
		return true
	}
	s.visited[k] = struct{}{}
	return false
}

// seenDirectory records a directory by Unix file identity when available and
// falls back to its canonical path elsewhere. The fallback is essential on
// platforms whose FileInfo has no device/inode fields: treating every entry as
// (0,0) skips valid trees, while disabling tracking permits symlink loops.
func (s *scanner) seenDirectory(path string, info fs.FileInfo) bool {
	if dev, ino, ok := identityOfPath(path, info); ok {
		return s.markVisited(dev, ino)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	key := canonicalPath(resolved)
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	if _, ok := s.visitedPaths[key]; ok {
		return true
	}
	s.visitedPaths[key] = struct{}{}
	return false
}

func canonicalPath(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
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
func (s *scanner) makeFile(name string, info fs.FileInfo, depth int) *tree.Node {
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
func (s *scanner) hardlinkNode(name string, info fs.FileInfo, depth int) *tree.Node {
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
