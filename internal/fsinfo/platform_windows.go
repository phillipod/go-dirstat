//go:build windows

package fsinfo

import (
	"io/fs"
	"os"
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

func volumeFor(path string) (Volume, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return Volume{}, err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &available, &total, &free); err != nil {
		return Volume{}, err
	}
	return Volume{Total: total, Free: free, Available: available}, nil
}
