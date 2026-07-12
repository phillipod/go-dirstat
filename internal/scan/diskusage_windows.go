//go:build windows

package scan

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows' FileInfo.Sys contains size and timestamps but not the volume/file
// identity needed for loop protection and hardlink deduplication. Querying the
// handle gives us the same stable identity that os.SameFile uses internally.
func allocBytes(info fs.FileInfo) int64 { return info.Size() }

func devOf(info fs.FileInfo) uint64 { return 0 }

func identityOf(info fs.FileInfo) (dev, ino uint64, ok bool) {
	return 0, 0, false
}

func linkCount(info fs.FileInfo) uint64 { return 0 }

func devOfPath(path string, info fs.FileInfo) uint64 {
	dev, _, ok := identityOfPath(path, info)
	return devIfKnown(dev, ok)
}

func devOfPathNoFollow(path string, info fs.FileInfo) uint64 {
	dev, _, ok := identityOfPathNoFollow(path, info)
	return devIfKnown(dev, ok)
}

func identityOfPath(path string, info fs.FileInfo) (dev, ino uint64, ok bool) {
	file, err := openMetadataHandle(path, info)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = file.Close() }()

	var data syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &data); err != nil {
		return 0, 0, false
	}
	return uint64(data.VolumeSerialNumber), uint64(data.FileIndexHigh)<<32 | uint64(data.FileIndexLow), true
}

func identityOfPathNoFollow(path string, _ fs.FileInfo) (dev, ino uint64, ok bool) {
	file, err := openNoFollowMetadataHandle(path)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = file.Close() }()

	var data syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &data); err != nil {
		return 0, 0, false
	}
	return uint64(data.VolumeSerialNumber), uint64(data.FileIndexHigh)<<32 | uint64(data.FileIndexLow), true
}

func linkCountPath(path string, info fs.FileInfo) uint64 {
	file, err := openMetadataHandle(path, info)
	if err != nil {
		return 0
	}
	defer func() { _ = file.Close() }()

	var data syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &data); err != nil {
		return 0
	}
	return uint64(data.NumberOfLinks)
}

func linkCountPathNoFollow(path string, _ fs.FileInfo) uint64 {
	file, err := openNoFollowMetadataHandle(path)
	if err != nil {
		return 0
	}
	defer func() { _ = file.Close() }()

	var data syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &data); err != nil {
		return 0
	}
	return uint64(data.NumberOfLinks)
}

func devIfKnown(dev uint64, ok bool) uint64 {
	if !ok {
		return 0
	}
	return dev
}

// openMetadataHandle obtains the handle used for identity and link-count
// queries. Windows follows a symlink or mount-point reparse point when a path
// is opened normally. That is correct after statEntry has deliberately
// followed an alias, but it violates the scanner's default no-follow policy
// when the FileInfo came from Lstat/DirEntry.Info. In that case open the
// reparse point itself so metadata collection cannot touch the target (for
// example, a UNC path that would otherwise trigger outbound authentication).
//
// FILE_FLAG_BACKUP_SEMANTICS permits opening directories for metadata. The
// share-delete flag keeps this metadata probe from changing normal rename and
// cleanup behaviour.
func openMetadataHandle(path string, info fs.FileInfo) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	attributes := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS)
	if info != nil && metadataReparseMode(info.Mode()) {
		attributes |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	}
	handle, err := windows.CreateFile(
		name,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		attributes,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, os.ErrInvalid
	}
	return file, nil
}

func openNoFollowMetadataHandle(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, os.ErrInvalid
	}
	return file, nil
}

// metadataReparseMode identifies modes returned by Lstat/DirEntry.Info for
// aliases. Ordinary Windows symlinks carry ModeSymlink; directory junctions
// are exposed by Go as ModeIrregular because they are mount-point reparse
// points rather than ordinary symbolic links.
func metadataReparseMode(mode fs.FileMode) bool {
	return mode&(fs.ModeSymlink|fs.ModeIrregular) != 0
}

// resolvedAliasPath returns the canonical target path represented by a
// followed Windows symlink or junction. filepath.EvalSymlinks intentionally
// preserves a final mount-point junction on current Go releases, so policy
// checks that need the real target must ask Windows for the final path of a
// handle opened with normal (followed) semantics.
func resolvedAliasPath(path string) (string, error) {
	file, err := openMetadataHandle(path, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	const (
		initialBuffer = uint32(256)
		maxBuffer     = uint32(32768)
	)
	for size := initialBuffer; size <= maxBuffer; size *= 2 {
		buffer := make([]uint16, size)
		length, err := windows.GetFinalPathNameByHandle(
			windows.Handle(file.Fd()), &buffer[0], size, 0, // FILE_NAME_NORMALIZED
		)
		if err != nil {
			return "", err
		}
		if length+1 < size {
			return normalizeWindowsFinalPath(windows.UTF16ToString(buffer[:length])), nil
		}
	}
	return "", errors.New("windows final path exceeds maximum supported length")
}

func normalizeWindowsFinalPath(path string) string {
	const (
		uncPrefix      = `\\?\UNC\`
		verbatimPrefix = `\\?\`
	)
	if strings.HasPrefix(path, uncPrefix) {
		return `\\` + strings.TrimPrefix(path, uncPrefix)
	}
	// Keep device-volume paths in their verbatim form. They do not have a
	// drive-letter spelling that can be safely reconstructed for policy checks.
	if strings.HasPrefix(path, verbatimPrefix) && len(path) >= len(verbatimPrefix)+2 && path[len(verbatimPrefix)+1] == ':' {
		return path[len(verbatimPrefix):]
	}
	return path
}
