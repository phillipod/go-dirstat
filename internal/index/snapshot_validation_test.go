package index

import (
	"bytes"
	"encoding/gob"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestSnapshotValidationRejectsInvalidRootAndNames(t *testing.T) {
	tests := map[string]func(*Snapshot){
		"empty root path": func(s *Snapshot) {
			s.Root = ""
		},
		"relative root path": func(s *Snapshot) {
			s.Root = filepath.Base(s.Root)
		},
		"empty fingerprint": func(s *Snapshot) {
			s.Fingerprint = ""
		},
		"whitespace fingerprint": func(s *Snapshot) {
			s.Fingerprint = " \t"
		},
		"zero scan time": func(s *Snapshot) {
			s.ScannedAt = time.Time{}
		},
		"missing nodes": func(s *Snapshot) {
			s.Nodes = nil
		},
		"root name differs from path basename": func(s *Snapshot) {
			s.Nodes[0].Name = "another-root"
		},
		"root has parent": func(s *Snapshot) {
			s.Nodes[0].Parent = 1
		},
		"root has nonzero depth": func(s *Snapshot) {
			s.Nodes[0].Depth = 1
		},
		"empty child name": func(s *Snapshot) {
			s.Nodes[2].Name = ""
		},
		"dot child name": func(s *Snapshot) {
			s.Nodes[2].Name = "."
		},
		"dot-dot child name": func(s *Snapshot) {
			s.Nodes[2].Name = ".."
		},
		"slash in child name": func(s *Snapshot) {
			s.Nodes[2].Name = "dir/file"
		},
		"backslash in child name": func(s *Snapshot) {
			s.Nodes[2].Name = `dir\file`
		},
		"nul in child name": func(s *Snapshot) {
			s.Nodes[2].Name = "file\x00name"
		},
		"child has negative parent": func(s *Snapshot) {
			s.Nodes[2].Parent = -1
		},
		"child parent is not earlier": func(s *Snapshot) {
			s.Nodes[2].Parent = 2
		},
		"child parent is a file": func(s *Snapshot) {
			s.Nodes[3].Parent = 2
		},
		"child depth skips a level": func(s *Snapshot) {
			s.Nodes[3].Depth = 3
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			snap := validationDirectorySnapshot()
			mutate(snap)
			assertSnapshotRejected(t, snap)
		})
	}
}

func TestSnapshotValidationRejectsDuplicateSiblingNames(t *testing.T) {
	snap := validationDirectorySnapshot()
	snap.Nodes[2].Name = snap.Nodes[1].Name
	assertSnapshotRejected(t, snap)
}

