// Package history stores a bounded sequence of scan snapshots and compares
// them to expose growth over time. It deliberately reuses index.Snapshot's
// versioned encoding so history does not create a second scan-data format.
package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	appconfig "github.com/phillipod/go-dirstat/internal/config"
	"github.com/phillipod/go-dirstat/internal/index"
	"github.com/phillipod/go-dirstat/internal/storefs"
)

const (
	// MaxRecords is the maximum number of snapshots retained for one root and
	// scope fingerprint.
	MaxRecords = 20
	// MaxAge is the maximum snapshot age retained when a new record is written.
	MaxAge = 30 * 24 * time.Hour
	// DefaultMaxBytes is the store-wide durable-history budget. The per-key
	// record limit still applies independently.
	DefaultMaxBytes    int64 = 2 << 30
	historyLockName          = ".dirstat.lock"
	historySnapshotExt       = ".bin"
)

// Policy governs durable history across every root and fingerprint.
type Policy struct {
	MaxBytes int64
	MaxAge   time.Duration
}

// DefaultPolicy returns the built-in durable-history policy.
func DefaultPolicy() Policy { return Policy{MaxBytes: DefaultMaxBytes, MaxAge: MaxAge} }

// Store persists history below a private directory.
type Store struct {
	dir        string
	now        func() time.Time
	maxRecords int
	policy     Policy
}

// Entry describes one durable history file, including corruption and
// no-follow safety state for lifecycle reporting.
type Entry struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	Root        string    `json:"root,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	ScannedAt   time.Time `json:"scanned_at,omitempty"`
	ModifiedAt  time.Time `json:"modified_at"`
	SizeBytes   int64     `json:"size_bytes"`
	Complete    bool      `json:"complete"`
	Valid       bool      `json:"valid"`
	Safe        bool      `json:"safe"`
	Issue       string    `json:"issue,omitempty"`
}

// Action describes one deterministic history prune or clear decision.
type Action struct {
	Entry          Entry  `json:"entry"`
	Reason         string `json:"reason"`
	Removed        bool   `json:"removed"`
	MayHaveMutated bool   `json:"may_have_mutated,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Record identifies one stored snapshot.
type Record struct {
	ID          string    `json:"id"`
	Root        string    `json:"root"`
	Fingerprint string    `json:"fingerprint"`
	ScannedAt   time.Time `json:"scanned_at"`
	Files       int       `json:"files"`
	Dirs        int       `json:"dirs"`
	Errors      int64     `json:"errors"`
	Apparent    int64     `json:"apparent_bytes"`
	Allocated   int64     `json:"allocated_bytes"`
}

// NewStore creates the default history store under the user's durable state
// directory. History is operational state rather than an expendable scan cache.
func NewStore() (*Store, error) {
	return NewStoreWithLimit(MaxRecords)
}

// DefaultStoreDir returns the default durable history location without
// creating it. Callers can use it to exclude operational state before a scan.
func DefaultStoreDir() (string, error) {
	base, err := appconfig.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "history"), nil
}

// NewStoreWithLimit creates the default durable store with a caller-selected
// per-root retention count.
func NewStoreWithLimit(maxRecords int) (*Store, error) {
	dir, err := DefaultStoreDir()
	if err != nil {
		return nil, err
	}
	return NewStoreAtWithPolicy(dir, maxRecords, DefaultPolicy())
}

// OpenStoreWithLimit opens the default store contract without creating any
// directories. Reads from a missing store behave like an empty history.
func OpenStoreWithLimit(maxRecords int) (*Store, error) {
	dir, err := DefaultStoreDir()
	if err != nil {
		return nil, err
	}
	return OpenStoreAtWithPolicy(dir, maxRecords, DefaultPolicy())
}

// NewStoreAt creates a history store at dir. It is useful for applications
// that configure a state location and for isolated tests.
func NewStoreAt(dir string) (*Store, error) {
	return NewStoreAtWithLimit(dir, MaxRecords)
}

// NewStoreAtWithLimit creates a history store with a validated per-key record
// limit. It is used by the CLI's history_max configuration.
func NewStoreAtWithLimit(dir string, maxRecords int) (*Store, error) {
	return NewStoreAtWithPolicy(dir, maxRecords, DefaultPolicy())
}

// NewStoreAtWithPolicy creates a durable store with explicit per-key and
// store-wide retention policy.
func NewStoreAtWithPolicy(dir string, maxRecords int, policy Policy) (*Store, error) {
	store, err := OpenStoreAtWithPolicy(dir, maxRecords, policy)
	if err != nil {
		return nil, err
	}
	root, err := storefs.EnsureRoot(store.dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if err := root.EnsureOwnershipContext(context.Background(), stateKindHistory, false); err != nil {
		return nil, err
	}
	return store, nil
}

// OpenStoreAtWithLimit validates a store location without creating it. A later
// RecordSnapshot call may create the keyed record directory.
func OpenStoreAtWithLimit(dir string, maxRecords int) (*Store, error) {
	return OpenStoreAtWithPolicy(dir, maxRecords, DefaultPolicy())
}

// OpenStoreAtWithPolicy validates a durable-store contract without creating
// any directory.
func OpenStoreAtWithPolicy(dir string, maxRecords int, policy Policy) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("history: store directory is required")
	}
	if maxRecords <= 0 {
		return nil, errors.New("history: max records must be greater than zero")
	}
	if policy.MaxBytes <= 0 || policy.MaxAge <= 0 {
		return nil, errors.New("history: max bytes and max age must be greater than zero")
	}
	abs, err := storefs.ResolveStoreDir(dir)
	if err != nil {
		return nil, err
	}
	store := &Store{dir: abs, now: time.Now, maxRecords: maxRecords, policy: policy}
	root, openErr := storefs.OpenRoot(abs)
	if openErr == nil {
		_, _ = root.Ownership(stateKindHistory)
		_ = root.Close()
	} else if !errors.Is(openErr, fs.ErrNotExist) {
		return nil, openErr
	}
	return store, nil
}

// Dir returns the absolute directory containing this store. Callers that scan
// broad roots use it to keep history records out of their own measurements.
func (s *Store) Dir() string { return s.dir }

// Policy returns the store-wide durable-history policy.
func (s *Store) Policy() Policy { return s.policy }

