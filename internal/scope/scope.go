// Package scope owns every "should this be counted / descended into?" decision
// for a scan. Keeping all filtering in one place lets the scanner stay a thin,
// fast traversal, and makes the policy independently testable. It is the only
// layer that knows about filesystem types, mount tables, and the special
// virtual directories that must never be walked.
package scope

import (
	"path/filepath"
	"runtime"
	"strings"
)

// Policy is an immutable bundle of filtering rules. The zero value is not safe
// — build one with New(), which fills in the safe defaults (do not cross
// filesystems; exclude /proc, /sys, /dev, /run and kernel pseudo-filesystems).
type Policy struct {
	// CrossDevice, when false (the default), forbids descending into a
	// directory on a different device than its parent — like `du -x`.
	CrossDevice bool

	// ExcludeVirtual drops the well-known virtual/pseudo directories even when
	// CrossDevice is true, and skips directories on kernel pseudo-filesystems.
	ExcludeVirtual bool

	// VirtualPaths are absolute path prefixes always excluded when
	// ExcludeVirtual is set. Populated by New().
	VirtualPaths []string

	// FollowSymlinks makes the scan chase symlinked directories (with loop
	// protection). Off by default, matching `du`.
	FollowSymlinks bool

	// IncludePaths, when non-empty, restricts the scan to entries under one of
	// these absolute path prefixes (a whitelist). Empty means no restriction.
	IncludePaths []string

	// ExcludePaths are absolute path prefixes to drop from the scan entirely.
	ExcludePaths []string

	// ExcludeGlobs are basename (or relative-path) glob patterns to drop,
	// du --exclude style: "*.o" matches that name at any depth.
	ExcludeGlobs []string

	// IncludeFS / ExcludeFS filter by filesystem type (ext4, tmpfs, ...).
	// An empty IncludeFS means "any type" subject to ExcludeFS.
	IncludeFS map[string]bool
	ExcludeFS map[string]bool

	// IncludeHidden, when false, skips dotfile entries.
	IncludeHidden bool

	// MinSize / MaxSize filter individual FILES by their measured size in
	// bytes; 0 means "no bound" on that side. Directories are never filtered
	// by size — they always aggregate.
	MinSize int64
	MaxSize int64

	mounts *mountTable // nil on platforms without a readable mount table
}

// Option configures a Policy. Use the With* helpers.
type Option func(*Policy)

// New returns a Policy with safe defaults applied, then layered with opts.
func New(opts ...Option) Policy {
	p := Policy{
		ExcludeVirtual: true,
		IncludeHidden:  true,
		VirtualPaths: []string{
			"/proc", "/sys", "/dev", "/run",
			"/var/run", "/var/lock",
		},
	}
	for _, o := range opts {
		o(&p)
	}
	p.mounts = loadMounts() // platform-specific; nil where unavailable
	return p
}

// WithCrossDevice enables crossing filesystem boundaries.
func WithCrossDevice(b bool) Option { return func(p *Policy) { p.CrossDevice = b } }

// WithFollowSymlinks enables following symlinked directories.
func WithFollowSymlinks(b bool) Option { return func(p *Policy) { p.FollowSymlinks = b } }

// WithExcludeVirtual toggles the virtual/pseudo-filesystem denylist.
func WithExcludeVirtual(b bool) Option { return func(p *Policy) { p.ExcludeVirtual = b } }

// WithIncludePaths sets the path-prefix whitelist.
func WithIncludePaths(ps []string) Option { return func(p *Policy) { p.IncludePaths = cleanPaths(ps) } }

// WithExcludePaths sets path-prefix exclusions (added to the virtual set).
func WithExcludePaths(ps []string) Option { return func(p *Policy) { p.ExcludePaths = cleanPaths(ps) } }

// WithExcludeGlobs sets basename/relative-path glob exclusions.
func WithExcludeGlobs(gs []string) Option { return func(p *Policy) { p.ExcludeGlobs = gs } }

// WithFilesystems sets the fstype include/exclude lists.
func WithFilesystems(include, exclude []string) Option {
	return func(p *Policy) {
		p.IncludeFS = toSet(include)
		p.ExcludeFS = toSet(exclude)
	}
}

// WithHidden toggles dotfile inclusion.
func WithHidden(b bool) Option { return func(p *Policy) { p.IncludeHidden = b } }

// WithSizeThreshold sets the per-file size window (0 = unbounded on that side).
func WithSizeThreshold(min, max int64) Option {
	return func(p *Policy) { p.MinSize = min; p.MaxSize = max }
}

// RootFS resolves the filesystem type of the scan root via the mount table.
// Returns "" if unknown (e.g. no mount table on this platform).
func (p *Policy) RootFS(rootAbs string) string {
	return p.FSOf(rootAbs)
}

// FSOf resolves the filesystem type of an arbitrary absolute path by
// longest-prefix match against the mount table. "" if unknown.
func (p Policy) FSOf(abs string) string {
	if p.mounts == nil {
		return ""
	}
	return p.mounts.fstype(abs)
}

// AllowsFilesystem reports whether a resolved filesystem type passes the
// policy's filesystem rules. An empty fstype is allowed unless IncludeFS is
// non-empty, in which case an unknown filesystem cannot satisfy the whitelist.
// Kernel pseudo-filesystems are also rejected while ExcludeVirtual is enabled.
func (p *Policy) AllowsFilesystem(fstype string) bool {
	if p.ExcludeVirtual && isPseudoFSType(fstype) {
		return false
	}
	if len(p.IncludeFS) > 0 && !p.IncludeFS[fstype] {
		return false
	}
	return !p.ExcludeFS[fstype]
}

