package query

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/tree"
)

func TestBuildFiltersSortsAndUsesPortablePaths(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	root := testTree(now)
	min := int64(10)
	records, err := Build(root, t.TempDir(), Options{
		Now: now,
		Filter: Filter{
			MinSize: &min, OlderThan: 24 * time.Hour,
			Extensions: []string{"LOG"}, Kinds: []Kind{KindFile},
			PathGlob: "logs/*.log", PathRegexp: `old`,
		},
		Sort: []SortKey{{Field: SortApparent, Desc: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records: %#v", len(records), records)
	}
	r := records[0]
	if r.Relative != "logs/old.log" || r.Name != "old.log" || r.Extension != ".log" {
		t.Fatalf("unexpected record: %#v", r)
	}
	if r.Path != filepath.Join(filepath.Dir(r.Path), "old.log") || !filepath.IsAbs(r.Path) {
		t.Fatalf("path is not native absolute path: %q", r.Path)
	}
}

func TestBuildMetadataIsOnDemandAndOwnershipMatchesNamesOrIDs(t *testing.T) {
	now := time.Now()
	root := testTree(now)
	var inspected []string
	inspect := func(path string, follow bool) (fsinfo.Entry, error) {
		if follow {
			t.Fatal("unexpected follow")
		}
		inspected = append(inspected, filepath.Base(path))
		if filepath.Base(path) == "new.txt" {
			return fsinfo.Entry{}, errors.New("vanished")
		}
		return fsinfo.Entry{
			UID: "1000", GID: "2000", Owner: "alice", Group: "staff",
			Mode: 0o100640, ModeText: "-rw-r-----", Links: 2,
			Identity: fsinfo.Identity{Device: 3, File: 4, Valid: true},
		}, nil
	}
	records, err := Build(root, t.TempDir(), Options{
		Now: now, Inspect: inspect,
		Filter: Filter{Extensions: []string{"log"}, Owners: []string{"1000"}, Groups: []string{"staff"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected) != 1 || inspected[0] != "old.log" {
		t.Fatalf("cheap filters should run before inspection: %v", inspected)
	}
	if len(records) != 1 || records[0].Owner != "alice" || !records[0].Identity.Valid {
		t.Fatalf("unexpected enriched records: %#v", records)
	}

	inspected = nil
	records, err = Build(root, t.TempDir(), Options{Now: now, Metadata: true, Inspect: inspect})
	if err != nil {
		t.Fatal(err)
	}
	if got := findRecord(records, "new.txt").MetadataError; got != "vanished" {
		t.Fatalf("metadata error not retained: %q", got)
	}
}

func TestBuildDefaultAndMultiKeySortingAreStable(t *testing.T) {
	now := time.Now()
	root := &tree.Node{Name: "root", IsDir: true}
	root.AddChild(&tree.Node{Name: "b", Apparent: 5, ModTime: now})
	root.AddChild(&tree.Node{Name: "a", Apparent: 5, ModTime: now})
	root.AddChild(&tree.Node{Name: "c", Apparent: 10, ModTime: now})
	records, err := Build(root, t.TempDir(), Options{Sort: []SortKey{
		{Field: SortApparent, Desc: true}, {Field: SortName},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := relativePaths(records); !reflect.DeepEqual(got, []string{"c", "a", "b", ""}) {
		t.Fatalf("multi-key sort = %v", got)
	}

	records, err = Build(root, t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := relativePaths(records); !reflect.DeepEqual(got, []string{"", "a", "b", "c"}) {
		t.Fatalf("default sort = %v", got)
	}
}

func TestBuildSizeFilterCanUseAllocatedBytes(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true}
	root.AddChild(&tree.Node{Name: "sparse", Apparent: 1000, Alloc: 0})
	root.AddChild(&tree.Node{Name: "allocated", Apparent: 1, Alloc: 512})
	min := int64(100)
	records, err := Build(root, t.TempDir(), Options{Filter: Filter{
		MinSize: &min, SizeMode: tree.SizeOnDisk, Kinds: []Kind{KindFile},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := relativePaths(records); !reflect.DeepEqual(got, []string{"allocated"}) {
		t.Fatalf("allocated size filter = %v", got)
	}
}

func TestBuildRejectsInvalidFiltersAndEscapingCandidates(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true}
	root.AddChild(&tree.Node{Name: "child"})
	negative := int64(-1)
	tests := []Options{
		{Filter: Filter{MinSize: &negative}},
		{Filter: Filter{OlderThan: -time.Second}},
		{Filter: Filter{Kinds: []Kind{"socket"}}},
		{Filter: Filter{PathGlob: "["}},
		{Filter: Filter{PathRegexp: "("}},
		{Sort: []SortKey{{Field: "bogus"}}},
	}
	for _, opts := range tests {
		if _, err := Build(root, t.TempDir(), opts); err == nil {
			t.Fatalf("Build(%#v) unexpectedly succeeded", opts)
		}
	}

	escape := &tree.Node{Name: "root", IsDir: true}
	escape.AddChild(&tree.Node{Name: ".."})
	if _, err := Build(escape, t.TempDir(), Options{}); err == nil {
		t.Fatal("escaping candidate unexpectedly accepted")
	}
}

func testTree(now time.Time) *tree.Node {
	root := &tree.Node{Name: "root", IsDir: true, Apparent: 35, Alloc: 48, FileCount: 2, DirCount: 1, ModTime: now}
	logs := &tree.Node{Name: "logs", IsDir: true, Apparent: 30, Alloc: 40, FileCount: 1, ModTime: now.Add(-48 * time.Hour)}
	logs.AddChild(&tree.Node{Name: "old.log", Apparent: 30, Alloc: 40, ModTime: now.Add(-48 * time.Hour)})
	root.AddChild(logs)
	root.AddChild(&tree.Node{Name: "new.txt", Apparent: 5, Alloc: 8, ModTime: now.Add(-time.Hour), Err: errors.New("scan warning")})
	return root
}

func relativePaths(records []Record) []string {
	result := make([]string, len(records))
	for i := range records {
		result[i] = records[i].Relative
	}
	return result
}

func findRecord(records []Record, name string) Record {
	for _, record := range records {
		if record.Name == name {
			return record
		}
	}
	return Record{}
}
