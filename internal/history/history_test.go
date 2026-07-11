package history

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/phillipod/go-dirstat/internal/index"
)

func TestRecordListLoadAndPrevious(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base.Add(time.Hour) }
	one := testSnapshot(base, 10)
	two := testSnapshot(base.Add(time.Minute), 20)
	if _, err := store.RecordSnapshot(one); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordSnapshot(two); err != nil {
		t.Fatal(err)
	}

	records, err := store.List(one.Root, one.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || !records[0].ScannedAt.Equal(two.ScannedAt) {
		t.Fatalf("records = %#v", records)
	}
	loaded, err := store.Load(records[1])
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Nodes[0].Apparent != 10 {
		t.Fatalf("loaded apparent = %d", loaded.Nodes[0].Apparent)
	}
	previous, err := store.Previous(one.Root, one.Fingerprint, two.ScannedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !previous.ScannedAt.Equal(one.ScannedAt) {
		t.Fatalf("previous scan = %v, want %v", previous.ScannedAt, one.ScannedAt)
	}
}

func TestRetentionKeepsNewestTwentyWithinThirtyDays(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	old := testSnapshot(now.Add(-MaxAge-time.Hour), 1)
	if _, err := store.RecordSnapshot(old); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		snap := testSnapshot(now.Add(-time.Duration(25-i)*time.Minute), int64(i+2))
		if _, err := store.RecordSnapshot(snap); err != nil {
			t.Fatal(err)
		}
	}
	records, err := store.List(old.Root, old.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != MaxRecords {
		t.Fatalf("retained = %d, want %d", len(records), MaxRecords)
	}
	if records[len(records)-1].ScannedAt.Before(now.Add(-20 * time.Minute)) {
		t.Fatalf("old record retained: %v", records[len(records)-1].ScannedAt)
	}
}

func TestLoadRejectsTraversalID(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Load(Record{ID: "../escape", Root: "/tmp/root", Fingerprint: "fp"})
	if err == nil {
		t.Fatal("traversal record was accepted")
	}
}

func TestRecordRejectsInvalidSnapshot(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap := testSnapshot(time.Now(), 10)
	snap.Nodes[1].Parent = 99
	if _, err := store.RecordSnapshot(snap); err == nil {
		t.Fatal("invalid snapshot was recorded")
	}
}

func TestRecordIsIdempotentAndRejectsTimestampCollision(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap := testSnapshot(time.Now(), 10)
	if _, err := store.RecordSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordSnapshot(snap); err != nil {
		t.Fatalf("recording same snapshot twice: %v", err)
	}
	collision := testSnapshot(snap.ScannedAt, 11)
	if _, err := store.RecordSnapshot(collision); err == nil {
		t.Fatal("different snapshot with same timestamp was accepted")
	}
}

func TestPreviousMissing(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Previous("/missing", "fp", time.Time{})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want fs.ErrNotExist", err)
	}
}

func TestRecordsArePrivate(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.RecordSnapshot(testSnapshot(time.Now(), 10))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.keyDir(record.Root, record.Fingerprint), record.ID+".bin")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %o, want 600", info.Mode().Perm())
	}
}

func testSnapshot(at time.Time, apparent int64) *index.Snapshot {
	return &index.Snapshot{
		Root: "/srv/data", Fingerprint: "scope-fp", ScannedAt: at,
		Files: 1, Dirs: 1,
		Nodes: []index.FlatNode{
			{Name: "data", IsDir: true, Apparent: apparent, Alloc: apparent, FileCount: 1, Parent: -1},
			{Name: "file", Depth: 1, Apparent: apparent, Alloc: apparent, Parent: 0},
		},
	}
}
