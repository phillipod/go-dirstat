// Package index serializes a scanned tree to and from a compact, durable
// snapshot so later runs — especially interactive ones — can render instantly
// while a fresh scan refreshes in the background.
//
// The snapshot format is a flat, parent-indexed node array encoded with gob,
// prefixed by a small magic+version header. A fingerprint derived from the
// scan root and the scope policy tags each snapshot so a cached result is only
// reused when it was produced under the same options.
package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

// magicVersion guards against reading foreign or incompatible cache files. If
// the on-disk layout ever changes, bump formatVersion to invalidate old caches.
var magicVersion = []byte{'d', 's', 't', 0x03}

// formatVersion is encoded after the magic bytes.
const formatVersion byte = 2

const windowsOS = "windows"

// ErrIncompatible is returned when a cache file is the wrong format or was
// written under a different scope fingerprint.
var ErrIncompatible = errors.New("cache: incompatible snapshot")

// ErrStale is returned when a valid snapshot exceeds the store TTL.
var ErrStale = errors.New("cache: stale snapshot")

// Snapshot is the on-disk representation of one scan.
type Snapshot struct {
	Root        string // absolute path that was scanned
	Fingerprint string // scope fingerprint this snapshot is valid for
	ScannedAt   time.Time
	RootFS      string
	Files       int
	Dirs        int
	Errors      int64
	// Complete is an explicit publication invariant. Errors==0 is not enough:
	// a canceled scan can stop before observing an unreadable entry.
	Complete bool
	Nodes    []FlatNode
}

// FlatNode is one node in the flattened snapshot array.
type FlatNode struct {
	Name      string
	IsDir     bool
	Depth     int
	Apparent  int64
	Alloc     int64
	FileCount int
	DirCount  int
	ModTime   time.Time
	Hardlink  bool
	ErrMsg    string
	Parent    int // index into Nodes, -1 for the root
}

const fingerprintVersion = "scope-fingerprint-v2"

// Fingerprint returns a versioned SHA-256 hash of the root path and every scope
// option that affects scan results. Tags and values are length-prefixed, so
// embedded newlines cannot alias another policy. Two scans with the same
// fingerprint produce interchangeable trees, so a cached snapshot can be reused.
func Fingerprint(root string, p scope.Policy) string {
	h := sha256.New()
	writeFingerprintField(h, "version", fingerprintVersion)
	writeFingerprintField(h, "root", root)
	writeFingerprintField(h, "cross-device", strconv.FormatBool(p.CrossDevice))
	writeFingerprintField(h, "follow-symlinks", strconv.FormatBool(p.FollowSymlinks))
	writeFingerprintField(h, "exclude-virtual", strconv.FormatBool(p.ExcludeVirtual))
	writeFingerprintField(h, "include-hidden", strconv.FormatBool(p.IncludeHidden))
	writeFingerprintField(h, "min-size", strconv.FormatInt(p.MinSize, 10))
	writeFingerprintField(h, "max-size", strconv.FormatInt(p.MaxSize, 10))
	writeSortedFingerprintFields(h, "exclude-glob", p.ExcludeGlobs)
	writeSortedFingerprintFields(h, "exclude-path", effectiveFingerprintPaths(root, p.ExcludePaths))
	writeSortedFingerprintFields(h, "include-path", normalizedFingerprintPaths(p.IncludePaths))
	writeSortedFingerprintFields(h, "virtual-path", effectiveFingerprintPaths(root, p.VirtualPaths))
	writeFingerprintKeys(h, "include-fs", p.IncludeFS)
	writeFingerprintKeys(h, "exclude-fs", p.ExcludeFS)
	return hex.EncodeToString(h.Sum(nil))
}

func writeFingerprintField(w io.Writer, tag, value string) {
	// Fixed-width hexadecimal lengths keep the stream portable and
	// unambiguous while remaining inspectable in a debugger.
	_, _ = fmt.Fprintf(w, "%016x%s%016x%s", len(tag), tag, len(value), value)
}

func writeSortedFingerprintFields(w io.Writer, tag string, xs []string) {
	cp := append([]string(nil), xs...)
	sort.Strings(cp)
	for i, s := range cp {
		if i > 0 && s == cp[i-1] {
			continue
		}
		writeFingerprintField(w, tag, s)
	}
}

func normalizedFingerprintPaths(paths []string) []string {
	candidates := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			candidates = append(candidates, filepath.Clean(path))
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i]) == len(candidates[j]) {
			return candidates[i] < candidates[j]
		}
		return len(candidates[i]) < len(candidates[j])
	})
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		redundant := false
		for _, parent := range result {
			relative, err := filepath.Rel(parent, candidate)
			if err == nil && !filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				redundant = true
				break
			}
		}
		if !redundant {
			result = append(result, candidate)
		}
	}
	return result
}

