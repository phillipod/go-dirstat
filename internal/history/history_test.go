package history

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestOpenStoreDoesNotCreateState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing", "history")
	store, err := OpenStoreAtWithLimit(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if records, err := store.List(historyTestRoot(), "scope-fp"); err != nil || len(records) != 0 {
		t.Fatalf("missing store list = %#v, %v", records, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("OpenStoreAtWithLimit created state: %v", err)
	}
}

func TestRetentionKeepsNewestTwentyWithinThirtyDays(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	old := testSnapshot(now.Add(-MaxAge-time.Hour), 1)
	store.now = func() time.Time { return old.ScannedAt }
	if _, err := store.RecordSnapshot(old); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
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

func TestConfiguredRetentionKeepsRequestedRecordCount(t *testing.T) {
	store, err := NewStoreAtWithLimit(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now.Add(3 * time.Minute) }
	for i := 0; i < 3; i++ {
		if _, err := store.RecordSnapshot(testSnapshot(now.Add(time.Duration(i)*time.Minute), int64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	records, err := store.List(historyTestRoot(), "scope-fp")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !records[0].ScannedAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("configured retention records = %#v", records)
	}
}

func TestLargeConfiguredRetentionStillPrunesExpiredRecords(t *testing.T) {
	store, err := NewStoreAtWithLimit(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	timestamps := []time.Time{
		now.Add(-MaxAge - time.Second),
		now.Add(-MaxAge),
		now.Add(-time.Minute),
	}
	for i, at := range timestamps {
		if i == 0 {
			store.now = func() time.Time { return at }
		} else {
			store.now = func() time.Time { return now }
		}
		if _, err := store.RecordSnapshot(testSnapshot(at, int64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	records, err := store.List(historyTestRoot(), "scope-fp")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("large retention kept %d records, want two unexpired records: %#v", len(records), records)
	}
	if !records[1].ScannedAt.Equal(now.Add(-MaxAge)) {
		t.Fatalf("record exactly at age cutoff was pruned: %#v", records)
	}
}

func TestStoreRejectsInvalidRetention(t *testing.T) {
	if _, err := NewStoreAtWithLimit(t.TempDir(), 0); err == nil {
		t.Fatal("zero retention was accepted")
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

func TestRecordRejectsIncompleteSnapshot(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap := testSnapshot(time.Now(), 10)
	snap.Errors = 1
	if _, err := store.RecordSnapshot(snap); err == nil || !strings.Contains(err.Error(), "incomplete snapshot") {
		t.Fatalf("error = %v, want incomplete-snapshot rejection", err)
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

func TestNewStoreUsesDurableStateDirectory(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	store, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(state, "dirstat", "history")
	if store.Dir() != want {
		t.Fatalf("store directory = %q, want %q", store.Dir(), want)
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

func TestClearRemovesLastHistoryKeyDirectory(t *testing.T) {
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.RecordSnapshot(testSnapshot(time.Now(), 10))
	if err != nil {
		t.Fatal(err)
	}
	keyDir := store.keyDir(record.Root, record.Fingerprint)
	actions, err := store.Clear(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || !actions[0].Removed {
		t.Fatalf("clear actions = %#v", actions)
	}
	if _, err := os.Lstat(keyDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("last history key directory remains: %v", err)
	}
}

func TestLoadRejectsExpiredRecord(t *testing.T) {
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	store, err := NewStoreAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return base }
	record, err := store.RecordSnapshot(testSnapshot(base, 10))
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return base.Add(MaxAge + time.Second) }
	if _, err := store.Load(record); !errors.Is(err, index.ErrStale) {
		t.Fatalf("expired Load() error = %v, want index.ErrStale", err)
	}
}

func TestMigrationPayloadCollisionPreflightPreservesEntireSource(t *testing.T) {
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	source, err := NewStoreAt(filepath.Join(t.TempDir(), "source"))
	if err != nil {
		t.Fatal(err)
	}
	destination, err := NewStoreAt(filepath.Join(t.TempDir(), "destination"))
	if err != nil {
		t.Fatal(err)
	}
	now := base.Add(time.Hour)
	source.now = func() time.Time { return now }
	destination.now = source.now
	unique := testSnapshot(base, 10)
	collidingSource := testSnapshot(base.Add(time.Second), 20)
	if _, err := source.RecordSnapshot(unique); err != nil {
		t.Fatal(err)
	}
	if _, err := source.RecordSnapshot(collidingSource); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.RecordSnapshot(testSnapshot(collidingSource.ScannedAt, 999)); err != nil {
		t.Fatal(err)
	}

	actions, err := source.MigrateTo(destination, false)
	if err == nil || len(actions) != 0 || !strings.Contains(err.Error(), "different record") {
		t.Fatalf("migration actions=%#v error=%v, want whole-batch collision rejection", actions, err)
	}
	sourceRecords, listErr := source.List(unique.Root, unique.Fingerprint)
	if listErr != nil || len(sourceRecords) != 2 {
		t.Fatalf("source changed after collision preflight: records=%#v error=%v", sourceRecords, listErr)
	}
	destinationRecords, listErr := destination.List(unique.Root, unique.Fingerprint)
	if listErr != nil || len(destinationRecords) != 1 || destinationRecords[0].Apparent != 999 {
		t.Fatalf("destination changed after collision preflight: records=%#v error=%v", destinationRecords, listErr)
	}
}

func TestMigrationBatchRejectsCollectivePerKeyRetentionLoss(t *testing.T) {
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	source, err := NewStoreAtWithLimit(filepath.Join(t.TempDir(), "source"), MaxRecords)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := NewStoreAtWithLimit(filepath.Join(t.TempDir(), "destination"), MaxRecords)
	if err != nil {
		t.Fatal(err)
	}
	source.now = func() time.Time { return base.Add(time.Hour) }
	destination.now = source.now
	for i := 0; i < 2; i++ {
		if _, err := source.RecordSnapshot(testSnapshot(base.Add(time.Duration(i)*time.Second), int64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < MaxRecords-1; i++ {
		if _, err := destination.RecordSnapshot(testSnapshot(base.Add(time.Duration(i+2)*time.Second), int64(i+3))); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(source.Dir(), ".dirstat-store")); err != nil {
		t.Fatal(err)
	}
	legacy, err := OpenStoreAtWithPolicy(source.Dir(), MaxRecords, source.Policy())
	if err != nil {
		t.Fatal(err)
	}
	if actions, err := legacy.MigrateTo(destination, true); err == nil || len(actions) != 0 || !strings.Contains(err.Error(), "retention") {
		t.Fatalf("migration dry-run actions=%#v error=%v, want collective retention rejection", actions, err)
	}
	if records, err := legacy.List(testSnapshot(base, 1).Root, testSnapshot(base, 1).Fingerprint); err != nil || len(records) != 0 {
		// Unowned legacy state is deliberately unavailable through trusted reads;
		// verify the source payload count directly below.
		matches, globErr := filepath.Glob(filepath.Join(source.Dir(), "*", "*.bin"))
		if globErr != nil || len(matches) != 2 {
			t.Fatalf("source records changed after dry-run: records=%#v error=%v matches=%v glob_error=%v", records, err, matches, globErr)
		}
	}
	destinationRecords, err := destination.List(historyTestRoot(), "scope-fp")
	if err != nil || len(destinationRecords) != MaxRecords-1 {
		t.Fatalf("destination records changed after dry-run: count=%d error=%v", len(destinationRecords), err)
	}
}

func TestGlobalQuotaKeepsNewestRecordForEachHistoryKey(t *testing.T) {
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	a1 := testSnapshot(base, 10)
	a1.Fingerprint = "scope-a"
	b1 := testSnapshot(base.Add(time.Second), 20)
	b1.Fingerprint = "scope-b"
	a2 := testSnapshot(base.Add(2*time.Second), 30)
	a2.Fingerprint = "scope-a"
	a2Data, err := a2.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	b1Data, err := b1.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStoreAtWithPolicy(t.TempDir(), MaxRecords, Policy{
		MaxBytes: int64(len(a2Data) + len(b1Data)),
		MaxAge:   MaxAge,
	})
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return base.Add(time.Hour) }
	for _, snapshot := range []*index.Snapshot{a1, b1, a2} {
		if _, err := store.RecordSnapshot(snapshot); err != nil {
			t.Fatal(err)
		}
	}
	aRecords, err := store.List(a1.Root, a1.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	bRecords, err := store.List(b1.Root, b1.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if len(aRecords) != 1 || !aRecords[0].ScannedAt.Equal(a2.ScannedAt) || len(bRecords) != 1 {
		t.Fatalf("quota fairness records: key-a=%#v key-b=%#v", aRecords, bRecords)
	}
}

func testSnapshot(at time.Time, apparent int64) *index.Snapshot {
	return &index.Snapshot{
		Root: historyTestRoot(), Fingerprint: "scope-fp", ScannedAt: at,
		Files: 1, Dirs: 1, Complete: true,
		Nodes: []index.FlatNode{
			{Name: "data", IsDir: true, Apparent: apparent, Alloc: apparent, FileCount: 1, Parent: -1},
			{Name: "file", Depth: 1, Apparent: apparent, Alloc: apparent, Parent: 0},
		},
	}
}

func historyTestRoot() string {
	if runtime.GOOS == "windows" {
		return `C:\srv\data`
	}
	return "/srv/data"
}
