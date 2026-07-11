//go:build !linux

package diagnose

import "context"

func gatherOpenDeleted(context.Context, []string) (Capability, openDeletedReport, []string) {
	return Capability{
		Name:      "open-deleted-files",
		Available: false,
		Reason:    "open-deleted process attribution requires Linux /proc",
	}, openDeletedReport{}, nil
}
