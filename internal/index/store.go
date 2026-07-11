package index

import (
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

	"github.com/phillipod/go-dirstat/internal/storefs"
)

const (
	// DefaultMaxBytes is the global cache budget used when configuration does
	// not override it.
	DefaultMaxBytes int64 = 512 << 20
	// MaxRecordBytes bounds one decode/allocation independently of the global
	// store budget.
	MaxRecordBytes   int64 = 256 << 20
	indexLockName          = ".dirstat.lock"
	indexMarkerName        = ".dirstat-store"
	indexSnapshotExt       = ".bin"
	// DefaultMaxAge is the cache TTL and default persisted-query freshness
	// horizon.
	DefaultMaxAge = 30 * 24 * time.Hour
)

// Policy governs the whole cache store, not one root/fingerprint key.
type Policy struct {
	MaxBytes int64
	MaxAge   time.Duration
}

// DefaultPolicy returns the bounded built-in cache policy.
func DefaultPolicy() Policy {
	return Policy{MaxBytes: DefaultMaxBytes, MaxAge: DefaultMaxAge}
}

// Store persists complete snapshots in a single private directory.
type Store struct {
	dir    string
	policy Policy
	now    func() time.Time
}

// Entry describes one cache filename without hiding corrupt or unsafe entries.
// Unsafe entries are visible but never removed by lifecycle operations.
type Entry struct {
	ID          string    `json:"id"`
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

// Action describes a deterministic prune or clear decision.
type Action struct {
	Entry          Entry  `json:"entry"`
	Reason         string `json:"reason"`
	Removed        bool   `json:"removed"`
	MayHaveMutated bool   `json:"may_have_mutated,omitempty"`
	Error          string `json:"error,omitempty"`
}

// DefaultStoreDir returns the cache location without creating it.
func DefaultStoreDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "dirstat"), nil
}

// NewStore opens the default cache for writes, creating the directory safely.
func NewStore() (*Store, error) {
	dir, err := DefaultStoreDir()
	if err != nil {
		return nil, err
	}
	return NewStoreAtWithPolicy(dir, DefaultPolicy())
}

// OpenStore opens the default cache contract without creating state.
func OpenStore() (*Store, error) {
	dir, err := DefaultStoreDir()
	if err != nil {
		return nil, err
	}
	return OpenStoreAtWithPolicy(dir, DefaultPolicy())
}

// NewStoreAt creates a cache at dir using the default global policy.
func NewStoreAt(dir string) (*Store, error) {
	return NewStoreAtWithPolicy(dir, DefaultPolicy())
}

// OpenStoreAt validates a cache location without creating it.
func OpenStoreAt(dir string) (*Store, error) {
	return OpenStoreAtWithPolicy(dir, DefaultPolicy())
}

// NewStoreAtWithPolicy creates a cache with an explicit lifecycle policy.
func NewStoreAtWithPolicy(dir string, policy Policy) (*Store, error) {
	store, err := OpenStoreAtWithPolicy(dir, policy)
	if err != nil {
		return nil, err
	}
	root, err := storefs.EnsureRoot(store.dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if err := root.EnsureOwnershipContext(context.Background(), stateKindIndex, false); err != nil {
		return nil, err
	}
	return store, nil
}

// OpenStoreAtWithPolicy validates but does not create a cache contract.
func OpenStoreAtWithPolicy(dir string, policy Policy) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("index: store directory is required")
	}
	if policy.MaxBytes <= 0 || policy.MaxAge <= 0 {
		return nil, errors.New("index: cache max bytes and max age must be greater than zero")
	}
	abs, err := storefs.ResolveStoreDir(dir)
	if err != nil {
		return nil, err
	}
	store := &Store{dir: abs, policy: policy, now: time.Now}
	root, openErr := storefs.OpenRoot(abs)
	if openErr == nil {
		_, _ = root.Ownership(stateKindIndex)
		_ = root.Close()
	} else if !errors.Is(openErr, fs.ErrNotExist) {
		return nil, openErr
	}
	return store, nil
}