func TestSnapshotValidationUsesPlatformSiblingCaseRules(t *testing.T) {
	snap := validationDirectorySnapshot()
	snap.Nodes[1].Name = "DATA"
	snap.Nodes[2].Name = "data"

	data := mustMarshalSnapshot(t, snap)
	got, err := Unmarshal(data, snap.Fingerprint)
	if runtime.GOOS == windowsOS {
		if !errors.Is(err, ErrIncompatible) {
			t.Fatalf("Unmarshal() error = %v, want ErrIncompatible for case-equivalent Windows siblings", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("Unmarshal() error = %v, want case-distinct siblings to be valid on %s", err, runtime.GOOS)
	}
	if got == nil {
		t.Fatal("Unmarshal() returned a nil snapshot")
	}
}

func TestSnapshotValidationRejectsAggregateAndCounterInconsistencies(t *testing.T) {
	maxInt64 := int64(^uint64(0) >> 1)
	tests := map[string]func(*Snapshot){
		"negative files summary": func(s *Snapshot) {
			s.Files = -1
		},
		"negative directories summary": func(s *Snapshot) {
			s.Dirs = -1
		},
		"negative errors summary": func(s *Snapshot) {
			s.Errors = -1
		},
		"files summary mismatch": func(s *Snapshot) {
			s.Files--
		},
		"directories summary mismatch": func(s *Snapshot) {
			s.Dirs--
		},
		"errors summary mismatch": func(s *Snapshot) {
			s.Errors++
		},
		"root apparent bytes mismatch": func(s *Snapshot) {
			s.Nodes[0].Apparent++
		},
		"root allocated bytes below children": func(s *Snapshot) {
			s.Nodes[0].Alloc--
		},
		"root file count mismatch": func(s *Snapshot) {
			s.Nodes[0].FileCount--
		},
		"root directory count mismatch": func(s *Snapshot) {
			s.Nodes[0].DirCount--
		},
		"nested directory apparent bytes mismatch": func(s *Snapshot) {
			s.Nodes[1].Apparent++
		},
		"nested directory allocated bytes below children": func(s *Snapshot) {
			s.Nodes[1].Alloc--
		},
		"nested directory file count mismatch": func(s *Snapshot) {
			s.Nodes[1].FileCount++
		},
		"nested directory directory count mismatch": func(s *Snapshot) {
			s.Nodes[1].DirCount++
		},
		"file carries file count": func(s *Snapshot) {
			s.Nodes[2].FileCount = 1
		},
		"file carries directory count": func(s *Snapshot) {
			s.Nodes[2].DirCount = 1
		},
		"negative apparent bytes": func(s *Snapshot) {
			s.Nodes[2].Apparent = -1
		},
		"negative allocated bytes": func(s *Snapshot) {
			s.Nodes[2].Alloc = -1
		},
		"negative file count": func(s *Snapshot) {
			s.Nodes[2].FileCount = -1
		},
		"negative directory count": func(s *Snapshot) {
			s.Nodes[2].DirCount = -1
		},
		"negative depth": func(s *Snapshot) {
			s.Nodes[2].Depth = -1
		},
		"root modtime older than child": func(s *Snapshot) {
			s.Nodes[0].ModTime = s.Nodes[1].ModTime.Add(-time.Nanosecond)
		},
		"nested directory modtime older than child": func(s *Snapshot) {
			s.Nodes[1].ModTime = s.Nodes[3].ModTime.Add(-time.Nanosecond)
		},
		"apparent byte aggregation overflows": func(s *Snapshot) {
			s.Nodes[0].Apparent = maxInt64
			s.Nodes[0].Alloc = 0
			s.Nodes[1].Apparent = maxInt64
			s.Nodes[1].Alloc = 0
			s.Nodes[2].Apparent = 1
			s.Nodes[2].Alloc = 0
			s.Nodes[3].Apparent = maxInt64
			s.Nodes[3].Alloc = 0
		},
		"allocated byte aggregation overflows": func(s *Snapshot) {
			s.Nodes[0].Apparent = 0
			s.Nodes[0].Alloc = maxInt64
			s.Nodes[1].Apparent = 0
			s.Nodes[1].Alloc = maxInt64
			s.Nodes[2].Apparent = 0
			s.Nodes[2].Alloc = 1
			s.Nodes[3].Apparent = 0
			s.Nodes[3].Alloc = maxInt64
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			snap := validationDirectorySnapshot()
			mutate(snap)
			assertSnapshotRejected(t, snap)
		})
	}
}

func TestSnapshotValidationCompletenessAndErrors(t *testing.T) {
	t.Run("complete snapshot cannot contain node errors", func(t *testing.T) {
		snap := validationSnapshotWithError()
		snap.Complete = true
		assertSnapshotRejected(t, snap)
	})

	t.Run("error summary must match node errors", func(t *testing.T) {
		snap := validationSnapshotWithError()
		snap.Errors = 0
		assertSnapshotRejected(t, snap)
	})

	t.Run("errored file is not counted as a measured file", func(t *testing.T) {
		snap := validationSnapshotWithError()
		snap.Files++
		snap.Nodes[0].FileCount++
		assertSnapshotRejected(t, snap)
	})

	t.Run("incomplete snapshot with an error is structurally valid", func(t *testing.T) {
		assertSnapshotAccepted(t, validationSnapshotWithError())
	})

	t.Run("incomplete snapshot need not invent an error", func(t *testing.T) {
		snap := validationDirectorySnapshot()
		snap.Complete = false
		assertSnapshotAccepted(t, snap)
	})
}

func TestSnapshotValidationHardlinkConstraints(t *testing.T) {
	t.Run("zero-size file hardlink is valid", func(t *testing.T) {
		snap := validationDirectorySnapshot()
		makeDirectFileHardlink(snap)
		assertSnapshotAccepted(t, snap)
	})

	tests := map[string]func(*Snapshot){
		"root hardlink": func(s *Snapshot) {
			s.Nodes[0].Hardlink = true
		},
		"directory hardlink": func(s *Snapshot) {
			s.Nodes[1].Hardlink = true
		},
		"hardlink with apparent bytes": func(s *Snapshot) {
			makeDirectFileHardlink(s)
			s.Nodes[2].Apparent = 1
			s.Nodes[0].Apparent++
		},
		"hardlink with allocated bytes": func(s *Snapshot) {
			makeDirectFileHardlink(s)
			s.Nodes[2].Alloc = 1
			s.Nodes[0].Alloc++
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			snap := validationDirectorySnapshot()
			mutate(snap)
			assertSnapshotRejected(t, snap)
		})
	}
}

