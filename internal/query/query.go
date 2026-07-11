// Package query turns a measured tree into flat, filterable records suitable
// for interactive candidate lists and machine-readable output.
package query

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/phillipod/go-dirstat/internal/fileclass"
	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/tree"
)

// Kind is the measured kind of a candidate.
type Kind string

const (
	KindFile      Kind = "file"
	KindDirectory Kind = "directory"
)

// Record is one flattened node. Metadata fields are populated only when
// Options.Metadata is true or an ownership filter requires inspection.
type Record struct {
	Path      string    `json:"path"`
	Relative  string    `json:"relative_path"`
	Name      string    `json:"name"`
	Extension string    `json:"extension,omitempty"`
	Kind      Kind      `json:"kind"`
	Apparent  int64     `json:"apparent_bytes"`
	Allocated int64     `json:"allocated_bytes"`
	FileCount int       `json:"file_count"`
	DirCount  int       `json:"directory_count"`
	ModTime   time.Time `json:"modified_at,omitempty"`
	Hardlink  bool      `json:"hardlink,omitempty"`
	ScanError string    `json:"scan_error,omitempty"`

	UID      string          `json:"uid,omitempty"`
	GID      string          `json:"gid,omitempty"`
	Owner    string          `json:"owner,omitempty"`
	Group    string          `json:"group,omitempty"`
	Mode     uint32          `json:"mode,omitempty"`
	ModeText string          `json:"mode_text,omitempty"`
	Links    uint64          `json:"links,omitempty"`
	Identity fsinfo.Identity `json:"identity,omitempty"`

	// MetadataError records an inspection failure without discarding a useful
	// measured candidate. Ownership-filtered records fail closed instead.
	MetadataError string `json:"metadata_error,omitempty"`
}

// Filter selects candidates. Size limits use SizeMode (apparent by default).
// OlderThan means age >= duration; NewerThan means age <= duration. Extensions
// may be supplied with or without a leading dot and match case-insensitively.
type Filter struct {
	MinSize    *int64
	MaxSize    *int64
	SizeMode   tree.SizeMode
	OlderThan  time.Duration
	NewerThan  time.Duration
	Owners     []string
	Groups     []string
	Extensions []string
	Kinds      []Kind
	PathGlob   string
	PathRegexp string
}

// SortField names a sortable record value.
type SortField string

const (
	SortPath      SortField = "path"
	SortName      SortField = "name"
	SortApparent  SortField = "apparent"
	SortAllocated SortField = "allocated"
	SortFiles     SortField = "files"
	SortDirs      SortField = "directories"
	SortMTime     SortField = "mtime"
	SortOwner     SortField = "owner"
	SortGroup     SortField = "group"
	SortExtension SortField = "extension"
	SortKind      SortField = "kind"
)

// SortKey describes one ordering key. Keys are applied in slice order.
type SortKey struct {
	Field SortField
	Desc  bool
}

// Inspector is compatible with fsinfo.Inspect and exists to make metadata
// loading replaceable in callers and tests.
type Inspector func(path string, follow bool) (fsinfo.Entry, error)

// Options controls record construction.
type Options struct {
	Filter Filter
	Sort   []SortKey
	// Limit bounds retained sorted records. Zero is unlimited.
	Limit          int
	Metadata       bool
	FollowMetadata bool
	Now            time.Time
	Inspect        Inspector
}

// Build flattens measuredRoot into filtered records rooted at scanRoot.
// Relative paths always use forward slashes; absolute paths use the host's
// native form. The root record has an empty relative path.
func Build(measuredRoot *tree.Node, scanRoot string, opts Options) ([]Record, error) {
	return BuildContext(context.Background(), measuredRoot, scanRoot, opts)
}

