package history

import (
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
	want := map[string]Change{
		"/srv/data":        ChangeGrown,
		"/srv/data/grow":   ChangeGrown,
		"/srv/data/shrink": ChangeShrunk,
		"/srv/data/gone":   ChangeRemoved,
		"/srv/data/new":    ChangeNew,
	}
	if len(got) != len(want) {
		t.Fatalf("deltas = %#v", deltas)
	}
	for path, change := range want {
		if got[path].Change != change {
			t.Errorf("%s change = %q, want %q", path, got[path].Change, change)
		}
	}
	if got["/srv/data/shrink"].AllocatedDelta != -30 {
		t.Errorf("shrink delta = %d", got["/srv/data/shrink"].AllocatedDelta)
	}
	if got["/srv/data/gone"].AfterAlloc != 0 {
		t.Errorf("removed after = %d", got["/srv/data/gone"].AfterAlloc)
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
		Root: "/srv/data", Fingerprint: "fp", ScannedAt: time.Now(),
		Nodes: nodes,
	}
}