// AllowsTarget reports whether a resolved child target may be measured.
//
//   - parentDev/childDev drive the cross-device check,
//   - childFS (resolved by the caller via the mount table) drives fstype filters,
//   - abs is checked against the virtual, explicit-exclude, and include rules.
//
// The scanner applies this to both directories and files. That matters when a
// followed symlink or file bind mount resolves somewhere outside the alias's
// device, filesystem, or path scope.
func (p *Policy) AllowsTarget(abs string, childDev, parentDev uint64, childFS string) bool {
	if !p.CrossDevice && childDev != parentDev {
		return false
	}
	if !p.AllowsFilesystem(childFS) {
		return false
	}
	return p.AllowsPath(abs)
}

// Descend reports whether the scan may enter a child directory.
func (p *Policy) Descend(abs string, childDev, parentDev uint64, childFS string) bool {
	return p.AllowsTarget(abs, childDev, parentDev, childFS)
}

// AllowsPath reports whether abs passes the virtual, explicit-exclude, and
// include-prefix rules. Include paths also admit their ancestors: a scan rooted
// at / may traverse /srv in order to reach an included /srv/data subtree, while
// unrelated siblings remain excluded.
func (p *Policy) AllowsPath(abs string) bool {
	return !p.RejectsPath(abs) && p.includedPath(abs)
}

// RejectsPath reports whether abs is explicitly denied or belongs to the
// default virtual-path denylist. The scanner also applies this to resolved
// symlink targets so an alias cannot bypass an exclusion.
func (p *Policy) RejectsPath(abs string) bool {
	return (p.ExcludeVirtual && p.isVirtual(abs)) || p.excludedPath(abs)
}

// Entry reports whether a non-directory entry (or a dir being counted) named
// name, at relative path rel and absolute path abs, should be included.
func (p *Policy) Entry(rel, name, abs string) bool {
	if !p.IncludeHidden && isHidden(name) {
		return false
	}
	if !p.AllowsPath(abs) {
		return false
	}
	for _, g := range p.ExcludeGlobs {
		if matchGlob(g, name) || matchGlob(g, rel) {
			return false
		}
	}
	return true
}

// File reports whether a regular file of the given size passes the size window.
func (p *Policy) File(size int64) bool {
	if p.MinSize > 0 && size < p.MinSize {
		return false
	}
	if p.MaxSize > 0 && size > p.MaxSize {
		return false
	}
	return true
}

// isVirtual matches the absolute path against the virtual denylist.
func (p *Policy) isVirtual(abs string) bool {
	for _, vp := range p.VirtualPaths {
		if pathWithin(vp, abs) {
			return true
		}
	}
	return false
}

func (p *Policy) excludedPath(abs string) bool {
	for _, ep := range p.ExcludePaths {
		if pathWithin(ep, abs) {
			return true
		}
	}
	return false
}

func (p *Policy) includedPath(abs string) bool {
	if len(p.IncludePaths) == 0 {
		return true
	}
	for _, ip := range p.IncludePaths {
		// Keep include paths lexical. A symlink used as the include root must
		// not silently turn its resolved target into an additional whitelist;
		// the scanner separately checks the resolved target before following it.
		if pathWithinClean(filepath.Clean(ip), filepath.Clean(abs)) || pathWithinClean(filepath.Clean(abs), filepath.Clean(ip)) {
			return true
		}
	}
	return false
}

// pathWithin reports whether child is parent itself or is below it at a real
// path-component boundary. filepath.Rel handles root paths, platform-specific
// separators, and misleading lexical prefixes such as /tmp/data-old.
func pathWithin(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if pathWithinClean(parent, child) {
		return true
	}
	// Most entries match lexically. Only pay for filesystem alias resolution
	// when that fast path fails (for example /var vs /private/var on macOS).
	return pathWithinClean(canonicalComparePath(parent), canonicalComparePath(child))
}

func pathWithinClean(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// canonicalComparePath normalizes aliases before applying path policies.
// macOS commonly exposes /var as a symlink to /private/var; Windows may also
// return short/long path spellings for the same target. Resolution is best
// effort so policies still work for synthetic or not-yet-created paths.
func canonicalComparePath(path string) string {
	path = filepath.Clean(path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	// EvalSymlinks requires the complete path to exist. Walk up to the deepest
	// existing ancestor, resolve that prefix, and append any missing suffix so
	// exclusions also protect paths that have not been created yet.
	var suffix []string
	probe := path
	for {
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			path = resolved
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

func isHidden(name string) bool {
	return len(name) > 1 && name[0] == '.' && name != ".."
}

// matchGlob wraps filepath.Match, treating a malformed pattern as non-matching
// rather than erroring — a bad glob should never crash a scan.
func matchGlob(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	ok, err := filepath.Match(pattern, name)
	return err == nil && ok
}

func cleanPaths(ps []string) []string {
	out := make([]string, 0, len(ps))
	for _, s := range ps {
		if s == "" {
			continue
		}
		out = append(out, filepath.Clean(s))
	}
	return out
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" {
			m[x] = true
		}
	}
	return m
}