// BuildContext is Build with cancellation checked throughout tree traversal
// and before and after potentially slow metadata inspection.
func BuildContext(ctx context.Context, measuredRoot *tree.Node, scanRoot string, opts Options) ([]Record, error) {
	if measuredRoot == nil {
		return nil, errors.New("query: nil measured root")
	}
	if opts.Limit < 0 {
		return nil, errors.New("query: limit cannot be negative")
	}
	if err := validateSort(opts.Sort); err != nil {
		return nil, err
	}
	keys := effectiveSortKeys(opts.Sort)
	capacity := measuredRoot.FileCount + measuredRoot.DirCount + 1
	if capacity < 0 {
		capacity = 0
	}
	if opts.Limit > 0 && opts.Limit < capacity {
		capacity = opts.Limit
	}
	if opts.Limit == 0 {
		records := make([]Record, 0, capacity)
		err := walkMatchingRecords(ctx, measuredRoot, scanRoot, opts, func(record Record) error {
			records = append(records, record)
			return nil
		})
		if err != nil {
			return nil, err
		}
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		sortRecords(records, keys)
		return records, nil
	}

	top := &topRecordHeap{keys: keys, items: make([]rankedRecord, 0, capacity)}
	order := 0
	err := walkMatchingRecords(ctx, measuredRoot, scanRoot, opts, func(record Record) error {
		candidate := rankedRecord{record: record, order: order}
		order++
		if top.Len() < opts.Limit {
			heap.Push(top, candidate)
		} else if rankedRecordBefore(candidate, top.items[0], keys) {
			top.items[0] = candidate
			heap.Fix(top, 0)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	sort.Slice(top.items, func(i, j int) bool {
		return rankedRecordBefore(top.items[i], top.items[j], keys)
	})
	records := make([]Record, len(top.items))
	for i := range top.items {
		records[i] = top.items[i].record
	}
	return records, nil
}

type rankedRecord struct {
	record Record
	order  int
}

type topRecordHeap struct {
	items []rankedRecord
	keys  []SortKey
}

func (h topRecordHeap) Len() int { return len(h.items) }

func (h topRecordHeap) Less(i, j int) bool {
	return rankedRecordBefore(h.items[j], h.items[i], h.keys)
}

func (h topRecordHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *topRecordHeap) Push(value any) {
	h.items = append(h.items, value.(rankedRecord))
}

func (h *topRecordHeap) Pop() any {
	last := len(h.items) - 1
	value := h.items[last]
	h.items[last] = rankedRecord{}
	h.items = h.items[:last]
	return value
}

func rankedRecordBefore(a, b rankedRecord, keys []SortKey) bool {
	if recordBefore(a.record, b.record, keys) {
		return true
	}
	if recordBefore(b.record, a.record, keys) {
		return false
	}
	return a.order < b.order
}

var errStreamLimit = errors.New("query: stream limit reached")

// Stream visits matching records in deterministic tree order without sorting
// or retaining a result slice. Limit zero is unlimited. Callers use this for
// low-memory JSONL, TSV, or NUL pipelines where ordering is not required.
func Stream(measuredRoot *tree.Node, scanRoot string, opts Options, visit func(Record) error) error {
	return StreamContext(context.Background(), measuredRoot, scanRoot, opts, visit)
}

// StreamContext is Stream with cancellation checked throughout traversal and
// metadata inspection.
func StreamContext(ctx context.Context, measuredRoot *tree.Node, scanRoot string, opts Options, visit func(Record) error) error {
	if opts.Limit < 0 {
		return errors.New("query: limit cannot be negative")
	}
	if len(opts.Sort) != 0 {
		return errors.New("query: streaming does not accept sort keys")
	}
	if visit == nil {
		return errors.New("query: stream visitor is required")
	}
	count := 0
	err := walkMatchingRecords(ctx, measuredRoot, scanRoot, opts, func(record Record) error {
		if err := visit(record); err != nil {
			return err
		}
		count++
		if opts.Limit > 0 && count >= opts.Limit {
			return errStreamLimit
		}
		return nil
	})
	if errors.Is(err, errStreamLimit) {
		return nil
	}
	return err
}

func walkMatchingRecords(ctx context.Context, measuredRoot *tree.Node, scanRoot string, opts Options, visit func(Record) error) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if measuredRoot == nil {
		return errors.New("query: nil measured root")
	}
	root, err := filepath.Abs(filepath.Clean(scanRoot))
	if err != nil {
		return fmt.Errorf("query: absolute root: %w", err)
	}
	compiled, err := compileFilter(opts.Filter)
	if err != nil {
		return err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	inspect := opts.Inspect
	if inspect == nil {
		inspect = fsinfo.Inspect
	}
	needMetadata := opts.Metadata || len(compiled.owners) > 0 || len(compiled.groups) > 0

	var walk func(*tree.Node) error
	walk = func(n *tree.Node) error {
		if err := contextError(ctx); err != nil {
			return err
		}
		rel := filepath.ToSlash(n.Path())
		absolute := root
		if rel != "" {
			absolute = filepath.Join(root, filepath.FromSlash(rel))
			inside, err := filepath.Rel(root, absolute)
			if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
				return fmt.Errorf("query: candidate %q escapes scan root", rel)
			}
		}
		r := recordFromNode(n, absolute, rel)
		matched := compiled.cheapMatch(r, now)
		if matched && needMetadata {
			if err := contextError(ctx); err != nil {
				return err
			}
			entry, err := inspect(absolute, opts.FollowMetadata)
			if contextErr := contextError(ctx); contextErr != nil {
				return contextErr
			}
			if err != nil {
				r.MetadataError = err.Error()
				if len(compiled.owners) > 0 || len(compiled.groups) > 0 {
					matched = false
				}
			} else {
				copyMetadata(&r, entry)
			}
		}
		if matched && compiled.metadataMatch(r) {
			if err := contextError(ctx); err != nil {
				return err
			}
			if err := visit(r); err != nil {
				return err
			}
		}
		for _, child := range n.Children {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(measuredRoot)
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func recordFromNode(n *tree.Node, absolute, relative string) Record {
	kind := KindFile
	if n.IsDir {
		kind = KindDirectory
	}
	r := Record{
		Path: absolute, Relative: relative, Name: n.Name, Kind: kind,
		Apparent: n.Apparent, Allocated: n.Alloc,
		FileCount: n.FileCount, DirCount: n.DirCount,
		ModTime: n.ModTime, Hardlink: n.Hardlink,
	}
	if !n.IsDir {
		r.Extension = fileclass.Extension(n.Name)
	}
	if n.Err != nil {
		r.ScanError = n.Err.Error()
	}
	return r
}

func copyMetadata(r *Record, e fsinfo.Entry) {
	r.UID, r.GID, r.Owner, r.Group = e.UID, e.GID, e.Owner, e.Group
	r.Mode, r.ModeText, r.Links, r.Identity = e.Mode, e.ModeText, e.Links, e.Identity
}

type compiledFilter struct {
	raw        Filter
	owners     map[string]bool
	groups     map[string]bool
	exts       map[string]bool
	kinds      map[Kind]bool
	pathRegexp *regexp.Regexp
}

func compileFilter(f Filter) (compiledFilter, error) {
	c := compiledFilter{raw: f, owners: stringSet(f.Owners), groups: stringSet(f.Groups), kinds: make(map[Kind]bool)}
	if f.MinSize != nil && *f.MinSize < 0 || f.MaxSize != nil && *f.MaxSize < 0 {
		return c, errors.New("query: size limits cannot be negative")
	}
	if f.SizeMode != tree.SizeApparent && f.SizeMode != tree.SizeOnDisk {
		return c, fmt.Errorf("query: unsupported size mode %d", f.SizeMode)
	}
	if f.MinSize != nil && f.MaxSize != nil && *f.MinSize > *f.MaxSize {
		return c, errors.New("query: minimum size exceeds maximum size")
	}
	if f.OlderThan < 0 || f.NewerThan < 0 {
		return c, errors.New("query: age durations cannot be negative")
	}
	for _, ext := range f.Extensions {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		if c.exts == nil {
			c.exts = make(map[string]bool)
		}
		c.exts[ext] = true
	}
	for _, kind := range f.Kinds {
		if kind != KindFile && kind != KindDirectory {
			return c, fmt.Errorf("query: unsupported kind %q", kind)
		}
		c.kinds[kind] = true
	}
	if f.PathGlob != "" {
		if _, err := path.Match(filepath.ToSlash(f.PathGlob), "probe"); err != nil {
			return c, fmt.Errorf("query: invalid path glob: %w", err)
		}
	}
	if f.PathRegexp != "" {
		re, err := regexp.Compile(f.PathRegexp)
		if err != nil {
			return c, fmt.Errorf("query: invalid path regexp: %w", err)
		}
		c.pathRegexp = re
	}
	return c, nil
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result[value] = true
		}
	}
	return result
}

func (c compiledFilter) cheapMatch(r Record, now time.Time) bool {
	size := r.Apparent
	if c.raw.SizeMode == tree.SizeOnDisk {
		size = r.Allocated
	}
	if c.raw.MinSize != nil && size < *c.raw.MinSize || c.raw.MaxSize != nil && size > *c.raw.MaxSize {
		return false
	}
	if c.raw.OlderThan > 0 && (r.ModTime.IsZero() || r.ModTime.After(now.Add(-c.raw.OlderThan))) {
		return false
	}
	if c.raw.NewerThan > 0 && (r.ModTime.IsZero() || r.ModTime.Before(now.Add(-c.raw.NewerThan))) {
		return false
	}
	if len(c.exts) > 0 && !c.exts[r.Extension] {
		return false
	}
	if len(c.kinds) > 0 && !c.kinds[r.Kind] {
		return false
	}
	if c.raw.PathGlob != "" {
		matched, _ := path.Match(filepath.ToSlash(c.raw.PathGlob), r.Relative)
		if !matched {
			return false
		}
	}
	return c.pathRegexp == nil || c.pathRegexp.MatchString(r.Relative)
}

func (c compiledFilter) metadataMatch(r Record) bool {
	if len(c.owners) > 0 && !c.owners[r.Owner] && !c.owners[r.UID] {
		return false
	}
	return len(c.groups) == 0 || c.groups[r.Group] || c.groups[r.GID]
}

func validateSort(keys []SortKey) error {
	valid := map[SortField]bool{
		SortPath: true, SortName: true, SortApparent: true, SortAllocated: true,
		SortFiles: true, SortDirs: true, SortMTime: true, SortOwner: true,
		SortGroup: true, SortExtension: true, SortKind: true,
	}
	for _, key := range keys {
		if !valid[key.Field] {
			return fmt.Errorf("query: unsupported sort field %q", key.Field)
		}
	}
	return nil
}

func sortRecords(records []Record, keys []SortKey) {
	keys = effectiveSortKeys(keys)
	// Stable sorts from least to most significant preserve caller key priority.
	for i := len(keys) - 1; i >= 0; i-- {
		key := keys[i]
		sort.SliceStable(records, func(a, b int) bool {
			cmp := compare(records[a], records[b], key.Field)
			if key.Desc {
				return cmp > 0
			}
			return cmp < 0
		})
	}
}

func effectiveSortKeys(keys []SortKey) []SortKey {
	if len(keys) == 0 {
		return []SortKey{{Field: SortPath}}
	}
	return keys
}

func recordBefore(a, b Record, keys []SortKey) bool {
	for _, key := range effectiveSortKeys(keys) {
		comparison := compare(a, b, key.Field)
		if comparison == 0 {
			continue
		}
		if key.Desc {
			return comparison > 0
		}
		return comparison < 0
	}
	return false
}

func compare(a, b Record, field SortField) int {
	switch field {
	case SortPath:
		return strings.Compare(a.Relative, b.Relative)
	case SortApparent:
		return compareInt64(a.Apparent, b.Apparent)
	case SortAllocated:
		return compareInt64(a.Allocated, b.Allocated)
	case SortFiles:
		return compareInt64(int64(a.FileCount), int64(b.FileCount))
	case SortDirs:
		return compareInt64(int64(a.DirCount), int64(b.DirCount))
	case SortMTime:
		return a.ModTime.Compare(b.ModTime)
	case SortName:
		return strings.Compare(a.Name, b.Name)
	case SortOwner:
		return strings.Compare(a.Owner, b.Owner)
	case SortGroup:
		return strings.Compare(a.Group, b.Group)
	case SortExtension:
		return strings.Compare(a.Extension, b.Extension)
	case SortKind:
		return strings.Compare(string(a.Kind), string(b.Kind))
	default:
		return 0
	}
}

func compareInt64(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