const stateKindHistory = "history"

// Owned reports whether the exact history marker authorizes trusted reads and
// lifecycle writes.
func (s *Store) Owned() (bool, string) {
	root, err := storefs.OpenRoot(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, "store does not exist"
	}
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = root.Close() }()
	return root.Ownership(stateKindHistory)
}

// AdoptContext validates a recognized legacy history layout, then creates the
// ownership marker only when explicitly authorized by a non-dry migration.
func (s *Store) AdoptContext(ctx context.Context, dryRun bool) (bool, error) {
	if owned, _ := s.Owned(); owned {
		return false, nil
	}
	exists, err := storefs.CheckDir(s.dir)
	if err != nil || !exists {
		return false, err
	}
	root, err := storefs.OpenRoot(s.dir)
	if err != nil {
		return false, err
	}
	if err := s.validateLegacyStore(root); err != nil {
		_ = root.Close()
		return false, err
	}
	if err := root.CanAdopt(); err != nil {
		_ = root.Close()
		return false, err
	}
	if dryRun {
		_ = root.Close()
		return true, nil
	}
	err = root.WithLockContext(ctx, func(locked *storefs.Root) error {
		if err := s.validateLegacyStore(locked); err != nil {
			return err
		}
		if err := locked.CanAdopt(); err != nil {
			return err
		}
		return locked.EnsureOwnershipContext(ctx, stateKindHistory, true)
	})
	_ = root.Close()
	return true, err
}

func (s *Store) validateLegacyStore(root *storefs.Root) error {
	entries, err := root.ReadDir()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		key := entry.Name()
		if key == historyLockName {
			continue
		}
		if key == ".dirstat-store" {
			return errors.New("history: incompatible ownership marker cannot be adopted")
		}
		info, err := root.Lstat(key)
		if err != nil {
			return err
		}
		if strings.HasPrefix(key, ".marker-") && strings.HasSuffix(key, ".tmp") {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("history: legacy marker temp %q is unsafe", key)
			}
			continue
		}
		if !validKey(key) || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("history: foreign legacy entry %q prevents adoption", key)
		}
		keyRoot, err := root.OpenDir(key)
		if err != nil {
			return err
		}
		files, err := keyRoot.ReadDir()
		if err != nil {
			_ = keyRoot.Close()
			return err
		}
		for _, file := range files {
			name := file.Name()
			fileInfo, err := keyRoot.Lstat(name)
			if err != nil {
				_ = keyRoot.Close()
				return err
			}
			if fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() {
				_ = keyRoot.Close()
				return fmt.Errorf("history: legacy entry %q is not a regular file", filepath.Join(key, name))
			}
			if strings.HasPrefix(name, ".history-") && strings.HasSuffix(name, ".tmp") {
				continue
			}
			if filepath.Ext(name) != historySnapshotExt || !validID(strings.TrimSuffix(name, historySnapshotExt)) {
				_ = keyRoot.Close()
				return fmt.Errorf("history: foreign legacy entry %q prevents adoption", filepath.Join(key, name))
			}
			data, _, err := keyRoot.ReadRegular(name, s.recordLimit())
			if err != nil {
				_ = keyRoot.Close()
				return err
			}
			snap, _, inspectErr := index.Inspect(data)
			id := strings.TrimSuffix(name, historySnapshotExt)
			if inspectErr != nil || s.keyName(snap.Root, snap.Fingerprint) != key ||
				snap.ScannedAt.UTC().Format("20060102T150405.000000000Z") != id {
				_ = keyRoot.Close()
				return fmt.Errorf("history: entry %q is not a recognized dirstat snapshot", filepath.Join(key, name))
			}
		}
		_ = keyRoot.Close()
	}
	return nil
}

// RecordSnapshot atomically stores snap, then applies the per-root/fingerprint
// retention limits. Recording the same snapshot twice is idempotent.
func (s *Store) RecordSnapshot(snap *index.Snapshot) (Record, error) {
	return s.RecordSnapshotContext(context.Background(), snap)
}

// RecordSnapshotContext records a complete snapshot with caller cancellation
// threaded through locking, publication, and retention.
func (s *Store) RecordSnapshotContext(parent context.Context, snap *index.Snapshot) (Record, error) {
	if parent == nil {
		parent = context.Background()
	}
	if snap == nil || snap.Root == "" || snap.Fingerprint == "" || snap.ScannedAt.IsZero() || len(snap.Nodes) == 0 {
		return Record{}, errors.New("history: complete snapshot is required")
	}
	if !snap.Complete || snap.Errors != 0 {
		return Record{}, fmt.Errorf("history: incomplete snapshot (complete=%t errors=%d)", snap.Complete, snap.Errors)
	}
	if snap.ScannedAt.After(s.currentTime().Add(time.Minute)) {
		return Record{}, errors.New("history: snapshot timestamp is in the future")
	}
	if snap.ScannedAt.Before(s.currentTime().Add(-s.policy.MaxAge)) {
		return Record{}, errors.New("history: snapshot is older than the retention TTL")
	}
	data, err := snap.Marshal()
	if err != nil {
		return Record{}, err
	}
	validated, err := index.Unmarshal(data, snap.Fingerprint)
	if err != nil || validated.Root != snap.Root {
		return Record{}, errors.New("history: snapshot is invalid")
	}
	if int64(len(data)) > s.recordLimit() {
		return Record{}, fmt.Errorf("history: snapshot is larger than record budget (%d > %d bytes)", len(data), s.recordLimit())
	}
	id := snap.ScannedAt.UTC().Format("20060102T150405.000000000Z")
	ctx := parent
	err = storefs.WithLockChecked(ctx, s.dir, func(root *storefs.Root) error {
		return canInitializeHistoryStore(root)
	}, func(root *storefs.Root) error {
		if owned, _ := root.Ownership(stateKindHistory); !owned {
			if err := root.EnsureOwnershipContext(ctx, stateKindHistory, false); err != nil {
				return fmt.Errorf("history: store is not owned: %w", err)
			}
		}
		key := s.keyName(snap.Root, snap.Fingerprint)
		keyRoot, _, err := root.EnsureDirContext(ctx, key)
		if err != nil {
			return err
		}
		defer func() { _ = keyRoot.Close() }()
		destination := id + historySnapshotExt
		existing, _, readErr := keyRoot.ReadRegularContext(ctx, destination, s.recordLimit())
		alreadyExists := readErr == nil
		if alreadyExists && !bytes.Equal(existing, data) {
			return errors.New("history: scan timestamp collides with a different snapshot")
		}
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			return readErr
		}
		incoming := Entry{
			ID: filepath.ToSlash(filepath.Join(key, destination)), Key: key,
			Root: snap.Root, Fingerprint: snap.Fingerprint, ScannedAt: snap.ScannedAt.UTC(),
			ModifiedAt: s.currentTime().UTC(), SizeBytes: int64(len(data)),
			Complete: true, Valid: true, Safe: true,
		}
		if err := s.preflightRecordPolicyUnlocked(ctx, root, incoming, alreadyExists); err != nil {
			return err
		}
		if !alreadyExists {
			if err := keyRoot.AtomicWriteContext(ctx, destination, ".history-*.tmp", data); err != nil {
				return err
			}
		}
		if err := s.enforceRecordPolicyUnlocked(ctx, root, incoming); err != nil {
			return err
		}
		if _, err := keyRoot.Lstat(destination); err != nil {
			return fmt.Errorf("history: published record was not retained: %w", err)
		}
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	return describe(id, snap), nil
}

func canInitializeHistoryStore(root *storefs.Root) error {
	if owned, _ := root.Ownership(stateKindHistory); owned {
		return nil
	}
	if err := root.CanInitialize(); err != nil {
		return err
	}
	entries, err := root.ReadDir()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != historyLockName {
			return errors.New("history: store is unowned and non-empty; use state migrate --dry-run then --yes")
		}
	}
	return nil
}

