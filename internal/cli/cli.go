// Package cli wires the cobra command tree, flags, and the scan→aggregate→render
// pipeline. It is deliberately thin: it translates flags into a scope.Policy and
// render.TextOptions, drives a scan, and hands results to the renderers. All
// heavy lifting lives in the leaf packages.
package cli

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/phillipod/go-dirstat/internal/render"
	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

// Config holds every user-facing flag value. Flags are bound to it by name in
// the command constructors; it is the single source of truth for building a
// scan + render run.
type Config struct {
	Paths []string

	// Output shaping.
	Depth  int
	Limit  int
	Sort   string
	ByExt  bool
	Bytes  bool
	NoBar  bool
	NoCol  bool
	NoCt   bool
	Files  bool // include individual file rows (du -a); default dirs only
	Format string

	// Size mode.
	Apparent bool

	// Scope / filtering.
	CrossDevice   bool
	OneFileSystem bool
	Follow        bool
	NoVirtual     bool
	NoHidden      bool
	Exclude       []string
	ExcludePath   []string
	IncludePath   []string
	IncludeFS     []string
	ExcludeFS     []string
	MinSize       string
	MaxSize       string

	// Execution.
	Jobs    int
	NoCache bool
}

// maxJobs bounds the scanner's process-wide traversal and stat pools. Larger
// values offer no practical throughput benefit and can consume excessive
// memory on a wide tree.
const maxJobs = 4096

// newConfig returns Config with the built-in defaults. Cobra overrides fields
// via bound flags; this just seeds values cobra cannot express as a zero value.
func newConfig() *Config {
	return &Config{Sort: "size", Format: "text"}
}

// sizeMode resolves the requested size mode (default: on-disk, matching du).
func (c *Config) sizeMode() tree.SizeMode {
	if c.Apparent {
		return tree.SizeApparent
	}
	return tree.SizeOnDisk
}

// policy validates the flag set and builds the scope.Policy. Keeping validation
// here makes every execution mode reject bad input before it starts scanning.
func (c *Config) policy() (scope.Policy, error) {
	if err := c.validate(); err != nil {
		return scope.Policy{}, err
	}

	min, err := parseSize(c.MinSize)
	if err != nil {
		return scope.Policy{}, fmt.Errorf("invalid --min-size %q: %w", c.MinSize, err)
	}
	max, err := parseSize(c.MaxSize)
	if err != nil {
		return scope.Policy{}, fmt.Errorf("invalid --max-size %q: %w", c.MaxSize, err)
	}
	if strings.TrimSpace(c.MaxSize) != "" && max == 0 {
		return scope.Policy{}, errors.New("--max-size must be greater than zero; omit it for no upper bound")
	}
	if strings.TrimSpace(c.MaxSize) != "" && min > max {
		return scope.Policy{}, fmt.Errorf("--min-size (%s) must not exceed --max-size (%s)", c.MinSize, c.MaxSize)
	}

	return scope.New(
		scope.WithCrossDevice(c.CrossDevice),
		scope.WithFollowSymlinks(c.Follow),
		scope.WithExcludeVirtual(!c.NoVirtual),
		scope.WithExcludeGlobs(c.Exclude),
		scope.WithExcludePaths(c.ExcludePath),
		scope.WithIncludePaths(c.IncludePath),
		scope.WithFilesystems(c.IncludeFS, c.ExcludeFS),
		scope.WithHidden(!c.NoHidden),
		scope.WithSizeThreshold(min, max),
	), nil
}

