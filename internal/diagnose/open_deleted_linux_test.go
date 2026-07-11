//go:build linux

package diagnose

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestGatherFindsUniqueOpenDeletedObjectInsideRequestedRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "held-open.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	result := Gather(context.Background(), []string{root})
	file := findOpenDeletedByPath(result.OpenDeleted, path)
	if file == nil {
		t.Fatalf("open deleted object not found in %#v (warnings: %v)", result.OpenDeleted, result.Warnings)
	}
	if file.Device != uint64(stat.Dev) || file.Inode != uint64(stat.Ino) {
		t.Fatalf("identity = %d:%d, want %d:%d", file.Device, file.Inode, stat.Dev, stat.Ino)
	}
	if file.Size != 4096 || file.Allocated != stat.Blocks*512 {
		t.Fatalf("sizes = logical %d / allocated %d, want 4096 / %d", file.Size, file.Allocated, stat.Blocks*512)
	}
	if result.OpenDeletedSummary == nil || result.OpenDeletedSummary.Objects != 1 ||
		result.OpenDeletedSummary.ReclaimableBytes != file.Allocated {
		t.Fatalf("summary = %#v", result.OpenDeletedSummary)
	}
}

func TestGatherDoesNotReportLiveFilenameWithDeletedSuffix(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "live (deleted)")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("still linked"); err != nil {
		t.Fatal(err)
	}

	result := Gather(context.Background(), []string{root})
	if len(result.OpenDeleted) != 0 {
		t.Fatalf("live suffix file was reported as deleted: %#v", result.OpenDeleted)
	}
	if result.OpenDeletedSummary == nil || result.OpenDeletedSummary.Objects != 0 ||
		result.OpenDeletedSummary.ReclaimableBytes != 0 {
		t.Fatalf("summary = %#v, want zero unique objects", result.OpenDeletedSummary)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live suffix file disappeared: %v", err)
	}
}