func effectiveFingerprintPaths(root string, paths []string) []string {
	root = filepath.Clean(root)
	var effective []string
	for _, candidate := range normalizedFingerprintPaths(paths) {
		// A subtree filter changes the scan whenever it intersects the scan
		// root. That includes both a path below root and an ancestor that
		// suppresses root itself. Scope owns alias normalization, including
		// missing suffixes below symlinked existing ancestors.
		if scope.PathsIntersect(root, candidate) {
			effective = append(effective, candidate)
		}
	}
	return effective
}

func writeFingerprintKeys(w io.Writer, tag string, m map[string]bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeFingerprintField(w, tag, k)
	}
}

// FromTree flattens root into a Snapshot tagged with the given fingerprint.
// Parent indexes and basenames are sufficient to rebuild every relative path;
// storing the full path per node would make caches disproportionately large on
// deep or million-entry trees.
func FromTree(root *tree.Node, fingerprint, rootFS string, files, dirs int, errs int64, complete bool, at time.Time) *Snapshot {
	snap := &Snapshot{
		Root:        root.Path(), // "" for the root; caller overrides with the absolute path
		Fingerprint: fingerprint,
		ScannedAt:   at,
		RootFS:      rootFS,
		Files:       files,
		Dirs:        dirs,
		Errors:      errs,
		Complete:    complete,
		Nodes:       make([]FlatNode, 0, 64),
	}
	type item struct {
		n         *tree.Node
		parentIdx int
	}
	queue := []item{{n: root, parentIdx: -1}}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		idx := len(snap.Nodes)
		snap.Nodes = append(snap.Nodes, flat(it.n, it.parentIdx))
		for _, c := range it.n.Children {
			queue = append(queue, item{n: c, parentIdx: idx})
		}
	}
	return snap
}

func flat(n *tree.Node, parent int) FlatNode {
	fn := FlatNode{
		Name: n.Name, IsDir: n.IsDir, Depth: n.Depth,
		Apparent: n.Apparent, Alloc: n.Alloc,
		FileCount: n.FileCount, DirCount: n.DirCount,
		ModTime: n.ModTime, Hardlink: n.Hardlink, Parent: parent,
	}
	if n.Err != nil {
		fn.ErrMsg = n.Err.Error()
	}
	return fn
}

// ToTree rebuilds a tree.Node tree from the snapshot. Parent links (and thus
// Path()) are restored by re-Adopting children, so the node's relative path is
// recoverable without storing it on every node.
func (s *Snapshot) ToTree() *tree.Node {
	if !s.validTreeLayout() {
		return nil
	}
	nodes := make([]*tree.Node, len(s.Nodes))
	for i, f := range s.Nodes {
		nodes[i] = &tree.Node{
			Name: f.Name, IsDir: f.IsDir, Depth: f.Depth,
			Apparent: f.Apparent, Alloc: f.Alloc,
			FileCount: f.FileCount, DirCount: f.DirCount,
			ModTime: f.ModTime, Hardlink: f.Hardlink,
			Err: cachedErr(f.ErrMsg),
		}
	}
	for i, f := range s.Nodes {
		if f.Parent < 0 {
			continue
		}
		nodes[f.Parent].Adopt(nodes[i])
	}
	return nodes[0]
}

