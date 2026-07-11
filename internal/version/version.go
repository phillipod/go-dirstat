// Package version holds build-time metadata injected via -ldflags.
package version

// Build-time variables, overridden by the linker (see Makefile LDFLAGS).
var (
	// Version is the human-readable release identifier (git describe).
	Version = "dev"
	// Commit is the short VCS hash the binary was built from.
	Commit = "none"
	// BuildDate is the UTC build timestamp in RFC3339-ish form.
	BuildDate = "unknown"
)

// Info returns a single-line build identifier suitable for --version output.
func Info() string {
	return Version + " (" + Commit + ") built " + BuildDate
}
