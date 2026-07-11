// Package diagnose gathers bounded, read-only evidence that helps explain
// disk pressure. Platform-specific probes report their availability explicitly
// instead of silently returning an empty result.
package diagnose

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
)

// Capability describes whether a diagnostic probe is usable on this host.
type Capability struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// OpenDeletedFile is a regular file that has been unlinked but is still held
// open by a process. Its bytes are normally reclaimable only after that file
// descriptor is closed.
type OpenDeletedFile struct {
	PID        int    `json:"pid"`
	Process    string `json:"process,omitempty"`
	Descriptor string `json:"descriptor"`
	Path       string `json:"path"`
	Size       int64  `json:"size_bytes"`
}

// Result contains portable volume status and all available platform evidence.
// Probe failures that do not invalidate the whole result are returned as
// warnings so permission-restricted /proc trees remain useful.
type Result struct {
	Capabilities []Capability      `json:"capabilities"`
	Volumes      []fsinfo.Volume   `json:"volumes,omitempty"`
	OpenDeleted  []OpenDeletedFile `json:"open_deleted,omitempty"`
	Warnings     []string          `json:"warnings,omitempty"`
}

// Gather inspects the volumes containing paths and runs bounded platform
// diagnostics. Empty paths still run host diagnostics, but omit volume status.
func Gather(ctx context.Context, paths []string) Result {
	result := Result{}
	seen := make(map[string]bool)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			result.Warnings = append(result.Warnings, err.Error())
			break
		}
		abs, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("volume %q: %v", path, err))
			continue
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		volume, err := fsinfo.VolumeFor(abs)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("volume %q: %v", path, err))
			continue
		}
		result.Volumes = append(result.Volumes, volume)
	}

	capability, files, warnings := gatherOpenDeleted(ctx, paths)
	result.Capabilities = append(result.Capabilities, capability)
	result.OpenDeleted = files
	result.Warnings = append(result.Warnings, warnings...)
	return result
}
