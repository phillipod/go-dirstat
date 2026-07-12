//go:build !windows

package scan

import "path/filepath"

// resolvedAliasPath returns the canonical target path for a followed alias.
// Unix filepath.EvalSymlinks resolves the final symlink component, unlike the
// Windows mount-point junction behavior handled by the Windows implementation.
func resolvedAliasPath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
