package query

import (
	"context"
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

func TestBuildCanFollowMetadataForFollowedScanAliases(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true}
	root.AddChild(&tree.Node{Name: "alias", Apparent: 7, Alloc: 8})
	var followed bool
	records, err := Build(root, t.TempDir(), Options{
		Metadata: true, FollowMetadata: true,
		Inspect: func(_ string, follow bool) (fsinfo.Entry, error) {
			followed = follow
			return fsinfo.Entry{Mode: 0o100600, ModeText: "-rw-------"}, nil
		},
		Filter: Filter{Kinds: []Kind{KindFile}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !followed || len(records) != 1 || records[0].ModeText != "-rw-------" {
		t.Fatalf("followed metadata records = %#v, followed=%t", records, followed)
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

func TestBuildLimitRetainsBestSortedRecords(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, FileCount: 3}
	root.AddChild(&tree.Node{Name: "small", Apparent: 1})
	root.AddChild(&tree.Node{Name: "largest", Apparent: 30})
	root.AddChild(&tree.Node{Name: "middle", Apparent: 20})
	records, err := Build(root, t.TempDir(), Options{
		Limit:  2,
		Filter: Filter{Kinds: []Kind{KindFile}},
		Sort:   []SortKey{{Field: SortApparent, Desc: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := relativePaths(records); !reflect.DeepEqual(got, []string{"largest", "middle"}) {
		t.Fatalf("bounded sorted records = %v", got)
	}
}

func TestStreamUsesTreeOrderHonorsLimitAndPropagatesVisitorError(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, FileCount: 2}
	root.AddChild(&tree.Node{Name: "b", Apparent: 1})
	root.AddChild(&tree.Node{Name: "a", Apparent: 2})
	var paths []string
	err := Stream(root, t.TempDir(), Options{Limit: 1, Filter: Filter{Kinds: []Kind{KindFile}}}, func(record Record) error {
		paths = append(paths, record.Relative)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"b"}) {
		t.Fatalf("streamed paths = %v, want deterministic tree order", paths)
	}

	want := errors.New("writer closed")
	err = Stream(root, t.TempDir(), Options{}, func(Record) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("stream error = %v, want %v", err, want)
	}
	if err := Stream(root, t.TempDir(), Options{}, nil); err == nil {
		t.Fatal("nil stream visitor was accepted")
	}
	if err := Stream(root, t.TempDir(), Options{Sort: []SortKey{{Field: SortPath}}}, func(Record) error { return nil }); err == nil {
		t.Fatal("stream sort key was silently ignored")
	}
}

func TestBuildAndStreamRejectNegativeLimit(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true}
	if _, err := Build(root, t.TempDir(), Options{Limit: -1}); err == nil {
		t.Fatal("Build accepted a negative limit")
	}
	if err := Stream(root, t.TempDir(), Options{Limit: -1}, func(Record) error { return nil }); err == nil {
		t.Fatal("Stream accepted a negative limit")
	}
}

func TestBuildAndStreamContextHonorCancellation(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, FileCount: 2}
	root.AddChild(&tree.Node{Name: "first"})
	root.AddChild(&tree.Node{Name: "second"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildContext(ctx, root, t.TempDir(), Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled BuildContext() error = %v, want context.Canceled", err)
	}
	if err := StreamContext(ctx, root, t.TempDir(), Options{}, func(Record) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled StreamContext() error = %v, want context.Canceled", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	visits := 0
	err := StreamContext(ctx, root, t.TempDir(), Options{}, func(Record) error {
		visits++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || visits != 1 {
		t.Fatalf("canceled StreamContext() visits=%d error=%v, want one visit and context.Canceled", visits, err)
	}
}

func TestBuildLimitMatchesStableUnlimitedPrefixWithTies(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, FileCount: 6}
	for _, name := range []string{"z-first", "a", "z-second", "b", "z-third", "c"} {
		root.AddChild(&tree.Node{Name: name, Apparent: 10})
	}
	opts := Options{
		Filter: Filter{Kinds: []Kind{KindFile}},
		Sort:   []SortKey{{Field: SortApparent, Desc: true}},
	}
	all, err := Build(root, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.Limit = 3
	limited, err := Build(root, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := relativePaths(limited), relativePaths(all[:3]); !reflect.DeepEqual(got, want) {
		t.Fatalf("bounded tied prefix = %v, want stable unlimited prefix %v", got, want)
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

func TestBuildClassifiesLeadingDotfileWithoutExtension(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true}
	root.AddChild(&tree.Node{Name: ".env", Apparent: 3, Alloc: 8})
	records, err := Build(root, t.TempDir(), Options{Filter: Filter{Kinds: []Kind{KindFile}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Name != ".env" || records[0].Extension != "" {
		t.Fatalf("dotfile record = %#v", records)
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
