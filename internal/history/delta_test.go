package history

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/phillipod/go-dirstat/internal/index"
)

func TestCompareClassifiesPathChanges(t *testing.T) {
	before := deltaSnapshot([]index.FlatNode{
		{Name: "data", IsDir: true, Apparent: 60, Alloc: 80, Parent: -1},
		{Name: "grow", Depth: 1, Apparent: 10, Alloc: 20, Parent: 0},
		{Name: "shrink", Depth: 1, Apparent: 30, Alloc: 40, Parent: 0},
		{Name: "gone", Depth: 1, Apparent: 20, Alloc: 20, Parent: 0},
	})
	after := deltaSnapshot([]index.FlatNode{
		{Name: "data", IsDir: true, Apparent: 75, Alloc: 85, Parent: -1},
		{Name: "grow", Depth: 1, Apparent: 25, Alloc: 30, Parent: 0},
		{Name: "shrink", Depth: 1, Apparent: 10, Alloc: 10, Parent: 0},
		{Name: "new", Depth: 1, Apparent: 40, Alloc: 45, Parent: 0},
	})

	deltas, err := Compare(before, after)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]Delta, len(deltas))
	for _, delta := range deltas {
		got[delta.Path] = delta
	}
	root := before.Root
	want := map[string]Change{
		root:                          ChangeGrown,
		filepath.Join(root, "grow"):   ChangeGrown,
		filepath.Join(root, "shrink"): ChangeShrunk,
		filepath.Join(root, "gone"):   ChangeRemoved,
		filepath.Join(root, "new"):    ChangeNew,
	}
	if len(got) != len(want) {
		t.Fatalf("deltas = %#v", deltas)
	}
	for path, change := range want {
		if got[path].Change != change {
			t.Errorf("%s change = %q, want %q", path, got[path].Change, change)
		}
	}
	if got[root].Depth != 0 || got[filepath.Join(root, "grow")].Depth != 1 {
		t.Fatalf("delta depths: root=%d child=%d", got[root].Depth, got[filepath.Join(root, "grow")].Depth)
	}
	if got[filepath.Join(root, "shrink")].AllocatedDelta != -30 {
		t.Errorf("shrink delta = %d", got[filepath.Join(root, "shrink")].AllocatedDelta)
	}
	if got[filepath.Join(root, "gone")].AfterAlloc != 0 {
		t.Errorf("removed after = %d", got[filepath.Join(root, "gone")].AfterAlloc)
	}
}

func TestFilterDeltasControlsKindDepthLeafAndLimit(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv", "data")
	deltas := []Delta{
		{Path: root, IsDir: true},
		{Path: filepath.Join(root, "sub"), IsDir: true},
		{Path: filepath.Join(root, "sub", "file")},
		{Path: filepath.Join(root, "sibling")},
	}
	directories, err := FilterDeltas(deltas, root, DeltaFilter{Kind: DeltaKindDirectory, MaxDepth: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(directories) != 2 || !directories[0].IsDir || !directories[1].IsDir {
		t.Fatalf("directory deltas = %#v", directories)
	}
	leaves, err := FilterDeltas(deltas, root, DeltaFilter{Kind: DeltaKindAll, MaxDepth: -1, LeafOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 2 || leaves[0].Path != filepath.Join(root, "sub", "file") || leaves[1].Path != filepath.Join(root, "sibling") {
		t.Fatalf("leaf deltas = %#v", leaves)
	}
	shallow, err := FilterDeltas(deltas, root, DeltaFilter{Kind: DeltaKindAll, MaxDepth: 1, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(shallow) != 2 || shallow[0].Depth != 0 || shallow[1].Depth != 1 {
		t.Fatalf("shallow limited deltas = %#v", shallow)
	}
}

func TestFilterDeltasRejectsInvalidControlsAndEscapingPaths(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv", "data")
	tests := []struct {
		deltas []Delta
		filter DeltaFilter
	}{
		{filter: DeltaFilter{Kind: "socket", MaxDepth: -1}},
		{filter: DeltaFilter{Kind: DeltaKindAll, MaxDepth: -2}},
		{filter: DeltaFilter{Kind: DeltaKindAll, MaxDepth: -1, Limit: -1}},
		{deltas: []Delta{{Path: filepath.Dir(root)}}, filter: DeltaFilter{Kind: DeltaKindAll, MaxDepth: -1}},
	}
	for _, test := range tests {
		if _, err := FilterDeltas(test.deltas, root, test.filter); err == nil {
			t.Fatalf("FilterDeltas(%#v, %#v) unexpectedly succeeded", test.deltas, test.filter)
		}
	}
}

func TestCompareRejectsDifferentKeysAndMalformedLayout(t *testing.T) {
	one := deltaSnapshot([]index.FlatNode{{Name: "data", IsDir: true, Parent: -1}})
	two := deltaSnapshot([]index.FlatNode{{Name: "data", IsDir: true, Parent: -1}})
	two.Fingerprint = "other"
	if _, err := Compare(one, two); err == nil {
		t.Fatal("different fingerprint accepted")
	}
	two.Fingerprint = one.Fingerprint
	two.Nodes = append(two.Nodes, index.FlatNode{Name: "bad", Depth: 1, Parent: 9})
	if _, err := Compare(one, two); err == nil {
		t.Fatal("malformed layout accepted")
	}
}

func deltaSnapshot(nodes []index.FlatNode) *index.Snapshot {
	return &index.Snapshot{
		Root: filepath.Join(string(filepath.Separator), "srv", "data"), Fingerprint: "fp", ScannedAt: time.Now(),
		Nodes: nodes,
	}
}