// Dir returns the absolute cache directory.
func (s *Store) Dir() string { return s.dir }

// Policy returns the global cache policy.
func (s *Store) Policy() Policy { return s.policy }

const stateKindIndex = "index"

// Owned reports whether the exact store marker authorizes lifecycle writes.
func (s *Store) Owned() (bool, string) {
	root, err := storefs.OpenRoot(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, "store does not exist"
	}
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = root.Close() }()
	return root.Ownership(stateKindIndex)
}

// AdoptContext explicitly marks a preexisting store as dirstat-owned. Dry runs
// report whether adoption is needed without changing permissions or state.
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
		return locked.EnsureOwnershipContext(ctx, stateKindIndex, true)
	})
	_ = root.Close()
	return true, err
}

// PreviewInvalidationAfterAdoption validates a legacy store and reports the
// incompatible entries an explicit migration would remove.
func (s *Store) PreviewInvalidationAfterAdoption(ctx context.Context) ([]Action, error) {
	root, err := storefs.OpenRoot(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if owned, _ := root.Ownership(stateKindIndex); !owned {
		if err := s.validateLegacyStore(root); err != nil {
			return nil, err
		}
	}
	entries, err := s.listRoot(ctx, root, true)
	if err != nil {
		return nil, err
	}
	var actions []Action
	for _, entry := range entries {
		if entry.Safe && !entry.Valid {
			actions = append(actions, Action{Entry: entry, Reason: "invalidate"})
		}
	}
	return actions, nil
}

func (s *Store) validateLegacyStore(root *storefs.Root) error {
	entries, err := root.ReadDir()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == indexLockName {
			continue
		}
		if name == indexMarkerName {
			return errors.New("index: incompatible ownership marker cannot be adopted")
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			if name == "history" && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				// Explicit state migration validates this legacy history subtree
				// independently; cache lifecycle never deletes directories.
				continue
			}
			return fmt.Errorf("index: legacy entry %q is not a regular file", name)
		}
		if (strings.HasPrefix(name, ".dirstat-") || strings.HasPrefix(name, ".marker-")) && strings.HasSuffix(name, ".tmp") {
			continue
		}
		if !validCacheName(name) {
			return fmt.Errorf("index: foreign legacy entry %q prevents adoption", name)
		}
		data, _, err := root.ReadRegular(name, s.recordLimit())
		if err != nil {
			return err
		}
		snap, _, inspectErr := Inspect(data)
		if inspectErr != nil || filepath.Base(s.pathFor(snap.Root, snap.Fingerprint)) != name {
			return fmt.Errorf("index: entry %q is not a recognized dirstat snapshot", name)
		}
	}
	return nil
}

func (s *Store) pathFor(root, fingerprint string) string {
	sum := sha256.Sum256([]byte(root + "\x00" + fingerprint))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16])+indexSnapshotExt)
}

// Load reads a root/fingerprint snapshot without following store links.
func (s *Store) Load(root, fingerprint string) (*Snapshot, error) {
	return s.LoadContext(context.Background(), root, fingerprint)
}

// LoadContext loads a complete snapshot through one stable, ownership-checked
// store capability and honors cancellation during large reads.
func (s *Store) LoadContext(ctx context.Context, root, fingerprint string) (*Snapshot, error) {
	storeRoot, err := storefs.OpenRoot(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = storeRoot.Close() }()
	if owned, issue := storeRoot.Ownership(stateKindIndex); !owned {
		if empty, emptyErr := rootHasOnlyLock(storeRoot); emptyErr != nil {
			return nil, emptyErr
		} else if empty {
			return nil, fs.ErrNotExist
		}
		return nil, fmt.Errorf("index: store is not owned: %s", issue)
	}
	data, _, err := storeRoot.ReadRegularContext(ctx, filepath.Base(s.pathFor(root, fingerprint)), s.recordLimit())
	if err != nil {
		return nil, err
	}
	snap, err := Unmarshal(data, fingerprint)
	if err != nil {
		return nil, err
	}
	if snap.Root != root {
		return nil, ErrIncompatible
	}
	if !snap.Complete || snap.Errors != 0 || snap.ScannedAt.After(s.currentTime().Add(time.Minute)) {
		return nil, ErrIncompatible
	}
	if snap.ScannedAt.Before(s.currentTime().Add(-s.policy.MaxAge)) {
		return nil, ErrStale
	}
	return snap, nil
}