// preflightRecordPolicyUnlocked proves the incoming record can survive both
// per-key and global retention without deleting prior durable history.
func (s *Store) preflightRecordPolicyUnlocked(ctx context.Context, root *storefs.Root, incoming Entry, alreadyExists bool) error {
	entries, err := s.listEntriesRootContext(ctx, root, false)
	if err != nil {
		return err
	}
	_, err = s.recordReservationPlan(entries, incoming, alreadyExists)
	return err
}

// enforceRecordPolicyUnlocked runs after atomic publication so any staging or
// publication failure preserves every prior record. A failed prune may leave
// temporary policy overshoot, which the next record or explicit prune repairs.
func (s *Store) enforceRecordPolicyUnlocked(ctx context.Context, root *storefs.Root, incoming Entry) error {
	entries, err := s.listEntriesRootContext(ctx, root, false)
	if err != nil {
		return err
	}
	remove, err := s.recordReservationPlan(entries, incoming, true)
	if err != nil {
		return err
	}
	_, err = s.applyRemovalsContext(ctx, root, entries, remove, false)
	return err
}

func (s *Store) recordReservationPlan(entries []Entry, incoming Entry, alreadyExists bool) (map[string]string, error) {
	if incoming.SizeBytes < 0 || incoming.SizeBytes > s.policy.MaxBytes {
		return nil, errors.New("history: incoming snapshot exceeds the store quota")
	}
	remove := make(map[string]string)
	cutoff := s.currentTime().Add(-s.policy.MaxAge)
	retained := make([]Entry, 0, len(entries)+1)
	foundExisting := false
	for _, entry := range entries {
		if entry.ID == incoming.ID {
			if !entry.Safe {
				return nil, fmt.Errorf("history: destination %q is unsafe and cannot be replaced", entry.ID)
			}
			if alreadyExists {
				retained = append(retained, incoming)
				foundExisting = true
				continue
			}
			return nil, fmt.Errorf("history: destination %q changed during reservation", entry.ID)
		}
		if !entry.Safe {
			continue
		}
		ageTime := entry.ScannedAt
		if ageTime.IsZero() {
			ageTime = entry.ModifiedAt
		}
		switch {
		case !entry.Valid:
			remove[entry.ID] = "invalid"
		case ageTime.Before(cutoff):
			remove[entry.ID] = "ttl"
		default:
			retained = append(retained, entry)
		}
	}
	if alreadyExists && !foundExisting {
		return nil, errors.New("history: destination changed during reservation")
	}
	if !alreadyExists {
		retained = append(retained, incoming)
	}

	// The incoming record must be one of the newest maxRecords for its exact
	// root+fingerprint key. Refuse an out-of-order write that policy could not
	// retain, and reserve its slot by removing older records first.
	var sameKey []Entry
	for _, entry := range retained {
		if entry.Key == incoming.Key {
			sameKey = append(sameKey, entry)
		}
	}
	sort.Slice(sameKey, func(i, j int) bool {
		if sameKey[i].ScannedAt.Equal(sameKey[j].ScannedAt) {
			return sameKey[i].ID < sameKey[j].ID
		}
		return sameKey[i].ScannedAt.After(sameKey[j].ScannedAt)
	})
	incomingRetained := false
	for i, entry := range sameKey {
		if i < s.maxRecords {
			if entry.ID == incoming.ID {
				incomingRetained = true
			}
			continue
		}
		if entry.ID != incoming.ID {
			remove[entry.ID] = "retention"
		}
	}
	if !incomingRetained {
		return nil, errors.New("history: per-key retention would not retain incoming snapshot")
	}

	active := make([]Entry, 0, len(retained))
	for _, entry := range retained {
		if _, removed := remove[entry.ID]; !removed {
			active = append(active, entry)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		if active[i].ScannedAt.Equal(active[j].ScannedAt) {
			return active[i].ID < active[j].ID
		}
		return active[i].ScannedAt.After(active[j].ScannedAt)
	})
	used := incoming.SizeBytes
	kept := map[string]bool{incoming.ID: true}
	firstByKey := make(map[string]Entry)
	for _, entry := range active {
		if _, ok := firstByKey[entry.Key]; !ok {
			firstByKey[entry.Key] = entry
		}
	}
	keys := make([]string, 0, len(firstByKey))
	for key := range firstByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := firstByKey[key]
		if kept[entry.ID] {
			continue
		}
		if entry.SizeBytes >= 0 && entry.SizeBytes <= s.policy.MaxBytes-used {
			used += entry.SizeBytes
			kept[entry.ID] = true
		}
	}
	for _, entry := range active {
		if kept[entry.ID] {
			continue
		}
		if entry.SizeBytes < 0 || entry.SizeBytes > s.policy.MaxBytes-used {
			remove[entry.ID] = "quota"
			continue
		}
		used += entry.SizeBytes
		kept[entry.ID] = true
	}
	return remove, nil
}