func TestSnapshotValidationRejectsTrailingGobValueAndBytes(t *testing.T) {
	snap := validationDirectorySnapshot()

	t.Run("second gob value", func(t *testing.T) {
		var buf bytes.Buffer
		_, _ = buf.Write(magicVersion)
		_ = buf.WriteByte(formatVersion)
		encoder := gob.NewEncoder(&buf)
		if err := encoder.Encode(snap); err != nil {
			t.Fatal(err)
		}
		if err := encoder.Encode(struct{}{}); err != nil {
			t.Fatal(err)
		}
		if _, err := Unmarshal(buf.Bytes(), snap.Fingerprint); !errors.Is(err, ErrIncompatible) {
			t.Fatalf("Unmarshal() error = %v, want ErrIncompatible", err)
		}
	})

	for name, suffix := range map[string][]byte{
		"single byte": {0xff},
		"text":        []byte("trailing"),
	} {
		t.Run(name, func(t *testing.T) {
			data := append(mustMarshalSnapshot(t, snap), suffix...)
			if _, err := Unmarshal(data, snap.Fingerprint); !errors.Is(err, ErrIncompatible) {
				t.Fatalf("Unmarshal() error = %v, want ErrIncompatible", err)
			}
		})
	}
}

func TestSnapshotValidationAcceptsSingleFileRoundTrip(t *testing.T) {
	at := time.Unix(1_700_000_100, 0).UTC()
	root := filepath.Join(os.TempDir(), "dirstat-single-file.bin")
	snap := &Snapshot{
		Root:        root,
		Fingerprint: "single-file-fingerprint",
		ScannedAt:   at,
		Files:       1,
		Complete:    true,
		Nodes: []FlatNode{{
			Name: filepath.Base(root), Apparent: 17, Alloc: 512,
			ModTime: at.Add(-time.Minute), Parent: -1,
		}},
	}

	got := assertSnapshotAccepted(t, snap)
	if tree := got.ToTree(); tree == nil || tree.IsDir || tree.Name != filepath.Base(root) {
		t.Fatalf("ToTree() = %#v, want the single file root", tree)
	}
}

func TestSnapshotValidationAcceptsFilesystemRootRoundTrip(t *testing.T) {
	root := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	if !filepath.IsAbs(root) {
		t.Skipf("cannot derive an absolute filesystem root from %q", os.TempDir())
	}
	snap := &Snapshot{
		Root:        root,
		Fingerprint: "filesystem-root-fingerprint",
		ScannedAt:   time.Unix(1_700_000_200, 0).UTC(),
		Dirs:        1,
		Complete:    true,
		Nodes: []FlatNode{{
			Name: filepath.Base(root), IsDir: true, Parent: -1,
		}},
	}

	got := assertSnapshotAccepted(t, snap)
	if tree := got.ToTree(); tree == nil || !tree.IsDir || tree.Name != filepath.Base(root) {
		t.Fatalf("ToTree() = %#v, want the filesystem root directory", tree)
	}
}

func TestSnapshotValidationBroadTreeRegression(t *testing.T) {
	const files = 25_000
	snap := broadValidationSnapshot(files)

	tree := snap.ToTree()
	if tree == nil {
		t.Fatal("ToTree() rejected a valid broad snapshot")
	}
	if got := len(tree.Children); got != files {
		t.Fatalf("ToTree() root children = %d, want %d", got, files)
	}
	if tree.FileCount != files {
		t.Fatalf("ToTree() root file count = %d, want %d", tree.FileCount, files)
	}
}

