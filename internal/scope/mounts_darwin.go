//go:build darwin

package scope

import (
	"syscall"
)

// macOS exposes the filesystem type for any path through statfs(2). Resolving
// on demand is more reliable than parsing the human-readable `mount` output:
// it follows aliases such as /var -> /private/var and needs no subprocess.
type mountTable struct{}

func loadMounts() *mountTable { return &mountTable{} }

func (mt *mountTable) fstype(path string) string {
	if mt == nil {
		return ""
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return ""
	}
	var name []byte
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return string(name)
}