func rootHasOnlyLock(root *storefs.Root) (bool, error) {
	entries, err := root.ReadDir()
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != indexLockName {
			return false, nil
		}
	}
	return true, nil
}

// Save atomically publishes only an explicitly complete snapshot, then applies
// the store-wide TTL and byte budget under the same cross-process lock.
func (s *Store) Save(snap *Snapshot) error {
	return s.SaveContext(context.Background(), snap)
}

// SaveContext is Save with caller cancellation threaded through lock waiting,
// publication, and policy pruning.
func (s *Store) SaveContext(parent context.Context, snap *Snapshot) error {
	parent = nonNilContext(parent)
	if snap == nil {
		return errors.New("index: cannot save a nil snapshot")
	}
	if snap.Root == "" || snap.Fingerprint == "" {
		return errors.New("index: snapshot root and fingerprint are required")
	}
	if !filepath.IsAbs(snap.Root) || snap.ScannedAt.IsZero() || snap.ScannedAt.After(s.currentTime().Add(time.Minute)) {
		return errors.New("index: snapshot root and scan timestamp are invalid")
	}
	if snap.ScannedAt.Before(s.currentTime().Add(-s.policy.MaxAge)) {
		return errors.New("index: snapshot is older than the retention TTL")
	}
	if !snap.Complete || snap.Errors != 0 {
		return fmt.Errorf("index: incomplete snapshot cannot be published (complete=%t errors=%d)", snap.Complete, snap.Errors)
	}
	if !snap.validTreeLayout() {
		return errors.New("index: snapshot tree layout is invalid")
	}
	data, err := snap.Marshal()
	if err != nil {
		return err
	}
	if int64(len(data)) > s.recordLimit() {
		return fmt.Errorf("index: snapshot is larger than record budget (%d > %d bytes)", len(data), s.recordLimit())
	}
	ctx := parent
	return storefs.WithLockChecked(ctx, s.dir, func(root *storefs.Root) error {
		return canInitializeIndexStore(root)
	}, func(root *storefs.Root) error {
		if owned, _ := root.Ownership(stateKindIndex); !owned {
			if err := root.EnsureOwnershipContext(ctx, stateKindIndex, false); err != nil {
				return fmt.Errorf("index: store is not owned: %w", err)
			}
		}
		destination := filepath.Base(s.pathFor(snap.Root, snap.Fingerprint))
		incoming := Entry{
			ID: destination, Root: snap.Root, Fingerprint: snap.Fingerprint,
			ScannedAt: snap.ScannedAt.UTC(), ModifiedAt: s.currentTime().UTC(),
			SizeBytes: int64(len(data)), Complete: true, Valid: true, Safe: true,
		}
		if err := root.AtomicWriteContext(ctx, destination, ".dirstat-*.tmp", data); err != nil {
			return err
		}
		if err := s.enforcePublishedWriteUnlocked(ctx, root, incoming); err != nil {
			return err
		}
		if _, err := root.Lstat(destination); err != nil {
			return fmt.Errorf("index: published snapshot was not retained: %w", err)
		}
		return nil
	})
}

func canInitializeIndexStore(root *storefs.Root) error {
	if owned, _ := root.Ownership(stateKindIndex); owned {
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
		if entry.Name() != indexLockName {
			return errors.New("index: store is unowned and non-empty; use state migrate --dry-run then --yes")
		}
	}
	return nil
}

// List discovers cache entries without creating the cache. Results are sorted
// by ID so text and JSON lifecycle output remain deterministic.
func (s *Store) List() ([]Entry, error) {
	return s.ListContext(context.Background())
}

