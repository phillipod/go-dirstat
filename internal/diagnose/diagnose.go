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

// SchemaVersion identifies the machine-readable diagnostics contract. Version 2
// groups open descriptors by their underlying deleted filesystem object instead
// of emitting one row per descriptor.
const SchemaVersion = 2

// Capability describes whether a diagnostic probe is usable on this host.
type Capability struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// OpenDeletedHolder describes one process holding an open descriptor for a
// deleted filesystem object. Descriptors are sorted numeric strings from
// /proc/<pid>/fd.
type OpenDeletedHolder struct {
	PID         int      `json:"pid"`
	Process     string   `json:"process,omitempty"`
	Descriptors []string `json:"descriptors"`
}

// OpenDeletedFile is one unique regular filesystem object whose link count is
// zero but which remains open in at least one process. Device and Inode form the
// identity; Path is the lexicographically first observed former path, and Paths
// contains every observed former path when more than one was found.
type OpenDeletedFile struct {
	Device    uint64              `json:"device"`
	Inode     uint64              `json:"inode"`
	Path      string              `json:"path"`
	Paths     []string            `json:"paths,omitempty"`
	Size      int64               `json:"size_bytes"`
	Allocated int64               `json:"allocated_bytes"`
	Holders   []OpenDeletedHolder `json:"holders"`
}

// OpenDeletedCoverage describes how completely Linux /proc was inspected.
// Complete is false whenever a process or descriptor could not be inspected,
// a safety limit truncated the walk, or the context was canceled.
type OpenDeletedCoverage struct {
	Complete               bool `json:"complete"`
	ProcessEntries         int  `json:"process_entries"`
	ProcessesScanned       int  `json:"processes_scanned"`
	ProcessesSkipped       int  `json:"processes_skipped"`
	DescriptorEntries      int  `json:"descriptor_entries"`
	DescriptorsScanned     int  `json:"descriptors_scanned"`
	DescriptorsSkipped     int  `json:"descriptors_skipped"`
	ProcessLimitReached    bool `json:"process_limit_reached,omitempty"`
	DescriptorLimitReached bool `json:"descriptor_limit_reached,omitempty"`
	Canceled               bool `json:"canceled,omitempty"`
}

// OpenDeletedSummary contains unique-object totals. ReclaimableBytes is the
// allocated-byte total, not the sum of descriptor rows. When Coverage.Complete
// is false it is a trustworthy observed lower bound, not a claim of complete
// host coverage.
type OpenDeletedSummary struct {
	Objects          int                 `json:"objects"`
	Holders          int                 `json:"holders"`
	Descriptors      int                 `json:"descriptors"`
	LogicalBytes     int64               `json:"logical_bytes"`
	AllocatedBytes   int64               `json:"allocated_bytes"`
	ReclaimableBytes int64               `json:"reclaimable_bytes"`
	Coverage         OpenDeletedCoverage `json:"coverage"`
}

type openDeletedReport struct {
	Files   []OpenDeletedFile
	Summary *OpenDeletedSummary
}

// Result contains portable volume status and all available platform evidence.
// Probe failures that do not invalidate the whole result are returned as
// warnings so permission-restricted /proc trees remain useful.
type Result struct {
	SchemaVersion      int                 `json:"schema_version"`
	Capabilities       []Capability        `json:"capabilities"`
	Volumes            []fsinfo.Volume     `json:"volumes,omitempty"`
	OpenDeleted        []OpenDeletedFile   `json:"open_deleted,omitempty"`
	OpenDeletedSummary *OpenDeletedSummary `json:"open_deleted_summary,omitempty"`
	Warnings           []string            `json:"warnings,omitempty"`
}

// Gather inspects the volumes containing paths and runs bounded platform
// diagnostics. Empty paths still run host diagnostics, but omit volume status.
func Gather(ctx context.Context, paths []string) Result {
	result := Result{SchemaVersion: SchemaVersion}
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

	capability, report, warnings := gatherOpenDeleted(ctx, paths)
	result.Capabilities = append(result.Capabilities, capability)
	result.OpenDeleted = report.Files
	result.OpenDeletedSummary = report.Summary
	result.Warnings = append(result.Warnings, warnings...)
	return result
}