// validTreeLayout verifies the parent-indexed tree before any indexes are used.
// FromTree emits breadth-first rows, so every non-root parent must precede its
// child. Rejecting malformed cache data here prevents cycles and index panics.
func (s *Snapshot) validTreeLayout() bool {
	if s == nil || s.Files < 0 || s.Dirs < 0 || s.Errors < 0 || len(s.Nodes) == 0 ||
		s.Nodes[0].Parent != -1 || s.Nodes[0].Depth != 0 {
		return false
	}
	root := s.Nodes[0]
	if root.Name != filepath.Base(s.Root) {
		return false
	}
	if root.Hardlink {
		return false
	}
	if root.IsDir {
		if s.Dirs < 1 || root.FileCount != s.Files || root.DirCount != s.Dirs-1 {
			return false
		}
	} else if len(s.Nodes) != 1 || s.Files != 1 || s.Dirs != 0 || root.FileCount != 0 || root.DirCount != 0 {
		return false
	}
	siblings := make(map[int]map[string]bool)
	fileNodes, dirNodes := 0, 0
	var errorNodes int64
	for i := range s.Nodes {
		node := s.Nodes[i]
		if node.Depth < 0 || node.Apparent < 0 || node.Alloc < 0 || node.FileCount < 0 || node.DirCount < 0 {
			return false
		}
		if i == 0 {
			if node.IsDir {
				dirNodes++
			} else {
				fileNodes++
			}
			if node.ErrMsg != "" {
				errorNodes++
			}
			if node.Hardlink && (node.IsDir || node.Apparent != 0 || node.Alloc != 0) {
				return false
			}
			continue
		}
		if node.Name == "" || node.Name == "." || node.Name == ".." || strings.ContainsRune(node.Name, '\x00') ||
			strings.ContainsAny(node.Name, `/\`) || filepath.IsAbs(node.Name) {
			return false
		}
		parent := node.Parent
		if parent < 0 || parent >= i || !s.Nodes[parent].IsDir || node.Depth != s.Nodes[parent].Depth+1 {
			return false
		}
		if siblings[parent] == nil {
			siblings[parent] = make(map[string]bool)
		}
		nameKey := node.Name
		if runtime.GOOS == windowsOS {
			nameKey = strings.ToLower(nameKey)
		}
		if siblings[parent][nameKey] {
			return false
		}
		siblings[parent][nameKey] = true
		if node.IsDir {
			dirNodes++
		} else {
			if node.ErrMsg == "" {
				fileNodes++
			}
		}
		if node.ErrMsg != "" {
			errorNodes++
		}
		if node.Hardlink && (node.IsDir || node.Apparent != 0 || node.Alloc != 0) {
			return false
		}
	}
	if dirNodes != s.Dirs || fileNodes != s.Files || errorNodes != s.Errors || (s.Complete && s.Errors != 0) {
		return false
	}
	childApparent := make([]int64, len(s.Nodes))
	childAllocated := make([]int64, len(s.Nodes))
	childFiles := make([]int, len(s.Nodes))
	childDirs := make([]int, len(s.Nodes))
	childNewest := make([]time.Time, len(s.Nodes))
	for i := len(s.Nodes) - 1; i >= 0; i-- {
		node := s.Nodes[i]
		if !node.IsDir {
			if node.FileCount != 0 || node.DirCount != 0 {
				return false
			}
		} else if node.Apparent != childApparent[i] || node.Alloc < childAllocated[i] ||
			node.FileCount != childFiles[i] || node.DirCount != childDirs[i] ||
			(!childNewest[i].IsZero() && node.ModTime.Before(childNewest[i])) {
			return false
		}
		parent := node.Parent
		if parent < 0 {
			continue
		}
		if childApparent[parent] > 1<<63-1-node.Apparent || childAllocated[parent] > 1<<63-1-node.Alloc {
			return false
		}
		childApparent[parent] += node.Apparent
		childAllocated[parent] += node.Alloc
		if node.ModTime.After(childNewest[parent]) {
			childNewest[parent] = node.ModTime
		}
		if node.IsDir {
			if node.FileCount > int(^uint(0)>>1)-childFiles[parent] || node.DirCount >= int(^uint(0)>>1)-childDirs[parent] {
				return false
			}
			childFiles[parent] += node.FileCount
			childDirs[parent] += node.DirCount + 1
		} else if node.ErrMsg == "" {
			if childFiles[parent] == int(^uint(0)>>1) {
				return false
			}
			childFiles[parent]++
		}
	}
	return true
}

func cachedErr(msg string) error {
	if msg == "" {
		return nil
	}
	return errors.New(msg)
}

// Marshal encodes the snapshot with its magic+version header.
func (s *Snapshot) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.Write(magicVersion); err != nil {
		return nil, err
	}
	if err := buf.WriteByte(formatVersion); err != nil {
		return nil, err
	}
	if err := gob.NewEncoder(&buf).Encode(s); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unmarshal decodes a snapshot, rejecting wrong magic/version/fingerprint.
func Unmarshal(data []byte, wantFingerprint string) (*Snapshot, error) {
	snap, current, err := Inspect(data)
	if err != nil || !current {
		return nil, ErrIncompatible
	}
	if strings.TrimSpace(wantFingerprint) != "" && snap.Fingerprint != wantFingerprint {
		return nil, ErrIncompatible
	}
	return snap, nil
}

// Inspect validates current and recognized legacy snapshot encodings for
// explicit migration. Legacy snapshots are never returned by Load/Unmarshal;
// callers may only inventory or invalidate them.
func Inspect(data []byte) (*Snapshot, bool, error) {
	r := bytes.NewReader(data)
	magic := make([]byte, len(magicVersion))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, false, ErrIncompatible
	}
	current := bytes.Equal(magic, magicVersion)
	legacy := bytes.Equal(magic, []byte{'d', 's', 't', 0x02})
	if !current && !legacy {
		return nil, false, ErrIncompatible
	}
	ver, err := r.ReadByte()
	if err != nil || (current && ver != formatVersion) || (legacy && ver != 1) {
		return nil, false, ErrIncompatible
	}
	var snap Snapshot
	decoder := gob.NewDecoder(r)
	if err := decoder.Decode(&snap); err != nil {
		return nil, false, ErrIncompatible
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, false, ErrIncompatible
	}
	if strings.TrimSpace(snap.Root) == "" || !filepath.IsAbs(snap.Root) || strings.TrimSpace(snap.Fingerprint) == "" ||
		snap.ScannedAt.IsZero() || !snap.validTreeLayout() {
		return nil, false, ErrIncompatible
	}
	return &snap, current, nil
}
