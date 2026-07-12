//go:build linux || darwin

package fsinfo

import (
	"fmt"
	"io/fs"
	"os/user"
	"strconv"
	"sync"
	"syscall"
)

var ownerNames, groupNames sync.Map

func allocatedBytes(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return info.Size()
}

func identity(_ string, info fs.FileInfo, _ bool) Identity {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return Identity{Device: uint64(st.Dev), File: uint64(st.Ino), Valid: true}
	}
	return Identity{}
}

func linkCount(_ string, info fs.FileInfo, _ bool) uint64 {
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
	owner = cachedIdentityName(&ownerNames, uid, func(id string) (string, error) {
		value, err := user.LookupId(id)
		if err != nil {
			return "", err
		}
		return value.Username, nil
	})
	group = cachedIdentityName(&groupNames, gid, func(id string) (string, error) {
		value, err := user.LookupGroupId(id)
		if err != nil {
			return "", err
		}
		return value.Name, nil
	})
	return uid, gid, owner, group
}

func cachedIdentityName(cache *sync.Map, id string, lookup func(string) (string, error)) string {
	if cached, ok := cache.Load(id); ok {
		if name, valid := cached.(string); valid {
			return name
		}
	}
	name, err := lookup(id)
	if err != nil {
		name = ""
	}
	actual, _ := cache.LoadOrStore(id, name)
	resolved, _ := actual.(string)
	return resolved
}

// OwnershipAvailable reports whether this platform can populate UID/GID and
// owner/group metadata for query filters and fields.
func OwnershipAvailable() bool { return true }

func platformVolumeFor(path string) (Volume, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Volume{}, fmt.Errorf("statfs %q: %w", path, err)
	}
	block := uint64(st.Bsize)
	v := Volume{
		Total: uint64(st.Blocks) * block, Free: uint64(st.Bfree) * block,
		Available: uint64(st.Bavail) * block,
		Inodes:    uint64(st.Files), InodesFree: uint64(st.Ffree),
	}
	v.MountPoint, v.Filesystem, v.Device = volumeIdentityFor(path)
	return v, nil
}