// List returns retained snapshots newest first. Foreign, malformed, and
// incompatible files are ignored so a damaged record does not hide good ones.
func (s *Store) List(root, fingerprint string) ([]Record, error) {
	return s.ListContext(context.Background(), root, fingerprint)
}

// ListContext lists one history key with cancellable bounded reads.
func (s *Store) ListContext(ctx context.Context, root, fingerprint string) ([]Record, error) {
	storeRoot, err := storefs.OpenRoot(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = storeRoot.Close() }()
	if owned, issue := storeRoot.Ownership(stateKindHistory); !owned {
		entries, readErr := storeRoot.ReadDir()
		if readErr != nil {
			return nil, readErr
		}
		empty := true
		for _, entry := range entries {
			if entry.Name() != historyLockName {
				empty = false
				break
			}
		}
		if empty {
			return nil, nil
		}
		return nil, fmt.Errorf("history: store is not owned: %s", issue)
	}
	keyRoot, err := storeRoot.OpenDir(s.keyName(root, fingerprint))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = keyRoot.Close() }()
	return s.listKeyRoot(ctx, keyRoot, root, fingerprint)
}

func (s *Store) listKeyRoot(ctx context.Context, keyRoot *storefs.Root, root, fingerprint string) ([]Record, error) {
	entries, err := keyRoot.ReadDir()
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return records, err
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || filepath.Ext(entry.Name()) != historySnapshotExt {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), historySnapshotExt)
		if !validID(id) {
			continue
		}
		snap, err := s.loadRootContext(ctx, keyRoot, entry.Name(), root, fingerprint)
		if err != nil {
			continue
		}
		records = append(records, describe(id, snap))
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ScannedAt.After(records[j].ScannedAt) })
	return records, nil
}

// ListEntries discovers every history-store entry without creating state.
// Foreign, corrupt, and unsafe entries remain visible for diagnosis.
func (s *Store) ListEntries() ([]Entry, error) {
	return s.ListEntriesContext(context.Background())
}

