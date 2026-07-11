// Package fsinfo provides portable, on-demand filesystem metadata for
// interactive inspection and guarded mutation. The scanner deliberately keeps
// its per-node model compact; richer metadata is loaded only for selected or
// scripted candidates.
package fsinfo

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Identity is the stable filesystem identity used to detect replaced paths.
// Device and File are platform-defined (volume/file index on Windows).
type Identity struct {
	Device uint64 `json:"device,omitempty"`
	File   uint64 `json:"file,omitempty"`
	Valid  bool   `json:"valid"`
}

// Entry describes one filesystem object at a point in time.
type Entry struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Mode       uint32    `json:"mode"`
	ModeText   string    `json:"mode_text"`
	Size       int64     `json:"size"`
	Allocated  int64     `json:"allocated"`
	ModTime    time.Time `json:"modified_at"`
	UID        string    `json:"uid,omitempty"`
	GID        string    `json:"gid,omitempty"`
	Owner      string    `json:"owner,omitempty"`
	Group      string    `json:"group,omitempty"`
	Links      uint64    `json:"links,omitempty"`
	Identity   Identity  `json:"identity"`
	Symlink    string    `json:"symlink_target,omitempty"`
	Executable bool      `json:"executable,omitempty"`
}

// PathExpectation records whether a path existed and, when it did, the exact
// object observed there. Keeping absence explicit lets mutation plans guard a
// destination that was empty when the plan was reviewed.
type PathExpectation struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Entry  *Entry `json:"entry,omitempty"`
}

// CapturePath records the current no-follow state of path. A missing final
// component is a valid state; all other inspection errors are returned.
func CapturePath(path string) (PathExpectation, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return PathExpectation{}, fmt.Errorf("absolute path: %w", err)
	}
	entry, err := Inspect(abs, false)
	if err == nil {
		return PathExpectation{Path: abs, Exists: true, Entry: &entry}, nil
	}
	if os.IsNotExist(err) {
		return PathExpectation{Path: abs}, nil
	}
	return PathExpectation{}, err
}

// Inspect loads metadata without following the final symlink unless follow is
// true. Paths are made absolute but are not required to be inside a scan root.
func Inspect(path string, follow bool) (Entry, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return Entry{}, fmt.Errorf("absolute path: %w", err)
	}
	var info fs.FileInfo
	if follow {
		info, err = os.Stat(abs)
	} else {
		info, err = os.Lstat(abs)
	}
	if err != nil {
		return Entry{}, err
	}
	var kind string
	switch {
	case info.IsDir():
		kind = "directory"
	case info.Mode()&os.ModeSymlink != 0:
		kind = "symlink"
	case info.Mode().IsRegular():
		kind = "file"
	case info.Mode()&os.ModeNamedPipe != 0:
		kind = "fifo"
	case info.Mode()&os.ModeSocket != 0:
		kind = "socket"
	case info.Mode()&os.ModeDevice != 0:
		kind = "device"
	default:
		kind = "other"
	}
	e := Entry{
		Path: abs, Name: info.Name(), Kind: kind,
		Mode: uint32(info.Mode()), ModeText: info.Mode().String(),
		Size: info.Size(), Allocated: allocatedBytes(info), ModTime: info.ModTime(),
		Identity: identity(abs, info), Links: linkCount(abs, info),
		Executable: info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0,
	}
	e.UID, e.GID, e.Owner, e.Group = ownership(info)
	if info.Mode()&os.ModeSymlink != 0 {
		e.Symlink, _ = os.Readlink(abs)
	}
	return e, nil
}

// SameObject reports whether actual still refers to the object captured in
// expected. Identity wins when available; portable metadata is the fallback.
func SameObject(expected, actual Entry) bool {
	if expected.Identity.Valid && actual.Identity.Valid {
		if expected.Identity.Device != actual.Identity.Device || expected.Identity.File != actual.Identity.File {
			return false
		}
	}
	return expected.Kind == actual.Kind && expected.Size == actual.Size && expected.ModTime.Equal(actual.ModTime)
}

// Volume describes capacity and inode pressure for the filesystem containing
// Path. Path is the caller-facing absolute path; ResolvedPath and all volume
// identity/capacity fields describe the same symlink-resolved target.
type Volume struct {
	Path              string  `json:"path"`
	ResolvedPath      string  `json:"resolved_path"`
	MountPoint        string  `json:"mount_point,omitempty"`
	Filesystem        string  `json:"filesystem,omitempty"`
	Device            string  `json:"device,omitempty"`
	Total             uint64  `json:"total_bytes"`
	Free              uint64  `json:"free_bytes"`
	Available         uint64  `json:"available_bytes"`
	Reserved          uint64  `json:"reserved_bytes,omitempty"`
	PhysicalUsed      uint64  `json:"physical_used_bytes"`
	PhysicalUsedPct   float64 `json:"physical_used_percent"`
	CallerCapacity    uint64  `json:"caller_capacity_bytes"`
	CallerPressurePct float64 `json:"caller_pressure_percent"`
	// Used and UsedPct are compatibility aliases for physical allocation.
	// New consumers should use the explicitly named fields above.
	Used       uint64  `json:"used_bytes"`
	UsedPct    float64 `json:"used_percent"`
	Inodes     uint64  `json:"inodes,omitempty"`
	InodesFree uint64  `json:"inodes_free,omitempty"`
	InodePct   float64 `json:"inode_used_percent,omitempty"`
}

// VolumeFor returns capacity information for the filesystem containing path.
func VolumeFor(path string) (Volume, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return Volume{}, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Volume{}, fmt.Errorf("resolve volume path %q: %w", abs, err)
	}
	v, err := platformVolumeFor(resolved)
	if err != nil {
		return Volume{}, err
	}
	v.Path = abs
	v.ResolvedPath = resolved
	finalizeVolume(&v)
	return v, nil
}

func finalizeVolume(v *Volume) {
	if v.Total >= v.Free {
		v.PhysicalUsed = v.Total - v.Free
	}
	v.Used = v.PhysicalUsed
	if v.Total > 0 {
		v.PhysicalUsedPct = float64(v.PhysicalUsed) * 100 / float64(v.Total)
	}
	v.UsedPct = v.PhysicalUsedPct
	if v.Free >= v.Available {
		v.Reserved = v.Free - v.Available
	}
	v.CallerCapacity = v.PhysicalUsed + v.Available
	if v.CallerCapacity > 0 {
		v.CallerPressurePct = float64(v.PhysicalUsed) * 100 / float64(v.CallerCapacity)
	}
	if v.Inodes > 0 && v.Inodes >= v.InodesFree {
		v.InodePct = float64(v.Inodes-v.InodesFree) * 100 / float64(v.Inodes)
	}
}
