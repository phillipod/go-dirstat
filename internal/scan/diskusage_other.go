//go:build !linux && !darwin && !windows

package scan

import "io/fs"

// On platforms without the Unix stat_t, fall back to apparent size for the
// on-disk mode and treat every entry as the same device (no cross-device
// pruning). The real targets (linux/darwin) use the stat_t path.
func allocBytes(info fs.FileInfo) int64 { return info.Size() }

func devOf(fs.FileInfo) uint64 { return 0 }

func identityOf(fs.FileInfo) (dev, ino uint64, ok bool) { return 0, 0, false }

func linkCount(fs.FileInfo) uint64 { return 0 }

func devOfPath(_ string, info fs.FileInfo) uint64 { return devOf(info) }

func identityOfPath(_ string, info fs.FileInfo) (dev, ino uint64, ok bool) {
	return identityOf(info)
}

func linkCountPath(_ string, info fs.FileInfo) uint64 { return linkCount(info) }