// ListEntriesContext discovers all entries with cancellable bounded reads.
func (s *Store) ListEntriesContext(ctx context.Context) ([]Entry, error) {
	root, err := storefs.OpenRoot(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return s.listEntriesRootContext(ctx, root, false)
}

func (s *Store) listEntriesRoot(root *storefs.Root, trustLegacy bool) ([]Entry, error) {
	return s.listEntriesRootContext(context.Background(), root, trustLegacy)
}

func (s *Store) listEntriesRootContext(ctx context.Context, root *storefs.Root, trustLegacy bool) ([]Entry, error) {
	owned, ownershipIssue := root.Ownership(stateKindHistory)
	if trustLegacy {
		owned, ownershipIssue = true, ""
	}
	keys, err := root.ReadDir()
	if err != nil {
		return nil, err
	}
	var result []Entry
	for _, keyEntry := range keys {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		key := keyEntry.Name()
		if key == historyLockName || key == ".dirstat-store" {
			continue
		}
		if strings.HasPrefix(key, ".marker-") && strings.HasSuffix(key, ".tmp") {
			info, statErr := root.Lstat(key)
			entry := Entry{ID: key, Safe: owned, Issue: "abandoned marker temporary file"}
			if statErr != nil {
				entry.Safe, entry.Issue = false, statErr.Error()
			} else {
				entry.ModifiedAt, entry.SizeBytes = info.ModTime().UTC(), info.Size()
				if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
					entry.Safe, entry.Issue = false, "marker temporary entry is not a regular file"
				}
			}
			result = append(result, entry)
			continue
		}
		keyInfo, statErr := root.Lstat(key)
		if statErr != nil {
			result = append(result, Entry{ID: key, Key: key, Issue: statErr.Error()})
			continue
		}
		if !owned {
			result = append(result, Entry{
				ID: key, Key: key, ModifiedAt: keyInfo.ModTime().UTC(), SizeBytes: keyInfo.Size(),
				Issue: ownershipIssue,
			})
			continue
		}
		if keyInfo.Mode()&os.ModeSymlink != 0 || !keyInfo.IsDir() || !validKey(key) {
			result = append(result, Entry{
				ID: key, Key: key, ModifiedAt: keyInfo.ModTime().UTC(),
				SizeBytes: keyInfo.Size(), Issue: "not an owned history key directory",
			})
			continue
		}
		keyRoot, openErr := root.OpenDir(key)
		if openErr != nil {
			result = append(result, Entry{ID: key, Key: key, Safe: true, Issue: openErr.Error()})
			continue
		}
		files, readErr := keyRoot.ReadDir()
		if readErr != nil {
			_ = keyRoot.Close()
			result = append(result, Entry{ID: key, Key: key, Safe: true, Issue: readErr.Error()})
			continue
		}
		for _, fileEntry := range files {
			name := fileEntry.Name()
			id := filepath.ToSlash(filepath.Join(key, name))
			info, fileErr := keyRoot.Lstat(name)
			entry := Entry{ID: id, Key: key, Safe: owned && validID(strings.TrimSuffix(name, historySnapshotExt)) && filepath.Ext(name) == historySnapshotExt}
			if fileErr != nil {
				entry.Issue = fileErr.Error()
				result = append(result, entry)
				continue
			}
			entry.ModifiedAt, entry.SizeBytes = info.ModTime().UTC(), info.Size()
			ownedTemp := strings.HasPrefix(name, ".history-") && strings.HasSuffix(name, ".tmp")
			if ownedTemp {
				if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
					entry.Safe = false
					entry.Issue = "temporary entry is not a regular file"
				} else {
					entry.Safe = true
					entry.Issue = "abandoned temporary file"
				}
				result = append(result, entry)
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				entry.Safe = false
				entry.Issue = "not a regular file"
				result = append(result, entry)
				continue
			}
			data, opened, readErr := keyRoot.ReadRegularContext(ctx, name, s.recordLimit())
			if readErr != nil {
				entry.Issue = readErr.Error()
				result = append(result, entry)
				continue
			}
			entry.ModifiedAt, entry.SizeBytes = opened.ModTime().UTC(), opened.Size()
			snap, decodeErr := index.Unmarshal(data, "")
			if decodeErr != nil {
				entry.Issue = decodeErr.Error()
				result = append(result, entry)
				continue
			}
			entry.Root, entry.Fingerprint = snap.Root, snap.Fingerprint
			entry.ScannedAt = snap.ScannedAt.UTC()
			entry.Complete = snap.Complete && snap.Errors == 0
			switch {
			case s.keyName(snap.Root, snap.Fingerprint) != key:
				entry.Issue = "snapshot key does not match directory"
			case strings.TrimSuffix(name, historySnapshotExt) != snap.ScannedAt.UTC().Format("20060102T150405.000000000Z"):
				entry.Issue = "snapshot timestamp does not match filename"
			case snap.ScannedAt.After(s.currentTime().Add(time.Minute)):
				entry.Issue = "snapshot timestamp is in the future"
			case !entry.Complete:
				entry.Issue = "incomplete snapshot"
			default:
				entry.Valid = entry.Safe
			}
			result = append(result, entry)
		}
		_ = keyRoot.Close()
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// Prune applies the durable store's global TTL and byte policy. Missing stores
// are a non-creating no-op; dry runs are always read-only.
func (s *Store) Prune(dryRun bool) ([]Action, error) {
	return s.PruneContext(context.Background(), dryRun)
}

// PruneContext is Prune with cancellation before each deletion.
func (s *Store) PruneContext(parent context.Context, dryRun bool) ([]Action, error) {
	return s.runLifecycleContext(parent, dryRun, "prune", s.pruneGlobalUnlockedContext)
}

// Clear removes all safely owned history records. Unsafe and foreign entries
// remain visible and untouched.
func (s *Store) Clear(dryRun bool) ([]Action, error) {
	return s.ClearContext(context.Background(), dryRun)
}

// ClearContext is Clear with cancellation before each deletion.
func (s *Store) ClearContext(parent context.Context, dryRun bool) ([]Action, error) {
	return s.runLifecycleContext(parent, dryRun, "clear", s.clearUnlockedContext)
}

func (s *Store) runLifecycleContext(
	parent context.Context,
	dryRun bool,
	operation string,
	run func(context.Context, *storefs.Root, bool) ([]Action, error),
) ([]Action, error) {
	if parent == nil {
		parent = context.Background()
	}
	exists, err := storefs.CheckDir(s.dir)
	if err != nil || !exists {
		return nil, err
	}
	if dryRun {
		root, err := storefs.OpenRoot(s.dir)
		if err != nil {
			return nil, err
		}
		defer func() { _ = root.Close() }()
		if owned, issue := root.Ownership(stateKindHistory); !owned {
			return nil, fmt.Errorf("history: refusing to %s unowned store: %s", operation, issue)
		}
		return run(parent, root, true)
	}
	ctx := parent
	var actions []Action
	err = s.withOwnedLock(ctx, operation, func(root *storefs.Root) error {
		if owned, issue := root.Ownership(stateKindHistory); !owned {
			return fmt.Errorf("history: refusing to %s unowned store: %s", operation, issue)
		}
		var runErr error
		actions, runErr = run(ctx, root, false)
		return runErr
	})
	return actions, err
}

// MigrateTo moves compatible records to destination and invalidates safely
// owned incompatible records. Unsafe or foreign entries are reported and left
// untouched. A dry run does not create either store.
func (s *Store) MigrateTo(destination *Store, dryRun bool) ([]Action, error) {
	return s.MigrateToContext(context.Background(), destination, dryRun)
}

// MigrateToContext migrates recognized legacy state with caller cancellation.
func (s *Store) MigrateToContext(parent context.Context, destination *Store, dryRun bool) ([]Action, error) {
	if parent == nil {
		parent = context.Background()
	}
	if destination == nil {
		return nil, errors.New("history: migration destination is required")
	}
	if s.dir == destination.dir {
		return nil, errors.New("history: migration source and destination are the same store")
	}
	exists, err := storefs.CheckDir(s.dir)
	if err != nil || !exists {
		return nil, err
	}
	sourceRoot, err := storefs.OpenRoot(s.dir)
	if err != nil {
		return nil, err
	}
	if err := s.validateMigrationSource(sourceRoot, dryRun); err != nil {
		_ = sourceRoot.Close()
		return nil, err
	}
	if dryRun {
		defer func() { _ = sourceRoot.Close() }()
		return s.migrateUnlocked(parent, sourceRoot, destination, true)
	}
	ctx := parent
	defer func() { _ = sourceRoot.Close() }()
	if err := destination.ensureMigrationDestination(ctx); err != nil {
		return nil, fmt.Errorf("history: prepare migration destination: %w", err)
	}
	var actions []Action
	err = sourceRoot.WithLockContext(ctx, func(root *storefs.Root) error {
		if owned, issue := root.Ownership(stateKindHistory); !owned {
			return fmt.Errorf("history: refusing to migrate unowned source: %s", issue)
		}
		if err := s.validateMigrationSource(root, false); err != nil {
			return err
		}
		var migrateErr error
		actions, migrateErr = s.migrateUnlocked(ctx, root, destination, false)
		return migrateErr
	})
	return actions, err
}

// ensureMigrationDestination establishes exact destination ownership before
// any source mutation. Existing non-empty legacy state must have been adopted
// explicitly by the caller; direct migration never widens that authority.
func (s *Store) ensureMigrationDestination(ctx context.Context) error {
	exists, err := storefs.CheckDir(s.dir)
	if err != nil {
		return err
	}
	if exists {
		root, err := storefs.OpenRoot(s.dir)
		if err != nil {
			return err
		}
		if owned, _ := root.Ownership(stateKindHistory); owned {
			return root.Close()
		}
		entries, readErr := root.ReadDir()
		closeErr := root.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		for _, entry := range entries {
			if entry.Name() != historyLockName {
				return errors.New("destination history store is unowned and non-empty; adopt it explicitly before migration")
			}
		}
	}
	return storefs.WithLockChecked(ctx, s.dir, func(root *storefs.Root) error {
		return canInitializeHistoryStore(root)
	}, func(root *storefs.Root) error {
		if owned, _ := root.Ownership(stateKindHistory); owned {
			return nil
		}
		return root.EnsureOwnershipContext(ctx, stateKindHistory, false)
	})
}

func (s *Store) validateMigrationSource(root *storefs.Root, allowLegacy bool) error {
	if owned, issue := root.Ownership(stateKindHistory); owned {
		return nil
	} else if !allowLegacy {
		return fmt.Errorf("history: refusing to migrate unowned source: %s", issue)
	}
	return s.validateLegacyStore(root)
}

// withOwnedLock preflights and locks one stable directory capability, so a
// raced pathname replacement cannot receive a lock before ownership is known.
func (s *Store) withOwnedLock(ctx context.Context, operation string, fn func(*storefs.Root) error) error {
	root, err := storefs.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if owned, issue := root.Ownership(stateKindHistory); !owned {
		return fmt.Errorf("history: refusing to %s unowned store: %s", operation, issue)
	}
	return root.WithLockContext(ctx, fn)
}

func (s *Store) migrateUnlocked(ctx context.Context, root *storefs.Root, destination *Store, dryRun bool) ([]Action, error) {
	entries, err := s.listEntriesRootContext(ctx, root, true)
	if err != nil {
		return nil, err
	}
	if err := destination.preflightMigrationBatch(ctx, entries); err != nil {
		return nil, fmt.Errorf("preflight migration batch: %w", err)
	}
	// Decode and compare every valid payload before the first source removal.
	// Aggregate identity/count checks alone cannot detect a same-timestamp
	// destination collision containing different bytes.
	snapshots := make(map[string]*index.Snapshot)
	for _, entry := range entries {
		if !entry.Valid {
			continue
		}
		keyRoot, openErr := root.OpenDir(entry.Key)
		if openErr != nil {
			return nil, openErr
		}
		name := filepath.Base(filepath.FromSlash(entry.ID))
		snap, loadErr := s.loadRootContext(ctx, keyRoot, name, entry.Root, entry.Fingerprint)
		_ = keyRoot.Close()
		if loadErr != nil {
			return nil, loadErr
		}
		if preflightErr := destination.preflightMigration(ctx, snap); preflightErr != nil {
			return nil, fmt.Errorf("preflight destination record %q: %w", entry.ID, preflightErr)
		}
		snapshots[entry.ID] = snap
	}
	var actions []Action
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return actions, err
		}
		action := Action{Entry: entry}
		if !entry.Safe {
			action.Reason = "unsafe"
			action.Error = "left untouched"
			actions = append(actions, action)
			continue
		}
		if !entry.Valid {
			action.Reason = "invalidate"
			if !dryRun {
				removed, err := removeHistoryEntry(ctx, root, entry)
				action.Removed = removed
				if err != nil {
					action.MayHaveMutated = !removed
					action.Error = err.Error()
					actions = append(actions, action)
					return actions, err
				}
				action.Removed = true
			}
			actions = append(actions, action)
			continue
		}
		action.Reason = "migrate"
		name := filepath.Base(filepath.FromSlash(entry.ID))
		snap := snapshots[entry.ID]
		if !dryRun {
			if _, err := destination.RecordSnapshotContext(ctx, snap); err != nil {
				action.Error = err.Error()
				actions = append(actions, action)
				return actions, err
			}
			if _, err := destination.LoadContext(ctx, Record{ID: strings.TrimSuffix(name, historySnapshotExt), Root: entry.Root, Fingerprint: entry.Fingerprint}); err != nil {
				action.Error = "destination did not retain migrated record: " + err.Error()
				actions = append(actions, action)
				return actions, errors.New(action.Error)
			}
			removed, err := removeHistoryEntry(ctx, root, entry)
			action.Removed = removed
			if err != nil {
				action.MayHaveMutated = !removed
				action.Error = err.Error()
				actions = append(actions, action)
				return actions, err
			}
			action.Removed = true
		}
		actions = append(actions, action)
	}
	return actions, nil
}

