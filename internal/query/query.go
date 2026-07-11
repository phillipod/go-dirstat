// Package query turns a measured tree into flat, filterable records suitable
// for interactive candidate lists and machine-readable output.
package query

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

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
	Filter         Filter
	Sort           []SortKey
	Metadata       bool
	FollowMetadata bool
	Now            time.Time
	Inspect        Inspector
}

// Build flattens measuredRoot into filtered records rooted at scanRoot.
// Relative paths always use forward slashes; absolute paths use the host's
// native form. The root record has an empty relative path.
func Build(measuredRoot *tree.Node, scanRoot string, opts Options) ([]Record, error) {
	if measuredRoot == nil {
		return nil, errors.New("query: nil measured root")
	}
	root, err := filepath.Abs(filepath.Clean(scanRoot))
	if err != nil {
		return nil, fmt.Errorf("query: absolute root: %w", err)
	}
	compiled, err := compileFilter(opts.Filter)
	if err != nil {
		return nil, err
	}
	if err := validateSort(opts.Sort); err != nil {
		return nil, err
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

	records := make([]Record, 0, measuredRoot.FileCount+measuredRoot.DirCount+1)
	var buildErr error
	measuredRoot.Walk(func(n *tree.Node) bool {
		rel := filepath.ToSlash(n.Path())
		absolute := root
		if rel != "" {
			absolute = filepath.Join(root, filepath.FromSlash(rel))
			inside, err := filepath.Rel(root, absolute)
			if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
				buildErr = fmt.Errorf("query: candidate %q escapes scan root", rel)
				return false
			}
		}
		r := recordFromNode(n, absolute, rel)
		if !compiled.cheapMatch(r, now) {
			return true
		}
		if needMetadata {
			entry, err := inspect(absolute, opts.FollowMetadata)
			if err != nil {
				r.MetadataError = err.Error()
				if len(compiled.owners) > 0 || len(compiled.groups) > 0 {
					return true
				}
			} else {
				copyMetadata(&r, entry)
			}
		}
		if compiled.metadataMatch(r) {
			records = append(records, r)
		}
		return true
	})
	if buildErr != nil {
		return nil, buildErr
	}
	sortRecords(records, opts.Sort)
	return records, nil
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
		r.Extension = strings.ToLower(filepath.Ext(n.Name))
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
	if len(keys) == 0 {
		keys = []SortKey{{Field: SortPath}}
	}
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
