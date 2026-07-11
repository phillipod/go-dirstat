//go:build windows

package scan

import (
	"io/fs"
	"os"
	"syscall"
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

func identityOfPath(path string, _ fs.FileInfo) (dev, ino uint64, ok bool) {
	file, err := os.Open(path)
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

func linkCountPath(path string, _ fs.FileInfo) uint64 {
	file, err := os.Open(path)
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