func removeHistoryEntry(ctx context.Context, root *storefs.Root, entry Entry) (bool, error) {
	if entry.Key == "" {
		err := root.RemoveRegularContext(ctx, entry.ID)
		switch err {
		case nil:
			return true, nil
		default:
			_, statErr := root.Lstat(entry.ID)
			return errors.Is(statErr, fs.ErrNotExist), err
		}
	}
	keyRoot, err := root.OpenDir(entry.Key)
	if err != nil {
		return false, err
	}
	if err := keyRoot.RemoveRegularContext(ctx, filepath.Base(filepath.FromSlash(entry.ID))); err != nil {
		_, statErr := keyRoot.Lstat(filepath.Base(filepath.FromSlash(entry.ID)))
		_ = keyRoot.Close()
		return errors.Is(statErr, fs.ErrNotExist), err
	}
	if err := keyRoot.Close(); err != nil {
		return true, err
	}
	if err := root.RemoveEmptyDirContext(ctx, entry.Key); err != nil && !errors.Is(err, fs.ErrExist) {
		return true, err
	}
	return true, nil
}

func (s *Store) preflightMigration(ctx context.Context, snap *index.Snapshot) error {
	if snap == nil || !snap.Complete || snap.Errors != 0 || snap.ScannedAt.IsZero() {
		return errors.New("destination rejects incomplete migration snapshot")
	}
	if snap.ScannedAt.Before(s.currentTime().Add(-s.policy.MaxAge)) {
		return errors.New("destination TTL would immediately prune migrated record")
	}
	data, err := snap.Marshal()
	if err != nil {
		return err
	}
	if int64(len(data)) > s.recordLimit() {
		return errors.New("destination record budget is too small")
	}
	root, err := storefs.OpenRoot(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if !root.Writable() {
		return errors.New("destination history store is not writable")
	}
	trustedLegacy := false
	if owned, issue := root.Ownership(stateKindHistory); !owned {
		if err := s.validateLegacyStore(root); err != nil {
			return fmt.Errorf("destination history store is unowned: %s: %w", issue, err)
		}
		trustedLegacy = true
	}
	var records []Record
	keyRoot, keyErr := root.OpenDir(s.keyName(snap.Root, snap.Fingerprint))
	if keyErr == nil {
		records, err = s.listKeyRoot(ctx, keyRoot, snap.Root, snap.Fingerprint)
		_ = keyRoot.Close()
	} else if !errors.Is(keyErr, fs.ErrNotExist) {
		return keyErr
	}
	if err != nil {
		return err
	}
	id := snap.ScannedAt.UTC().Format("20060102T150405.000000000Z")
	for _, record := range records {
		if record.ID == id {
			existing, err := s.LoadContext(ctx, record)
			if err != nil {
				return err
			}
			existingData, err := existing.Marshal()
			if err != nil {
				return err
			}
			if !bytes.Equal(existingData, data) {
				return errors.New("destination has a different record at the same timestamp")
			}
			return nil
		}
	}
	if len(records) >= s.maxRecords && !snap.ScannedAt.After(records[s.maxRecords-1].ScannedAt) {
		return errors.New("destination per-key retention would immediately prune migrated record")
	}
	var entries []Entry
	if trustedLegacy {
		entries, err = s.listEntriesRootContext(ctx, root, true)
	} else {
		entries, err = s.listEntriesRootContext(ctx, root, false)
	}
	if err != nil {
		return err
	}
	var used int64
	for _, entry := range entries {
		if !entry.Valid || entry.ScannedAt.Before(s.currentTime().Add(-s.policy.MaxAge)) {
			continue
		}
		if entry.SizeBytes < 0 || entry.SizeBytes > s.policy.MaxBytes-used {
			return errors.New("destination history store already exceeds its quota")
		}
		used += entry.SizeBytes
	}
	if int64(len(data)) > s.policy.MaxBytes-used {
		return errors.New("destination quota would not retain migrated record")
	}
	return nil
}

func (s *Store) preflightMigrationBatch(ctx context.Context, source []Entry) error {
	var destination []Entry
	root, err := storefs.OpenRoot(s.dir)
	switch {
	case err == nil:
		trusted := false
		if owned, _ := root.Ownership(stateKindHistory); !owned {
			if validateErr := s.validateLegacyStore(root); validateErr != nil {
				_ = root.Close()
				return validateErr
			}
			trusted = true
		}
		destination, err = s.listEntriesRootContext(ctx, root, trusted)
		_ = root.Close()
	case errors.Is(err, fs.ErrNotExist):
		err = nil
	default:
		return err
	}
	if err != nil {
		return err
	}
	identities := make(map[string]bool)
	perKey := make(map[string]int)
	var used int64
	cutoff := s.currentTime().Add(-s.policy.MaxAge)
	for _, entry := range destination {
		if !entry.Valid || entry.ScannedAt.Before(cutoff) {
			continue
		}
		if entry.SizeBytes < 0 || entry.SizeBytes > s.policy.MaxBytes-used {
			return errors.New("destination history store exceeds its quota")
		}
		used += entry.SizeBytes
		identities[historyEntryIdentity(entry)] = true
		perKey[entry.Key]++
	}
	for _, entry := range source {
		if !entry.Valid {
			continue
		}
		if entry.ScannedAt.Before(cutoff) {
			return fmt.Errorf("destination TTL would prune %q", entry.ID)
		}
		identity := historyEntryIdentity(entry)
		if identities[identity] {
			continue
		}
		key := s.keyName(entry.Root, entry.Fingerprint)
		if perKey[key] >= s.maxRecords {
			return fmt.Errorf("destination per-key retention cannot retain migration batch for %q", entry.Root)
		}
		if entry.SizeBytes < 0 || entry.SizeBytes > s.policy.MaxBytes-used {
			return errors.New("destination quota cannot retain the complete migration batch")
		}
		used += entry.SizeBytes
		perKey[key]++
		identities[identity] = true
	}
	return nil
}

func historyEntryIdentity(entry Entry) string {
	return entry.Root + "\x00" + entry.Fingerprint + "\x00" + entry.ScannedAt.UTC().Format(time.RFC3339Nano)
}

func (s *Store) pruneGlobalUnlockedContext(ctx context.Context, root *storefs.Root, dryRun bool) ([]Action, error) {
	entries, err := s.listEntriesRoot(root, false)
	if err != nil {
		return nil, err
	}
	remove := make(map[string]string)
	cutoff := s.currentTime().Add(-s.policy.MaxAge)
	var retained []Entry
	for _, entry := range entries {
		if !entry.Safe {
			continue
		}
		ageTime := entry.ScannedAt
		if ageTime.IsZero() {
			ageTime = entry.ModifiedAt
		}
		switch {
		case !entry.Valid:
			remove[entry.ID] = "invalid"
		case ageTime.Before(cutoff):
			remove[entry.ID] = "ttl"
		default:
			retained = append(retained, entry)
		}
	}
	sort.Slice(retained, func(i, j int) bool {
		if retained[i].ScannedAt.Equal(retained[j].ScannedAt) {
			return retained[i].ID < retained[j].ID
		}
		return retained[i].ScannedAt.After(retained[j].ScannedAt)
	})
	var used int64
	kept := make(map[string]bool)
	firstByKey := make(map[string]Entry)
	for _, entry := range retained {
		if _, ok := firstByKey[entry.Key]; !ok {
			firstByKey[entry.Key] = entry
		}
	}
	keys := make([]string, 0, len(firstByKey))
	for key := range firstByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := firstByKey[key]
		if kept[entry.ID] {
			continue
		}
		if entry.SizeBytes >= 0 && entry.SizeBytes <= s.policy.MaxBytes-used {
			used += entry.SizeBytes
			kept[entry.ID] = true
		}
	}
	for _, entry := range retained {
		if kept[entry.ID] {
			continue
		}
		if entry.SizeBytes < 0 || entry.SizeBytes > s.policy.MaxBytes-used {
			remove[entry.ID] = "quota"
			continue
		}
		used += entry.SizeBytes
		kept[entry.ID] = true
	}
	return s.applyRemovalsContext(ctx, root, entries, remove, dryRun)
}

