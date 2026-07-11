//go:build linux

package fsinfo

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type volumeMount struct {
	path       string
	filesystem string
	device     string
}

func volumeIdentityFor(path string) (mountPoint, filesystem, device string) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", "", ""
	}
	defer func() { _ = f.Close() }()

	mounts := make([]volumeMount, 0, 32)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), " - ", 2)
		if len(parts) != 2 {
			continue
		}
		left, right := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(left) < 5 || len(right) < 2 {
			continue
		}
		mounts = append(mounts, volumeMount{
			path:       unescapeMountPath(left[4]),
			filesystem: right[0],
			device:     unescapeMountPath(right[1]),
		})
	}
	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i].path) > len(mounts[j].path) })
	clean := filepath.Clean(path)
	for _, mount := range mounts {
		if mount.path == string(filepath.Separator) || clean == mount.path ||
			strings.HasPrefix(clean, mount.path+string(filepath.Separator)) {
			return mount.path, mount.filesystem, mount.device
		}
	}
	return "", "", ""
}

func unescapeMountPath(value string) string {
	if !strings.ContainsRune(value, '\\') {
		return value
	}
	var result strings.Builder
	result.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+3 < len(value) {
			if decoded, err := strconv.ParseUint(value[i+1:i+4], 8, 8); err == nil {
				result.WriteByte(byte(decoded))
				i += 3
				continue
			}
		}
		result.WriteByte(value[i])
	}
	return result.String()
}
