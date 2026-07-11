//go:build linux

package diagnose

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	maxProcesses = 4096
	maxFDs       = 65536
	maxCommBytes = 256
)

func gatherOpenDeleted(ctx context.Context, paths []string) (Capability, []OpenDeletedFile, []string) {
	capability := Capability{Name: "open-deleted-files", Available: true}
	roots := cleanRoots(paths)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		capability.Available = false
		capability.Reason = err.Error()
		return capability, nil, nil
	}
	files := make([]OpenDeletedFile, 0)
	warnings := make([]string, 0)
	processes, descriptors := 0, 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			warnings = append(warnings, err.Error())
			break
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		if processes >= maxProcesses {
			warnings = append(warnings, fmt.Sprintf("open-deleted scan limited to %d processes", maxProcesses))
			break
		}
		processes++
		fdDir := filepath.Join("/proc", entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			// Processes commonly exit or deny access during this walk.
			continue
		}
		processName := readProcessName(filepath.Join("/proc", entry.Name(), "comm"))
		for _, fd := range fds {
			if descriptors >= maxFDs {
				warnings = append(warnings, fmt.Sprintf("open-deleted scan limited to %d file descriptors", maxFDs))
				break
			}
			descriptors++
			fdPath := filepath.Join(fdDir, fd.Name())
			target, err := os.Readlink(fdPath)
			if err != nil || !strings.HasSuffix(target, " (deleted)") {
				continue
			}
			path := strings.TrimSuffix(target, " (deleted)")
			if len(roots) > 0 && !withinAny(roots, path) {
				continue
			}
			info, err := os.Stat(fdPath)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			files = append(files, OpenDeletedFile{
				PID: pid, Process: processName, Descriptor: fd.Name(),
				Path: path, Size: info.Size(),
			})
		}
		if descriptors >= maxFDs {
			break
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Size != files[j].Size {
			return files[i].Size > files[j].Size
		}
		if files[i].PID != files[j].PID {
			return files[i].PID < files[j].PID
		}
		return files[i].Descriptor < files[j].Descriptor
	})
	return capability, files, warnings
}

func readProcessName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	data, _ := io.ReadAll(io.LimitReader(f, maxCommBytes))
	return strings.TrimSpace(string(data))
}

func cleanRoots(paths []string) []string {
	roots := make([]string, 0, len(paths))
	for _, path := range paths {
		abs, err := filepath.Abs(filepath.Clean(path))
		if err == nil {
			roots = append(roots, abs)
		}
	}
	return roots
}

func withinAny(roots []string, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	for _, root := range roots {
		rel, err := filepath.Rel(root, filepath.Clean(path))
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
