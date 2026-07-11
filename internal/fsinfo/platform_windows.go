//go:build windows

package fsinfo

import (
	"io/fs"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func allocatedBytes(info fs.FileInfo) int64 { return info.Size() }

func identity(path string, _ fs.FileInfo) Identity {
	f, err := os.Open(path)
	if err != nil {
		return Identity{}
	}
	defer func() { _ = f.Close() }()
	var data syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &data) != nil {
		return Identity{}
	}
	return Identity{
		Device: uint64(data.VolumeSerialNumber),
		File:   uint64(data.FileIndexHigh)<<32 | uint64(data.FileIndexLow), Valid: true,
	}
}

func linkCount(path string, _ fs.FileInfo) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	var data syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &data) != nil {
		return 0
	}
	return uint64(data.NumberOfLinks)
}

func ownership(fs.FileInfo) (string, string, string, string) { return "", "", "", "" }

// OwnershipAvailable is false until Windows SID lookup is implemented. Query
// surfaces use this capability to reject misleading empty ownership results.
func OwnershipAvailable() bool { return false }

func platformVolumeFor(path string) (Volume, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return Volume{}, err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &available, &total, &free); err != nil {
		return Volume{}, err
	}
	const pathBufferSize = 32768
	mountBuffer := make([]uint16, pathBufferSize)
	if err := windows.GetVolumePathName(p, &mountBuffer[0], uint32(len(mountBuffer))); err != nil {
		return Volume{}, err
	}
	mountPoint := windows.UTF16ToString(mountBuffer)
	mount, err := windows.UTF16PtrFromString(mountPoint)
	if err != nil {
		return Volume{}, err
	}
	volumeBuffer := make([]uint16, pathBufferSize)
	if err := windows.GetVolumeNameForVolumeMountPoint(mount, &volumeBuffer[0], uint32(len(volumeBuffer))); err != nil {
		return Volume{}, err
	}
	filesystemBuffer := make([]uint16, 256)
	if err := windows.GetVolumeInformation(mount, nil, 0, nil, nil, nil, &filesystemBuffer[0], uint32(len(filesystemBuffer))); err != nil {
		return Volume{}, err
	}
	return Volume{
		Total: total, Free: free, Available: available,
		MountPoint: mountPoint,
		Device:     strings.TrimSpace(windows.UTF16ToString(volumeBuffer)),
		Filesystem: strings.TrimSpace(windows.UTF16ToString(filesystemBuffer)),
	}, nil
}
