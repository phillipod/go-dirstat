package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

func TestSnapshotRoundTripPreservesTotals(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), 100)
	write(t, filepath.Join(root, "b.bin"), 2000)
	write(t, filepath.Join(root, "sub", "c.md"), 50)

	node, stats, err := scan.Scan(context.Background(), root, scan.WithPolicy(scope.New()))
	if err != nil {
		t.Fatal(err)
	}
	fp := Fingerprint(root, scope.New())

	snap := FromTree(node, fp, stats.RootFS, stats.Files, stats.Dirs, stats.Errors, time.Now())
	data, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(data, fp)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := got.ToTree()
	if rebuilt.Apparent != node.Apparent || rebuilt.Alloc != node.Alloc {
		t.Errorf("rebuilt sizes = %d/%d, want %d/%d", rebuilt.Apparent, rebuilt.Alloc, node.Apparent, node.Alloc)
	}
	if rebuilt.FileCount != node.FileCount || rebuilt.DirCount != node.DirCount {
		t.Errorf("rebuilt counts = %d/%d, want %d/%d", rebuilt.FileCount, rebuilt.DirCount, node.FileCount, node.DirCount)
	}
	if got.Files != stats.Files {
		t.Errorf("snap.Files = %d, want %d", got.Files, stats.Files)
	}
}

func TestFingerprintChangesWithOptions(t *testing.T) {
	root := "/some/path"
	p1 := scope.New()
	p2 := scope.New(scope.WithCrossDevice(true))
	if Fingerprint(root, p1) == Fingerprint(root, p2) {
		t.Error("fingerprint should differ when cross-device changes")
	}
	if Fingerprint(root, p1) != Fingerprint(root, scope.New()) {
		t.Error("identical options should produce identical fingerprint")
	}
}

func TestUnmarshalRejectsWrongFingerprint(t *testing.T) {
	snap := FromTree(&tree.Node{Name: "x"}, "aaaa", "", 0, 0, 0, time.Now())
	data, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unmarshal(data, "bbbb"); err == nil {
		t.Error("expected ErrIncompatible for mismatched fingerprint")
	}
}

func TestSnapshotRejectsInvalidParentIndexes(t *testing.T) {
	snap := &Snapshot{
		Fingerprint: "scope-fingerprint",
		Nodes: []FlatNode{
			{Name: "root", IsDir: true, Parent: -1},
			{Name: "child", Parent: 99},
		},
	}
	if got := snap.ToTree(); got != nil {
		t.Fatalf("ToTree() = %#v, want nil for an invalid parent index", got)
	}

	data, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unmarshal(data, snap.Fingerprint); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Unmarshal() error = %v, want ErrIncompatible", err)
	}
}

func TestSnapshotRejectsInvalidNodeStructure(t *testing.T) {
	tests := map[string][]FlatNode{
		"nonzero root depth": {
			{Name: "root", IsDir: true, Depth: 1, Parent: -1},
		},
		"file parent": {
			{Name: "root", Depth: 0, Parent: -1},
			{Name: "child", Depth: 1, Parent: 0},
		},
		"wrong child depth": {
			{Name: "root", IsDir: true, Depth: 0, Parent: -1},
			{Name: "child", Depth: 2, Parent: 0},
		},
		"negative size": {
			{Name: "root", IsDir: true, Depth: 0, Apparent: -1, Parent: -1},
		},
	}
	for name, nodes := range tests {
		t.Run(name, func(t *testing.T) {
			snap := &Snapshot{Fingerprint: "scope-fingerprint", Nodes: nodes}
			data, err := snap.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Unmarshal(data, snap.Fingerprint); !errors.Is(err, ErrIncompatible) {
				t.Fatalf("Unmarshal() error = %v, want ErrIncompatible", err)
			}
		})
	}
}

func TestSnapshotRejectsNegativeSummaryCounters(t *testing.T) {
	snap := &Snapshot{
		Fingerprint: "scope-fingerprint",
		Files:       -1,
		Nodes:       []FlatNode{{Name: "root", IsDir: true, Parent: -1}},
	}
	data, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unmarshal(data, snap.Fingerprint); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Unmarshal() error = %v, want ErrIncompatible", err)
	}
}

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, size)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
