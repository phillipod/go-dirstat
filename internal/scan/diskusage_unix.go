//go:build linux || darwin

package scan

import (
	"io/fs"
	"syscall"
)

// allocBytes returns the on-disk allocation for a file: the number of
// allocated 512-byte blocks (stat.st_blocks) times 512. This is exactly the
// quantity plain `du` reports, so the on-disk size mode matches `du` output.
func allocBytes(info fs.FileInfo) int64 {
	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		return s.Blocks * 512 // st_blocks is always in 512-byte units
	}
	return info.Size()
}

// devOf extracts the device number (st_dev) used to detect filesystem
// boundaries for the cross-device / same-filesystem policy.
func devOf(info fs.FileInfo) uint64 {
	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Dev)
	}
	return 0
}

// identityOf returns the stable filesystem identity used for directory-loop
// protection. The boolean distinguishes a real zero-valued identity from a
// platform where this metadata is unavailable.
func identityOf(info fs.FileInfo) (dev, ino uint64, ok bool) {
	if s, valid := info.Sys().(*syscall.Stat_t); valid {
		return uint64(s.Dev), uint64(s.Ino), true
	}
	return 0, 0, false
}

// linkCount returns the number of hard links to the file (st_nlink). The scanner
// uses it to decide whether a file might appear under another name and so needs
// inode deduplication. Returns 0 on platforms without a real stat_t, which
// disables dedup there (the supported targets are linux and darwin).
func linkCount(info fs.FileInfo) uint64 {
	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Nlink)
	}
	return 0
}

// Path-aware wrappers keep the scanner portable while allowing platforms such
// as Windows to obtain stable identities from an open handle. Unix stat data
// already carries everything needed, so no extra I/O is required here.
func devOfPath(_ string, info fs.FileInfo) uint64 { return devOf(info) }

func devOfPathNoFollow(_ string, info fs.FileInfo) uint64 { return devOf(info) }

func identityOfPath(_ string, info fs.FileInfo) (dev, ino uint64, ok bool) {
	return identityOf(info)
}

func identityOfPathNoFollow(_ string, info fs.FileInfo) (dev, ino uint64, ok bool) {
	return identityOf(info)
}

func linkCountPath(_ string, info fs.FileInfo) uint64 { return linkCount(info) }

func linkCountPathNoFollow(_ string, info fs.FileInfo) uint64 { return linkCount(info) }
