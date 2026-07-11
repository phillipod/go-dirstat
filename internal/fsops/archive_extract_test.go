package fsops

import (
	"archive/tar"
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type archiveTestEntry struct {
	name     string
	data     string
	linkname string
	typeflag byte
	mode     os.FileMode
}

func TestArchiveRejectsSymlinkParentsRegardlessOfEntryOrder(t *testing.T) {
	orders := []struct {
		name    string
		entries []archiveTestEntry
		want    string
	}{
		{
			name: "links before child",
			entries: []archiveTestEntry{
				{name: "first", linkname: "second", typeflag: tar.TypeSymlink, mode: os.ModeSymlink | 0o777},
				{name: "second", linkname: "target", typeflag: tar.TypeSymlink, mode: os.ModeSymlink | 0o777},
				{name: "first/payload", data: "must not escape", typeflag: tar.TypeReg, mode: 0o600},
			},
			want: "non-directory parent",
		},
		{
			name: "child before links",
			entries: []archiveTestEntry{
				{name: "first/payload", data: "must not escape", typeflag: tar.TypeReg, mode: 0o600},
				{name: "second", linkname: "target", typeflag: tar.TypeSymlink, mode: os.ModeSymlink | 0o777},
				{name: "first", linkname: "second", typeflag: tar.TypeSymlink, mode: os.ModeSymlink | 0o777},
			},
			want: "has descendant entries",
		},
	}
	formats := []struct {
		name  string
		ext   string
		write func(*testing.T, string, []archiveTestEntry)
	}{
		{name: archiveFormatTar, ext: ".tar", write: writeTarTestArchive},
		{name: "zip", ext: ".zip", write: writeZipTestArchive},
	}
	for _, format := range formats {
		format := format
		for _, order := range orders {
			order := order
			t.Run(format.name+"/"+order.name, func(t *testing.T) {
				root := t.TempDir()
				archive := filepath.Join(root, "input"+format.ext)
				format.write(t, archive, order.entries)
				assertArchiveRejectedInDryRunAndApply(t, root, archive, format.name, order.want)
			})
		}
	}
}

func TestArchiveRejectsUnsafeLinksHardlinksSpecialEntriesAndTraversal(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	tests := []struct {
		name, format, ext, want string
		entries                 []archiveTestEntry
		write                   func(*testing.T, string, []archiveTestEntry)
	}{
		{
			name: "tar unsafe symlink", format: archiveFormatTar, ext: ".tar", want: "unsafe symlink target",
			entries: []archiveTestEntry{{name: "pivot", linkname: filepath.ToSlash(outside), typeflag: tar.TypeSymlink, mode: os.ModeSymlink | 0o777}},
			write:   writeTarTestArchive,
		},
		{
			name: "tar hardlink", format: archiveFormatTar, ext: ".tar", want: "hardlink",
			entries: []archiveTestEntry{
				{name: "target", data: "data", typeflag: tar.TypeReg, mode: 0o600},
				{name: "pivot", linkname: "target", typeflag: tar.TypeLink, mode: 0o600},
				{name: "pivot/child", data: "data", typeflag: tar.TypeReg, mode: 0o600},
			},
			write: writeTarTestArchive,
		},
		{
			name: "tar special entry", format: archiveFormatTar, ext: ".tar", want: "unsupported archive entry type",
			entries: []archiveTestEntry{{name: "pipe", typeflag: tar.TypeFifo, mode: os.ModeNamedPipe | 0o600}},
			write:   writeTarTestArchive,
		},
		{
			name: "tar traversal", format: archiveFormatTar, ext: ".tar", want: "unsafe archive path",
			entries: []archiveTestEntry{{name: "../outside", data: "data", typeflag: tar.TypeReg, mode: 0o600}},
			write:   writeTarTestArchive,
		},
		{
			name: "zip unsafe symlink", format: "zip", ext: ".zip", want: "unsafe symlink target",
			entries: []archiveTestEntry{{name: "pivot", data: filepath.ToSlash(outside), mode: os.ModeSymlink | 0o777}},
			write:   writeZipTestArchive,
		},
		{
			name: "zip special entry", format: "zip", ext: ".zip", want: "unsupported zip entry mode",
			entries: []archiveTestEntry{{name: "pipe", mode: os.ModeNamedPipe | 0o600}},
			write:   writeZipTestArchive,
		},
		{
			name: "zip traversal", format: "zip", ext: ".zip", want: "unsafe archive path",
			entries: []archiveTestEntry{{name: "../outside", data: "data", mode: 0o600}},
			write:   writeZipTestArchive,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(root, "input"+tt.ext)
			tt.write(t, archive, tt.entries)
			assertArchiveRejectedInDryRunAndApply(t, root, archive, tt.format, tt.want)
			if _, err := os.Lstat(outside); !os.IsNotExist(err) {
				t.Fatalf("outside path was created: %v", err)
			}
		})
	}
}