func TestGatherGroupsDescriptorsAndProcessesByDeviceAndInode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "shared.log")
	first, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if _, err := first.WriteString("shared deleted object"); err != nil {
		t.Fatal(err)
	}
	second, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	cmd := exec.Command(os.Args[0], "-test.run=^TestOpenDeletedHolderProcess$")
	cmd.Env = append(os.Environ(), "DIRSTAT_OPEN_DELETED_HELPER=1")
	cmd.ExtraFiles = []*os.File{first}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("holder helper: %v", err)
		}
	}()
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(ready) != "ready" {
		t.Fatalf("holder helper readiness = %q, %v", ready, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	result := Gather(context.Background(), []string{root})
	file := findOpenDeletedByPath(result.OpenDeleted, path)
	if file == nil {
		t.Fatalf("shared deleted object not found: %#v", result.OpenDeleted)
	}
	if len(result.OpenDeleted) != 1 {
		t.Fatalf("unique objects = %d, want 1: %#v", len(result.OpenDeleted), result.OpenDeleted)
	}
	descriptorsByPID := make(map[int]int)
	for _, holder := range file.Holders {
		descriptorsByPID[holder.PID] = len(holder.Descriptors)
	}
	if descriptorsByPID[os.Getpid()] < 2 {
		t.Fatalf("parent descriptors = %d, want at least 2; holders=%#v", descriptorsByPID[os.Getpid()], file.Holders)
	}
	if descriptorsByPID[cmd.Process.Pid] < 1 {
		t.Fatalf("child descriptors = %d, want at least 1; holders=%#v", descriptorsByPID[cmd.Process.Pid], file.Holders)
	}
	if result.OpenDeletedSummary == nil || result.OpenDeletedSummary.Objects != 1 ||
		result.OpenDeletedSummary.Holders < 2 || result.OpenDeletedSummary.Descriptors < 3 ||
		result.OpenDeletedSummary.ReclaimableBytes != file.Allocated {
		t.Fatalf("summary = %#v", result.OpenDeletedSummary)
	}
}

func TestOpenDeletedHolderProcess(t *testing.T) {
	if os.Getenv("DIRSTAT_OPEN_DELETED_HELPER") != "1" {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestGatherReportsSparseLogicalAndAllocatedBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sparse.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	const logical = int64(8 << 20)
	if err := f.Truncate(logical); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{1}, logical-1); err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	wantAllocated := info.Sys().(*syscall.Stat_t).Blocks * 512
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	result := Gather(context.Background(), []string{root})
	file := findOpenDeletedByPath(result.OpenDeleted, path)
	if file == nil {
		t.Fatalf("sparse deleted object not found: %#v", result.OpenDeleted)
	}
	if file.Size != logical || file.Allocated != wantAllocated {
		t.Fatalf("sparse sizes = logical %d / allocated %d, want %d / %d", file.Size, file.Allocated, logical, wantAllocated)
	}
	if result.OpenDeletedSummary == nil || result.OpenDeletedSummary.LogicalBytes != logical ||
		result.OpenDeletedSummary.AllocatedBytes != wantAllocated ||
		result.OpenDeletedSummary.ReclaimableBytes != wantAllocated {
		t.Fatalf("sparse summary = %#v", result.OpenDeletedSummary)
	}
}

func TestGatherReportsPartialCoverageForUnreadableProcess(t *testing.T) {
	procRoot := t.TempDir()
	for _, pid := range []string{"100", "200"} {
		if err := os.MkdirAll(filepath.Join(procRoot, pid, "fd"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(procRoot, pid, "comm"), []byte("probe\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	denied := filepath.Join(procRoot, "200", "fd")
	probe := defaultOpenDeletedProbe()
	probe.procRoot = procRoot
	readDir := probe.readDir
	probe.readDir = func(path string) ([]os.DirEntry, error) {
		if path == denied {
			return nil, fs.ErrPermission
		}
		return readDir(path)
	}

	capability, report, warnings := gatherOpenDeletedWithProbe(context.Background(), nil, probe)
	if !capability.Available || report.Summary == nil {
		t.Fatalf("capability=%#v report=%#v", capability, report)
	}
	coverage := report.Summary.Coverage
	if coverage.Complete || coverage.ProcessEntries != 2 || coverage.ProcessesScanned != 1 || coverage.ProcessesSkipped != 1 {
		t.Fatalf("coverage = %#v", coverage)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "coverage partial") {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestWithinAnyHonorsPathBoundaries(t *testing.T) {
	if withinAny([]string{"/srv/data"}, "/srv/database/file") {
		t.Fatal("path prefix without a component boundary was accepted")
	}
	if !withinAny([]string{"/srv/data"}, "/srv/data/sub/file") {
		t.Fatal("descendant was rejected")
	}
}

func findOpenDeletedByPath(files []OpenDeletedFile, path string) *OpenDeletedFile {
	for i := range files {
		if files[i].Path == path {
			return &files[i]
		}
		for _, candidate := range files[i].Paths {
			if candidate == path {
				return &files[i]
			}
		}
	}
	return nil
}

func TestCoverageCountsDescriptorInspectionErrors(t *testing.T) {
	procRoot := t.TempDir()
	fdDir := filepath.Join(procRoot, "100", "fd")
	if err := os.MkdirAll(fdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(fdDir, "3")); err != nil {
		t.Fatal(err)
	}
	probe := defaultOpenDeletedProbe()
	probe.procRoot = procRoot
	probe.readlink = func(string) (string, error) { return "", errors.New("descriptor raced") }

	_, report, _ := gatherOpenDeletedWithProbe(context.Background(), nil, probe)
	coverage := report.Summary.Coverage
	if coverage.Complete || coverage.DescriptorEntries != 1 || coverage.DescriptorsSkipped != 1 {
		t.Fatalf("coverage = %#v", coverage)
	}
}
