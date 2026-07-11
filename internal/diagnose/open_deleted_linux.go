//go:build linux

package diagnose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const (
	maxProcesses = 4096
	maxFDs       = 65536
	maxCommBytes = 256
)

type openDeletedProbe struct {
	procRoot        string
	processLimit    int
	descriptorLimit int
	readDir         func(string) ([]os.DirEntry, error)
	readlink        func(string) (string, error)
	stat            func(string) (fs.FileInfo, error)
}

func defaultOpenDeletedProbe() openDeletedProbe {
	return openDeletedProbe{
		procRoot:        "/proc",
		processLimit:    maxProcesses,
		descriptorLimit: maxFDs,
		readDir:         os.ReadDir,
		readlink:        os.Readlink,
		stat:            os.Stat,
	}
}

type deletedObjectKey struct {
	device uint64
	inode  uint64
}

type openDeletedAccumulator struct {
	file    OpenDeletedFile
	paths   map[string]struct{}
	holders map[int]*OpenDeletedHolder
}

func gatherOpenDeleted(ctx context.Context, paths []string) (Capability, openDeletedReport, []string) {
	return gatherOpenDeletedWithProbe(ctx, paths, defaultOpenDeletedProbe())
}

func gatherOpenDeletedWithProbe(ctx context.Context, paths []string, probe openDeletedProbe) (Capability, openDeletedReport, []string) {
	capability := Capability{Name: "open-deleted-files", Available: true}
	roots := cleanRoots(paths)
	entries, err := probe.readDir(probe.procRoot)
	if err != nil {
		capability.Available = false
		capability.Reason = err.Error()
		return capability, openDeletedReport{}, nil
	}

	coverage := OpenDeletedCoverage{}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err == nil && entry.IsDir() {
			coverage.ProcessEntries++
		}
	}
	objects := make(map[deletedObjectKey]*openDeletedAccumulator)
	warnings := make([]string, 0)
	descriptorAttempts := 0

processLoop:
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			coverage.Canceled = true
			warnings = append(warnings, err.Error())
			break
		}
		if coverage.ProcessesScanned+coverage.ProcessesSkipped >= probe.processLimit {
			coverage.ProcessLimitReached = true
			break
		}

		fdDir := filepath.Join(probe.procRoot, entry.Name(), "fd")
		fds, err := probe.readDir(fdDir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// An exited process no longer owns reclaimable descriptors, so its
				// disappearance does not make the current result incomplete.
				coverage.ProcessesScanned++
				continue
			}
			// Processes commonly exit or deny access during this walk. Record the
			// coverage loss instead of silently presenting a complete total.
			coverage.ProcessesSkipped++
			continue
		}
		coverage.ProcessesScanned++
		processName := readProcessName(filepath.Join(probe.procRoot, entry.Name(), "comm"))

		for _, fd := range fds {
			if err := ctx.Err(); err != nil {
				coverage.Canceled = true
				warnings = append(warnings, err.Error())
				break processLoop
			}
			if descriptorAttempts >= probe.descriptorLimit {
				coverage.DescriptorLimitReached = true
				break processLoop
			}
			descriptorAttempts++
			coverage.DescriptorEntries++

			fdPath := filepath.Join(fdDir, fd.Name())
			target, err := probe.readlink(fdPath)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					// A descriptor that closed after enumeration cannot retain
					// storage at the time the report is returned.
					coverage.DescriptorsScanned++
					continue
				}
				coverage.DescriptorsSkipped++
				continue
			}
			if !strings.HasSuffix(target, " (deleted)") {
				coverage.DescriptorsScanned++
				continue
			}

			info, err := probe.stat(fdPath)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					coverage.DescriptorsScanned++
					continue
				}
				coverage.DescriptorsSkipped++
				continue
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				coverage.DescriptorsSkipped++
				continue
			}
			coverage.DescriptorsScanned++
			// The textual suffix alone is ambiguous: a live filename may
			// literally end in " (deleted)". Only a zero-link regular object is
			// reclaimable when its final descriptor closes.
			if !info.Mode().IsRegular() || stat.Nlink != 0 {
				continue
			}

			path := strings.TrimSuffix(target, " (deleted)")
			if len(roots) > 0 && !withinAny(roots, path) {
				continue
			}
			key := deletedObjectKey{device: uint64(stat.Dev), inode: uint64(stat.Ino)}
			object := objects[key]
			if object == nil {
				allocated := stat.Blocks * 512
				if allocated < 0 {
					allocated = 0
				}
				object = &openDeletedAccumulator{
					file: OpenDeletedFile{
						Device: uint64(stat.Dev), Inode: uint64(stat.Ino),
						Size: info.Size(), Allocated: allocated,
					},
					paths:   make(map[string]struct{}),
					holders: make(map[int]*OpenDeletedHolder),
				}
				objects[key] = object
			}
			object.paths[path] = struct{}{}
			holder := object.holders[pid]
			if holder == nil {
				holder = &OpenDeletedHolder{PID: pid, Process: processName}
				object.holders[pid] = holder
			}
			holder.Descriptors = append(holder.Descriptors, fd.Name())
		}
	}

	coverage.Complete = !coverage.ProcessLimitReached && !coverage.DescriptorLimitReached &&
		!coverage.Canceled && coverage.ProcessesSkipped == 0 && coverage.DescriptorsSkipped == 0 &&
		coverage.ProcessesScanned == coverage.ProcessEntries
	files, summary := finishOpenDeleted(objects, coverage)
	if !coverage.Complete {
		warnings = append(warnings, fmt.Sprintf(
			"open-deleted /proc coverage partial: processes scanned=%d skipped=%d entries=%d; descriptors scanned=%d skipped=%d entries=%d",
			coverage.ProcessesScanned, coverage.ProcessesSkipped, coverage.ProcessEntries,
			coverage.DescriptorsScanned, coverage.DescriptorsSkipped, coverage.DescriptorEntries,
		))
	}
	return capability, openDeletedReport{Files: files, Summary: &summary}, warnings
}