func TestArchiveExtractsSafeLeafSymlink(t *testing.T) {
	probeDir := t.TempDir()
	probeTarget := filepath.Join(probeDir, "target")
	if err := os.WriteFile(probeTarget, []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	probeLink := filepath.Join(probeDir, "link")
	if err := os.Symlink("target", probeLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	formats := []struct {
		name  string
		ext   string
		write func(*testing.T, string, []archiveTestEntry)
	}{
		{name: archiveFormatTar, ext: ".tar", write: writeTarTestArchive},
		{name: "zip", ext: ".zip", write: writeZipTestArchive},
	}
	for _, format := range formats {
		format := format
		t.Run(format.name, func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(root, "input"+format.ext)
			entries := []archiveTestEntry{
				{name: "target", data: "payload", typeflag: tar.TypeReg, mode: 0o600},
				{name: "link", data: "target", linkname: "target", typeflag: tar.TypeSymlink, mode: os.ModeSymlink | 0o777},
			}
			format.write(t, archive, entries)
			destination := filepath.Join(root, "out")
			plan := testPlan(root, Operation{ID: "extract", Action: ActionExtract, Source: archive, Destination: destination, Format: format.name})
			if _, err := Apply(context.Background(), plan, ApplyOptions{DryRun: true, DisableAudit: true}); err != nil {
				t.Fatalf("dry-run rejected safe archive: %v", err)
			}
			if _, err := Apply(context.Background(), plan, ApplyOptions{DisableAudit: true}); err != nil {
				t.Fatalf("apply rejected safe archive: %v", err)
			}
			target, err := os.Readlink(filepath.Join(destination, "link"))
			if err != nil || target != "target" {
				t.Fatalf("extracted symlink = %q, %v", target, err)
			}
			data, err := os.ReadFile(filepath.Join(destination, "target"))
			if err != nil || string(data) != "payload" {
				t.Fatalf("extracted target = %q, %v", data, err)
			}
		})
	}
}

func TestArchiveChangeAfterValidationCleansStaging(t *testing.T) {
	formats := []struct {
		name  string
		ext   string
		write func(*testing.T, string, []archiveTestEntry)
	}{
		{name: archiveFormatTar, ext: ".tar", write: writeTarTestArchive},
		{name: "zip", ext: ".zip", write: writeZipTestArchive},
	}
	for _, format := range formats {
		format := format
		t.Run(format.name, func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(root, "input"+format.ext)
			original := []archiveTestEntry{{name: "first", data: "one", typeflag: tar.TypeReg, mode: 0o600}}
			format.write(t, archive, original)
			layout, err := inspectArchive(context.Background(), archive, format.name)
			if err != nil {
				t.Fatal(err)
			}
			changed := append(original, archiveTestEntry{name: "second", data: "two", typeflag: tar.TypeReg, mode: 0o600})
			format.write(t, archive, changed)
			destination := filepath.Join(root, "out")
			if err := extractArchiveNew(context.Background(), archive, destination, format.name, layout); err == nil || !strings.Contains(err.Error(), "archive changed during extraction") {
				t.Fatalf("changed archive error = %v", err)
			}
			assertNoExtractionArtifacts(t, root, destination)
		})
	}
}

func assertArchiveRejectedInDryRunAndApply(t *testing.T, root, archive, format, want string) {
	t.Helper()
	for _, dryRun := range []bool{true, false} {
		destination := filepath.Join(root, "out-real")
		if dryRun {
			destination = filepath.Join(root, "out-dry")
		}
		plan := testPlan(root, Operation{ID: "extract", Action: ActionExtract, Source: archive, Destination: destination, Format: format})
		results, err := Apply(context.Background(), plan, ApplyOptions{DryRun: dryRun, DisableAudit: true})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("dry_run=%t results=%#v error=%v, want %q", dryRun, results, err, want)
		}
		if len(results) != 1 || results[0].Status != "error" {
			t.Fatalf("dry_run=%t results=%#v", dryRun, results)
		}
		assertNoExtractionArtifacts(t, root, destination)
	}
}

func assertNoExtractionArtifacts(t *testing.T, root, destination string) {
	t.Helper()
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("rejected extraction left destination: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".dirstat-extract-") {
			t.Fatalf("rejected extraction left staging path %q", entry.Name())
		}
	}
}

func writeTarTestArchive(t *testing.T, path string, entries []archiveTestEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.name, Linkname: entry.linkname, Typeflag: entry.typeflag,
			Mode: int64(entry.mode.Perm()),
		}
		if entry.typeflag == tar.TypeReg || entry.typeflag == 0 {
			header.Size = int64(len(entry.data))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := tw.Write([]byte(entry.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeZipTestArchive(t *testing.T, path string, entries []archiveTestEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		header.SetMode(entry.mode)
		writer, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		data := entry.data
		if entry.mode&os.ModeSymlink != 0 && data == "" {
			data = entry.linkname
		}
		if _, err := writer.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
