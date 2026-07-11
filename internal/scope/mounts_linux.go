//go:build linux

package scope

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
)

// mountTable maps mount points to their filesystem type, read once from
// /proc/self/mountinfo. It lets the policy resolve any path's fstype by
// longest-prefix match against the mount points.
type mountTable struct {
	byPath map[string]string // mountpoint -> fstype
	points []string          // mountpoints, longest first, for prefix lookup
}

func loadMounts() *mountTable {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer func() {
		_ = f.Close()
	}()

	mt := &mountTable{byPath: make(map[string]string)}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// "1 0 8:1 / / rw,... - ext4 /dev/sda1 rw,..."
		// Everything before " - " is per-mount fields; after is fstype/source.
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		left := strings.Fields(parts[0])
		right := strings.Fields(parts[1])
		if len(left) < 5 || len(right) < 1 {
			continue
		}
		mp := unescapeMountinfo(left[4])
		mt.byPath[mp] = right[0]
	}
	for k := range mt.byPath {
		mt.points = append(mt.points, k)
	}
	// Longest first so the prefix scan matches the most specific mount point.
	sort.Slice(mt.points, func(i, j int) bool { return len(mt.points[i]) > len(mt.points[j]) })
	if len(mt.byPath) == 0 {
		return nil
	}
	return mt
}

// fstype returns the filesystem type for path by finding the longest mount
// point that is path or an ancestor of it. "" if unknown.
func (mt *mountTable) fstype(path string) string {
	if mt == nil {
		return ""
	}
	for _, mp := range mt.points {
		if mp == "/" || path == mp || strings.HasPrefix(path, mp+"/") {
			return mt.byPath[mp]
		}
	}
	return ""
}

// unescapeMountinfo reverses the octal escapes mountinfo uses for spaces,
// tabs, newlines, and backslashes in mount points (\040 etc.).
func unescapeMountinfo(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseInt(s[i+1:i+4], 8, 16); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