func finishOpenDeleted(objects map[deletedObjectKey]*openDeletedAccumulator, coverage OpenDeletedCoverage) ([]OpenDeletedFile, OpenDeletedSummary) {
	files := make([]OpenDeletedFile, 0, len(objects))
	for _, object := range objects {
		paths := make([]string, 0, len(object.paths))
		for path := range object.paths {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		if len(paths) > 0 {
			object.file.Path = paths[0]
		}
		if len(paths) > 1 {
			object.file.Paths = paths
		}

		pids := make([]int, 0, len(object.holders))
		for pid := range object.holders {
			pids = append(pids, pid)
		}
		sort.Ints(pids)
		object.file.Holders = make([]OpenDeletedHolder, 0, len(pids))
		for _, pid := range pids {
			holder := object.holders[pid]
			sort.Slice(holder.Descriptors, func(i, j int) bool {
				return descriptorLess(holder.Descriptors[i], holder.Descriptors[j])
			})
			object.file.Holders = append(object.file.Holders, *holder)
		}
		files = append(files, object.file)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Allocated != files[j].Allocated {
			return files[i].Allocated > files[j].Allocated
		}
		if files[i].Size != files[j].Size {
			return files[i].Size > files[j].Size
		}
		if files[i].Path != files[j].Path {
			return files[i].Path < files[j].Path
		}
		if files[i].Device != files[j].Device {
			return files[i].Device < files[j].Device
		}
		return files[i].Inode < files[j].Inode
	})

	summary := OpenDeletedSummary{Objects: len(files), Coverage: coverage}
	for _, file := range files {
		summary.LogicalBytes += file.Size
		summary.AllocatedBytes += file.Allocated
		summary.ReclaimableBytes += file.Allocated
		summary.Holders += len(file.Holders)
		for _, holder := range file.Holders {
			summary.Descriptors += len(holder.Descriptors)
		}
	}
	return files, summary
}

func descriptorLess(a, b string) bool {
	ai, aErr := strconv.Atoi(a)
	bi, bErr := strconv.Atoi(b)
	if aErr == nil && bErr == nil && ai != bi {
		return ai < bi
	}
	return a < b
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