func (c *Config) validate() error {
	for _, value := range []struct {
		name  string
		value int
	}{
		{name: "depth", value: c.Depth},
		{name: "limit", value: c.Limit},
		{name: "jobs", value: c.Jobs},
	} {
		if value.value < 0 {
			return fmt.Errorf("--%s must be zero or greater, got %d", value.name, value.value)
		}
	}
	if c.Jobs > maxJobs {
		return fmt.Errorf("--jobs must not exceed %d, got %d", maxJobs, c.Jobs)
	}
	if c.CrossDevice && c.OneFileSystem {
		return errors.New("--cross-device and --one-file-system cannot be used together")
	}
	if !validSortMode(c.Sort) {
		return fmt.Errorf("invalid --sort %q: expected size, size-asc, count, mtime, or name", c.Sort)
	}
	if c.Format != "text" && c.Format != "tsv" {
		return fmt.Errorf("invalid --format %q: expected text or tsv", c.Format)
	}
	if c.Format == "tsv" && c.ByExt {
		return errors.New("--format=tsv cannot be used with --by-ext or the extensions command")
	}
	if c.ByExt {
		switch {
		case c.Depth != 0:
			return errors.New("--depth cannot be used with an extension breakdown")
		case c.Limit != 0:
			return errors.New("--limit cannot be used with an extension breakdown")
		case c.Files:
			return errors.New("--files cannot be used with an extension breakdown")
		case c.Sort != "size" && c.Sort != "size-desc":
			return errors.New("--sort only supports size for an extension breakdown")
		}
	}

	for _, pattern := range c.Exclude {
		if _, err := filepath.Match(pattern, ""); err != nil {
			return fmt.Errorf("invalid --exclude glob %q: %w", pattern, err)
		}
	}
	for _, paths := range []struct {
		name   string
		values []string
	}{
		{name: "exclude-path", values: c.ExcludePath},
		{name: "include-path", values: c.IncludePath},
	} {
		for _, path := range paths.values {
			if !filepath.IsAbs(path) {
				return fmt.Errorf("--%s must be absolute, got %q", paths.name, path)
			}
		}
	}

	includeFS, err := validateFilesystemNames("include-fs", c.IncludeFS)
	if err != nil {
		return err
	}
	excludeFS, err := validateFilesystemNames("exclude-fs", c.ExcludeFS)
	if err != nil {
		return err
	}
	for name := range includeFS {
		if excludeFS[name] {
			return fmt.Errorf("filesystem type %q cannot appear in both --include-fs and --exclude-fs", name)
		}
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && (len(c.IncludeFS) > 0 || len(c.ExcludeFS) > 0) {
		return errors.New("--include-fs and --exclude-fs are currently supported only on Linux and macOS")
	}
	return nil
}

func validateFilesystemNames(flag string, values []string) (map[string]bool, error) {
	names := make(map[string]bool, len(values))
	for _, value := range values {
		name := strings.ToLower(strings.TrimSpace(value))
		if name == "" {
			return nil, fmt.Errorf("--%s must not be empty", flag)
		}
		names[name] = true
	}
	return names, nil
}

func validSortMode(mode string) bool {
	switch mode {
	case "size", "size-desc", "size-asc", "count", "count-desc", "mtime", "mtime-desc", "name", "name-asc":
		return true
	default:
		return false
	}
}

// textOptions builds render.TextOptions, auto-disabling color when stdout is
// not a terminal (so piping never emits ANSI).
func (c *Config) textOptions(out io.Writer) render.TextOptions {
	return render.TextOptions{
		Depth:    c.Depth,
		Limit:    c.Limit,
		Sort:     tree.ParseSort(c.Sort),
		Size:     c.sizeMode(),
		Bar:      !c.NoBar,
		BarWidth: 16,
		Color:    !c.NoCol && writerIsTTY(out),
		Counts:   !c.NoCt,
		Bytes:    c.Bytes,
		Files:    c.Files,
	}
}

// writerIsTTY reports whether the actual output destination is a terminal.
// Cobra callers may replace stdout with a buffer or pipe, so consulting the
// process-global os.Stdout would leak ANSI into redirected command output.
func writerIsTTY(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// parseSize parses a human size with an optional B/K/M/G/T/P/E suffix (binary).
// "10M" -> 10*1024*1024. An empty string is 0 (no bound).
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'b', 'B':
		s = s[:len(s)-1]
	case 'k', 'K':
		mult, s = 1024, s[:len(s)-1]
	case 'm', 'M':
		mult, s = 1<<20, s[:len(s)-1]
	case 'g', 'G':
		mult, s = 1<<30, s[:len(s)-1]
	case 't', 'T':
		mult, s = 1<<40, s[:len(s)-1]
	case 'p', 'P':
		mult, s = 1<<50, s[:len(s)-1]
	case 'e', 'E':
		mult, s = 1<<60, s[:len(s)-1]
	}
	if s == "" {
		return 0, errors.New("expected a non-negative integer with an optional B, K, M, G, T, P, or E suffix")
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("expected a non-negative integer with an optional B, K, M, G, T, P, or E suffix: %w", err)
	}
	if v < 0 {
		return 0, errors.New("size must not be negative")
	}
	if v > math.MaxInt64/mult {
		return 0, errors.New("size exceeds the maximum supported value")
	}
	return v * mult, nil
}