func (s *Store) clearUnlockedContext(ctx context.Context, root *storefs.Root, dryRun bool) ([]Action, error) {
	entries, err := s.listEntriesRoot(root, false)
	if err != nil {
		return nil, err
	}
	remove := make(map[string]string)
	for _, entry := range entries {
		if entry.Safe {
			remove[entry.ID] = "clear"
		}
	}
	return s.applyRemovalsContext(ctx, root, entries, remove, dryRun)
}

func (*Store) applyRemovalsContext(ctx context.Context, root *storefs.Root, entries []Entry, remove map[string]string, dryRun bool) ([]Action, error) {
	var actions []Action
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return actions, err
		}
		reason, ok := remove[entry.ID]
		if !ok {
			continue
		}
		action := Action{Entry: entry, Reason: reason}
		if !dryRun {
			if entry.Key == "" {
				err := root.RemoveRegularContext(ctx, entry.ID)
				if err != nil {
					if _, statErr := root.Lstat(entry.ID); errors.Is(statErr, fs.ErrNotExist) {
						action.Removed = true
					} else {
						action.MayHaveMutated = true
					}
					action.Error = err.Error()
					actions = append(actions, action)
					return actions, err
				}
				action.Removed = true
				actions = append(actions, action)
				continue
			}
			keyRoot, err := root.OpenDir(entry.Key)
			if err != nil {
				action.Error = err.Error()
				actions = append(actions, action)
				return actions, err
			}
			name := filepath.Base(filepath.FromSlash(entry.ID))
			err = keyRoot.RemoveRegularContext(ctx, name)
			if err != nil {
				if _, statErr := keyRoot.Lstat(name); errors.Is(statErr, fs.ErrNotExist) {
					action.Removed = true
				} else {
					action.MayHaveMutated = true
				}
			}
			closeErr := keyRoot.Close()
			if err != nil {
				action.Error = err.Error()
				actions = append(actions, action)
				return actions, err
			}
			action.Removed = true
			if closeErr != nil {
				action.Error = closeErr.Error()
				actions = append(actions, action)
				return actions, closeErr
			}
			if err := root.RemoveEmptyDirContext(ctx, entry.Key); err != nil && !errors.Is(err, fs.ErrExist) {
				action.Error = err.Error()
				actions = append(actions, action)
				return actions, err
			}
			action.Removed = true
		}
		actions = append(actions, action)
	}
	return actions, nil
}

