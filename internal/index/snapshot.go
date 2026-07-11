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
	"sort"
	"strings"
	"time"

	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

// magicVersion guards against reading foreign or incompatible cache files. If
// the on-disk layout ever changes, bump formatVersion to invalidate old caches.
var magicVersion = []byte{'d', 's', 't', 0x02}

// formatVersion is encoded after the magic bytes.
const formatVersion byte = 1

// ErrIncompatible is returned when a cache file is the wrong format or was
// written under a different scope fingerprint.
var ErrIncompatible = errors.New("cache: incompatible snapshot")

// Snapshot is the on-disk representation of one scan.
type Snapshot struct {
	Root        string // absolute path that was scanned
	Fingerprint string // scope fingerprint this snapshot is valid for
	ScannedAt   time.Time
	RootFS      string
	Files       int
	Dirs        int
	Errors      int64
	Nodes       []FlatNode
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

// Fingerprint returns a short, stable hash of the root path and every scope
// option that affects scan results. Two scans with the same fingerprint produce
// interchangeable trees, so a cached snapshot can be reused.
func Fingerprint(root string, p scope.Policy) string {
	h := sha256.New()
	// hash.Hash writes are specified not to fail; make that explicit while
	// retaining the readable line-delimited fingerprint layout.
	_, _ = fmt.Fprintln(h, root)
	_, _ = fmt.Fprintln(h, p.CrossDevice, p.FollowSymlinks, p.ExcludeVirtual, p.IncludeHidden)
	_, _ = fmt.Fprintln(h, p.MinSize, p.MaxSize)
	writeSorted(h, "g", p.ExcludeGlobs)
	writeSorted(h, "ep", p.ExcludePaths)
	writeSorted(h, "ip", p.IncludePaths)
	writeSorted(h, "vp", p.VirtualPaths)
	writeKeys(h, "ifs", p.IncludeFS)
	writeKeys(h, "efs", p.ExcludeFS)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func writeSorted(w io.Writer, tag string, xs []string) {
	cp := append([]string(nil), xs...)
	sort.Strings(cp)
	for _, s := range cp {
		_, _ = fmt.Fprintln(w, tag, s)
	}
}

func writeKeys(w io.Writer, tag string, m map[string]bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintln(w, tag, k)
	}
}

// FromTree flattens root into a Snapshot tagged with the given fingerprint.
// Parent indexes and basenames are sufficient to rebuild every relative path;
// storing the full path per node would make caches disproportionately large on
// deep or million-entry trees.
func FromTree(root *tree.Node, fingerprint, rootFS string, files, dirs int, errs int64, at time.Time) *Snapshot {
	snap := &Snapshot{
		Root:        root.Path(), // "" for the root; caller overrides with the absolute path
		Fingerprint: fingerprint,
		ScannedAt:   at,
		RootFS:      rootFS,
		Files:       files,
		Dirs:        dirs,
		Errors:      errs,
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
	for i := range s.Nodes {
		node := s.Nodes[i]
		if node.Depth < 0 || node.Apparent < 0 || node.Alloc < 0 || node.FileCount < 0 || node.DirCount < 0 {
			return false
		}
		if i == 0 {
			continue
		}
		parent := node.Parent
		if parent < 0 || parent >= i || !s.Nodes[parent].IsDir || node.Depth != s.Nodes[parent].Depth+1 {
			return false
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
	r := bytes.NewReader(data)
	magic := make([]byte, len(magicVersion))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, ErrIncompatible
	}
	if !bytes.Equal(magic, magicVersion) {
		return nil, ErrIncompatible
	}
	ver, err := r.ReadByte()
	if err != nil || ver != formatVersion {
		return nil, ErrIncompatible
	}
	var snap Snapshot
	if err := gob.NewDecoder(r).Decode(&snap); err != nil {
		return nil, ErrIncompatible
	}
	if strings.TrimSpace(wantFingerprint) != "" && snap.Fingerprint != wantFingerprint {
		return nil, ErrIncompatible
	}
	if !snap.validTreeLayout() {
		return nil, ErrIncompatible
	}
	return &snap, nil
}
