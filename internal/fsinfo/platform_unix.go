//go:build linux || darwin

package fsinfo

import (
	"fmt"
	"io/fs"
	"os/user"
	"strconv"
	"syscall"
)

func allocatedBytes(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return info.Size()
}

func identity(_ string, info fs.FileInfo) Identity {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return Identity{Device: uint64(st.Dev), File: uint64(st.Ino), Valid: true}
	}
	return Identity{}
}

func linkCount(_ string, info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 0
}

func ownership(info fs.FileInfo) (uid, gid, owner, group string) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", "", "", ""
	}
	uid, gid = strconv.FormatUint(uint64(st.Uid), 10), strconv.FormatUint(uint64(st.Gid), 10)
	if u, err := user.LookupId(uid); err == nil {
		owner = u.Username
	}
	if g, err := user.LookupGroupId(gid); err == nil {
		group = g.Name
	}
	return uid, gid, owner, group
}

func platformVolumeFor(path string) (Volume, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Volume{}, fmt.Errorf("statfs %q: %w", path, err)
	}
	block := uint64(st.Bsize)
	return Volume{
		Total: uint64(st.Blocks) * block, Free: uint64(st.Bfree) * block,
		Available: uint64(st.Bavail) * block,
		Inodes:    uint64(st.Files), InodesFree: uint64(st.Ffree),
	}, nil
}