// ListContext discovers cache entries with cancellable bounded reads.
func (s *Store) ListContext(ctx context.Context) ([]Entry, error) {
	root, err := storefs.OpenRoot(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return s.listRoot(ctx, root, false)
}

func (s *Store) listRoot(ctx context.Context, root *storefs.Root, trustLegacy bool) ([]Entry, error) {
	owned, ownershipIssue := root.Ownership(stateKindIndex)
	if trustLegacy {
		owned, ownershipIssue = true, ""
	}
	entries, err := root.ReadDir()
	if err != nil {
		return nil, err
	}
	result := make([]Entry, 0, len(entries))
	for _, directoryEntry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		name := directoryEntry.Name()
		if name == indexLockName || name == indexMarkerName {
			continue
		}
		if name == "history" && quiescentOwnedHistory(root, name) {
			continue
		}
		ownedTemp := (strings.HasPrefix(name, ".dirstat-") || strings.HasPrefix(name, ".marker-")) && strings.HasSuffix(name, ".tmp")
		entry := Entry{ID: name, Safe: owned && (validCacheName(name) || ownedTemp)}
		info, statErr := root.Lstat(name)
		if statErr != nil {
			entry.Issue = statErr.Error()
			result = append(result, entry)
			continue
		}
		if !owned && entry.Issue == "" {
			entry.Issue = ownershipIssue
		}
		entry.ModifiedAt, entry.SizeBytes = info.ModTime().UTC(), info.Size()
		if ownedTemp {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				entry.Safe = false
				entry.Issue = "temporary entry is not a regular file"
			} else {
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
		data, opened, readErr := root.ReadRegularContext(ctx, name, s.recordLimit())
		if readErr != nil {
			entry.Issue = readErr.Error()
			result = append(result, entry)
			continue
		}
		entry.ModifiedAt, entry.SizeBytes = opened.ModTime().UTC(), opened.Size()
		snap, decodeErr := Unmarshal(data, "")
		if decodeErr != nil {
			entry.Issue = decodeErr.Error()
			result = append(result, entry)
			continue
		}
		entry.Root = snap.Root
		entry.Fingerprint = snap.Fingerprint
		entry.ScannedAt = snap.ScannedAt.UTC()
		entry.Complete = snap.Complete && snap.Errors == 0
		switch {
		case filepath.Base(s.pathFor(snap.Root, snap.Fingerprint)) != name:
			entry.Issue = "snapshot key does not match filename"
		case snap.ScannedAt.After(s.currentTime().Add(time.Minute)):
			entry.Issue = "snapshot timestamp is in the future"
		case !entry.Complete:
			entry.Issue = "incomplete snapshot"
		default:
			entry.Valid = entry.Safe
		}
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// quiescentOwnedHistory recognizes the marker/lock and empty key directories
// left by an explicit legacy-history migration. Cache lifecycle never deletes
// the nested store; treating its verified empty infrastructure as payload
// would make a successful all-state migration report a false unsafe residue.
func quiescentOwnedHistory(root *storefs.Root, name string) bool {
	historyRoot, err := root.OpenDir(name)
	if err != nil {
		return false
	}
	defer func() { _ = historyRoot.Close() }()
	if owned, _ := historyRoot.Ownership("history"); !owned {
		return false
	}
	entries, err := historyRoot.ReadDir()
	if err != nil {
		return false
	}
	for _, entry := range entries {
		entryName := entry.Name()
		if entryName == indexLockName || entryName == indexMarkerName {
			continue
		}
		if len(entryName) != 32 {
			return false
		}
		if _, err := hex.DecodeString(entryName); err != nil {
			return false
		}
		keyRoot, err := historyRoot.OpenDir(entryName)
		if err != nil {
			return false
		}
		children, readErr := keyRoot.ReadDir()
		closeErr := keyRoot.Close()
		if readErr != nil || closeErr != nil || len(children) != 0 {
			return false
		}
	}
	return true
}

// Prune applies global TTL and byte policy. A dry run is read-only.
func (s *Store) Prune(dryRun bool) ([]Action, error) {
	return s.PruneContext(context.Background(), dryRun)
}

// PruneContext is Prune with cancellation applied before every removal.
func (s *Store) PruneContext(parent context.Context, dryRun bool) ([]Action, error) {
	return s.runLifecycleContext(parent, dryRun, "prune", s.pruneUnlocked)
}

// Clear removes every safely owned cache file. A dry run is read-only.
func (s *Store) Clear(dryRun bool) ([]Action, error) {
	return s.ClearContext(context.Background(), dryRun)
}

// ClearContext is Clear with cancellation applied before every removal.
func (s *Store) ClearContext(parent context.Context, dryRun bool) ([]Action, error) {
	return s.runLifecycleContext(parent, dryRun, "clear", s.clearUnlocked)
}

func (s *Store) runLifecycleContext(
	parent context.Context,
	dryRun bool,
	operation string,
	run func(context.Context, *storefs.Root, bool) ([]Action, error),
) ([]Action, error) {
	parent = nonNilContext(parent)
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
		if owned, issue := root.Ownership(stateKindIndex); !owned {
			return nil, fmt.Errorf("index: refusing to %s unowned store: %s", operation, issue)
		}
		return run(parent, root, true)
	}
	ctx := parent
	var actions []Action
	err = s.withOwnedLock(ctx, operation, func(root *storefs.Root) error {
		if owned, issue := root.Ownership(stateKindIndex); !owned {
			return fmt.Errorf("index: refusing to %s unowned store: %s", operation, issue)
		}
		var runErr error
		actions, runErr = run(ctx, root, false)
		return runErr
	})
	return actions, err
}

// Invalidate removes safely owned incompatible or incomplete cache entries
// while retaining valid current-format snapshots. It is the cache migration
// contract across snapshot-format bumps.
func (s *Store) Invalidate(dryRun bool) ([]Action, error) {
	return s.InvalidateContext(context.Background(), dryRun)
}

// InvalidateContext is Invalidate with caller cancellation.
func (s *Store) InvalidateContext(parent context.Context, dryRun bool) ([]Action, error) {
	parent = nonNilContext(parent)
	exists, err := storefs.CheckDir(s.dir)
	if err != nil || !exists {
		return nil, err
	}
	run := func() ([]Action, error) {
		root, openErr := storefs.OpenRoot(s.dir)
		if openErr != nil {
			return nil, openErr
		}
		defer func() { _ = root.Close() }()
		if owned, issue := root.Ownership(stateKindIndex); !owned {
			return nil, fmt.Errorf("index: refusing to invalidate unowned store: %s", issue)
		}
		entries, listErr := s.listRoot(parent, root, false)
		if listErr != nil {
			return nil, listErr
		}
		remove := make(map[string]string)
		for _, entry := range entries {
			if entry.Safe && !entry.Valid {
				remove[entry.ID] = "invalidate"
			}
		}
		return s.applyRemovals(parent, root, entries, remove, dryRun)
	}
	if dryRun {
		return run()
	}
	ctx := parent
	var actions []Action
	err = s.withOwnedLock(ctx, "invalidate", func(root *storefs.Root) error {
		if owned, issue := root.Ownership(stateKindIndex); !owned {
			return fmt.Errorf("index: refusing to invalidate unowned store: %s", issue)
		}
		entries, listErr := s.listRoot(ctx, root, false)
		if listErr != nil {
			return listErr
		}
		remove := make(map[string]string)
		for _, entry := range entries {
			if entry.Safe && !entry.Valid {
				remove[entry.ID] = "invalidate"
			}
		}
		var invalidateErr error
		actions, invalidateErr = s.applyRemovals(ctx, root, entries, remove, false)
		return invalidateErr
	})
	return actions, err
}

// withOwnedLock performs the read-only ownership preflight and lock acquisition
// through one stable Root capability. A raced pathname replacement therefore
// cannot receive the lock file before the under-lock ownership recheck.
func (s *Store) withOwnedLock(ctx context.Context, operation string, fn func(*storefs.Root) error) error {
	root, err := storefs.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if owned, issue := root.Ownership(stateKindIndex); !owned {
		return fmt.Errorf("index: refusing to %s unowned store: %s", operation, issue)
	}
	return root.WithLockContext(ctx, fn)
}

func (s *Store) pruneUnlocked(ctx context.Context, root *storefs.Root, dryRun bool) ([]Action, error) {
	entries, err := s.listRoot(ctx, root, false)
	if err != nil {
		return nil, err
	}
	remove, err := s.prunePlan(entries, nil)
	if err != nil {
		return nil, err
	}
	return s.applyRemovals(ctx, root, entries, remove, dryRun)
}

// enforcePublishedWriteUnlocked applies final TTL/quota policy only after the
// new snapshot has been atomically published. A stage, fsync, rename, or
// cancellation failure therefore leaves every prior cache entry intact.
func (s *Store) enforcePublishedWriteUnlocked(ctx context.Context, root *storefs.Root, incoming Entry) error {
	entries, err := s.listRoot(ctx, root, false)
	if err != nil {
		return err
	}
	remove, err := s.prunePlan(entries, &incoming)
	if err != nil {
		return err
	}
	_, err = s.applyRemovals(ctx, root, entries, remove, false)
	return err
}

func (s *Store) prunePlan(entries []Entry, incoming *Entry) (map[string]string, error) {
	remove := make(map[string]string)
	cutoff := s.currentTime().Add(-s.policy.MaxAge)
	var retained []Entry
	foundIncoming := false
	for _, entry := range entries {
		if !entry.Safe {
			if incoming != nil && entry.ID == incoming.ID {
				return nil, fmt.Errorf("index: destination %q is unsafe and cannot be replaced", entry.ID)
			}
			continue
		}
		if incoming != nil && entry.ID == incoming.ID {
			retained = append(retained, *incoming)
			foundIncoming = true
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
	if incoming != nil {
		if incoming.SizeBytes < 0 || incoming.SizeBytes > s.policy.MaxBytes {
			return nil, errors.New("index: incoming snapshot exceeds the store quota")
		}
		if !foundIncoming {
			return nil, errors.New("index: published snapshot is missing from store inventory")
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
	if incoming != nil {
		used = incoming.SizeBytes
		kept[incoming.ID] = true
	}
	firstByRoot := make(map[string]Entry)
	for _, entry := range retained {
		if _, ok := firstByRoot[entry.Root]; !ok {
			firstByRoot[entry.Root] = entry
		}
	}
	roots := make([]string, 0, len(firstByRoot))
	for root := range firstByRoot {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	for _, root := range roots {
		entry := firstByRoot[root]
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
	return remove, nil
}

func (s *Store) clearUnlocked(ctx context.Context, root *storefs.Root, dryRun bool) ([]Action, error) {
	entries, err := s.listRoot(ctx, root, false)
	if err != nil {
		return nil, err
	}
	remove := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.Safe {
			remove[entry.ID] = "clear"
		}
	}
	return s.applyRemovals(ctx, root, entries, remove, dryRun)
}

func (*Store) applyRemovals(ctx context.Context, root *storefs.Root, entries []Entry, remove map[string]string, dryRun bool) ([]Action, error) {
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
			if err := root.RemoveRegularContext(ctx, entry.ID); err != nil {
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
		}
		actions = append(actions, action)
	}
	return actions, nil
}

func validCacheName(name string) bool {
	if filepath.Ext(name) != indexSnapshotExt || len(name) != 36 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimSuffix(name, indexSnapshotExt))
	return err == nil
}

func (s *Store) recordLimit() int64 {
	if s.policy.MaxBytes < MaxRecordBytes {
		return s.policy.MaxBytes
	}
	return MaxRecordBytes
}

func (s *Store) currentTime() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// Age returns how long ago the snapshot was produced.
func Age(snap *Snapshot) time.Duration {
	if snap == nil {
		return 0
	}
	age := time.Since(snap.ScannedAt)
	if age < 0 {
		return 0
	}
	return age
}

// IsMissing reports whether err means no cache file is present.
func IsMissing(err error) bool { return errors.Is(err, fs.ErrNotExist) }