func BenchmarkSnapshotValidationBroad(b *testing.B) {
	for _, files := range []int{1_000, 10_000, 50_000} {
		b.Run(strconv.Itoa(files), func(b *testing.B) {
			snap := broadValidationSnapshot(files)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := snap.ToTree(); got == nil {
					b.Fatal("ToTree() rejected a valid broad snapshot")
				}
			}
		})
	}
}

func validationDirectorySnapshot() *Snapshot {
	root := filepath.Join(os.TempDir(), "dirstat-snapshot-validation-root")
	old := time.Unix(1_700_000_000, 0).UTC()
	newer := old.Add(time.Minute)
	return &Snapshot{
		Root:        root,
		Fingerprint: "snapshot-validation-fingerprint",
		ScannedAt:   newer.Add(time.Minute),
		RootFS:      "testfs",
		Files:       2,
		Dirs:        2,
		Complete:    true,
		Nodes: []FlatNode{
			{Name: filepath.Base(root), IsDir: true, Apparent: 30, Alloc: 40, FileCount: 2, DirCount: 1, ModTime: newer, Parent: -1},
			{Name: "sub", IsDir: true, Depth: 1, Apparent: 20, Alloc: 24, FileCount: 1, ModTime: newer, Parent: 0},
			{Name: "top.dat", Depth: 1, Apparent: 10, Alloc: 16, ModTime: old, Parent: 0},
			{Name: "nested.dat", Depth: 2, Apparent: 20, Alloc: 24, ModTime: newer, Parent: 1},
		},
	}
}

func validationSnapshotWithError() *Snapshot {
	snap := validationDirectorySnapshot()
	snap.Complete = false
	snap.Errors = 1
	snap.Files = 1
	snap.Nodes[0].FileCount = 1
	snap.Nodes[0].Apparent = snap.Nodes[1].Apparent
	snap.Nodes[0].Alloc = snap.Nodes[1].Alloc
	snap.Nodes[2].Apparent = 0
	snap.Nodes[2].Alloc = 0
	snap.Nodes[2].ErrMsg = "permission denied"
	return snap
}

func makeDirectFileHardlink(snap *Snapshot) {
	snap.Nodes[0].Apparent -= snap.Nodes[2].Apparent
	snap.Nodes[0].Alloc -= snap.Nodes[2].Alloc
	snap.Nodes[2].Apparent = 0
	snap.Nodes[2].Alloc = 0
	snap.Nodes[2].Hardlink = true
}

func broadValidationSnapshot(files int) *Snapshot {
	root := filepath.Join(os.TempDir(), "dirstat-broad-validation-root")
	at := time.Unix(1_700_000_300, 0).UTC()
	nodes := make([]FlatNode, 2*files+1)
	nodes[0] = FlatNode{
		Name: filepath.Base(root), IsDir: true, Apparent: int64(files), Alloc: int64(files),
		FileCount: files, DirCount: files, ModTime: at, Parent: -1,
	}
	for i := 0; i < files; i++ {
		nodes[i+1] = FlatNode{
			Name: "dir-" + strconv.Itoa(i), IsDir: true, Depth: 1, Apparent: 1, Alloc: 1,
			FileCount: 1, ModTime: at, Parent: 0,
		}
		nodes[files+i+1] = FlatNode{
			Name: "file", Depth: 2, Apparent: 1, Alloc: 1,
			ModTime: at, Parent: i + 1,
		}
	}
	return &Snapshot{
		Root: root, Fingerprint: "broad-validation-fingerprint", ScannedAt: at,
		Files: files, Dirs: files + 1, Complete: true, Nodes: nodes,
	}
}

func assertSnapshotRejected(t *testing.T, snap *Snapshot) {
	t.Helper()
	data := mustMarshalSnapshot(t, snap)
	if _, err := Unmarshal(data, snap.Fingerprint); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Unmarshal() error = %v, want ErrIncompatible", err)
	}
}

func assertSnapshotAccepted(t *testing.T, snap *Snapshot) *Snapshot {
	t.Helper()
	data := mustMarshalSnapshot(t, snap)
	got, err := Unmarshal(data, snap.Fingerprint)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got == nil {
		t.Fatal("Unmarshal() returned a nil snapshot")
	}
	return got
}

func mustMarshalSnapshot(t *testing.T, snap *Snapshot) []byte {
	t.Helper()
	data, err := snap.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return data
}