// Load reads a listed record and verifies both its key and encoded identity.
func (s *Store) Load(record Record) (*index.Snapshot, error) {
	return s.LoadContext(context.Background(), record)
}

// LoadContext loads one record through stable store/key capabilities.
func (s *Store) LoadContext(ctx context.Context, record Record) (*index.Snapshot, error) {
	if record.Root == "" || record.Fingerprint == "" || !validID(record.ID) {
		return nil, errors.New("history: invalid record")
	}
	root, err := storefs.OpenRoot(s.dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if owned, issue := root.Ownership(stateKindHistory); !owned {
		return nil, fmt.Errorf("history: store is not owned: %s", issue)
	}
	keyRoot, err := root.OpenDir(s.keyName(record.Root, record.Fingerprint))
	if err != nil {
		return nil, err
	}
	defer func() { _ = keyRoot.Close() }()
	return s.loadRootContext(ctx, keyRoot, record.ID+historySnapshotExt, record.Root, record.Fingerprint)
}

// Previous loads the newest snapshot scanned strictly before before. A zero
// time selects the newest retained snapshot.
func (s *Store) Previous(root, fingerprint string, before time.Time) (*index.Snapshot, error) {
	return s.PreviousContext(context.Background(), root, fingerprint, before)
}

// PreviousContext is Previous with cancellable inventory and load.
func (s *Store) PreviousContext(ctx context.Context, root, fingerprint string, before time.Time) (*index.Snapshot, error) {
	records, err := s.ListContext(ctx, root, fingerprint)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Errors != 0 {
			continue
		}
		if record.ScannedAt.Before(s.currentTime().Add(-s.policy.MaxAge)) {
			continue
		}
		if before.IsZero() || record.ScannedAt.Before(before) {
			return s.LoadContext(ctx, record)
		}
	}
	return nil, fs.ErrNotExist
}

func (s *Store) loadRootContext(ctx context.Context, keyRoot *storefs.Root, name, root, fingerprint string) (*index.Snapshot, error) {
	data, _, err := keyRoot.ReadRegularContext(ctx, name, s.recordLimit())
	if err != nil {
		return nil, err
	}
	snap, err := index.Unmarshal(data, fingerprint)
	if err != nil {
		return nil, err
	}
	wantID := snap.ScannedAt.UTC().Format("20060102T150405.000000000Z")
	gotID := strings.TrimSuffix(name, historySnapshotExt)
	if snap.Root != root || !snap.Complete || snap.Errors != 0 || wantID != gotID || snap.ScannedAt.After(s.currentTime().Add(time.Minute)) {
		return nil, index.ErrIncompatible
	}
	if snap.ScannedAt.Before(s.currentTime().Add(-s.policy.MaxAge)) {
		return nil, index.ErrStale
	}
	return snap, nil
}

func (s *Store) keyDir(root, fingerprint string) string {
	return filepath.Join(s.dir, s.keyName(root, fingerprint))
}

func (*Store) keyName(root, fingerprint string) string {
	sum := sha256.Sum256([]byte(root + "\x00" + fingerprint))
	return hex.EncodeToString(sum[:16])
}

func describe(id string, snap *index.Snapshot) Record {
	record := Record{
		ID: id, Root: snap.Root, Fingerprint: snap.Fingerprint,
		ScannedAt: snap.ScannedAt, Files: snap.Files, Dirs: snap.Dirs, Errors: snap.Errors,
	}
	if len(snap.Nodes) > 0 {
		record.Apparent = snap.Nodes[0].Apparent
		record.Allocated = snap.Nodes[0].Alloc
	}
	return record
}

func validID(id string) bool {
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return false
	}
	_, err := time.Parse("20060102T150405.000000000Z", id)
	return err == nil
}

func validKey(key string) bool {
	if len(key) != 32 || filepath.Base(key) != key {
		return false
	}
	_, err := hex.DecodeString(key)
	return err == nil
}

func (s *Store) recordLimit() int64 {
	if s.policy.MaxBytes < index.MaxRecordBytes {
		return s.policy.MaxBytes
	}
	return index.MaxRecordBytes
}

func (s *Store) currentTime() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}
