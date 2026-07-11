// Package history stores a bounded sequence of scan snapshots and compares
// them to expose growth over time. It deliberately reuses index.Snapshot's
// versioned encoding so history does not create a second scan-data format.
package history

import (
	"bytes"
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

	"github.com/phillipod/go-dirstat/internal/index"
)

const (
	// MaxRecords is the maximum number of snapshots retained for one root and
	// scope fingerprint.
	MaxRecords = 20
	// MaxAge is the maximum snapshot age retained when a new record is written.
	MaxAge = 30 * 24 * time.Hour
)

// Store persists history below a private directory.
type Store struct {
	dir string
	now func() time.Time
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

// NewStore creates the default history store under the user's cache directory.
func NewStore() (*Store, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	return NewStoreAt(filepath.Join(base, "dirstat", "history"))
}

// NewStoreAt creates a history store at dir. It is useful for applications
// that configure a state location and for isolated tests.
func NewStoreAt(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("history: store directory is required")
	}
	abs, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: abs, now: time.Now}, nil
}

// RecordSnapshot atomically stores snap, then applies the per-root/fingerprint
// retention limits. Recording the same snapshot twice is idempotent.
func (s *Store) RecordSnapshot(snap *index.Snapshot) (Record, error) {
	if snap == nil || snap.Root == "" || snap.Fingerprint == "" || snap.ScannedAt.IsZero() || len(snap.Nodes) == 0 {
		return Record{}, errors.New("history: complete snapshot is required")
	}
	data, err := snap.Marshal()
	if err != nil {
		return Record{}, err
	}
	validated, err := index.Unmarshal(data, snap.Fingerprint)
	if err != nil || validated.Root != snap.Root {
		return Record{}, errors.New("history: snapshot is invalid")
	}
	dir := s.keyDir(snap.Root, snap.Fingerprint)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Record{}, err
	}
	id := snap.ScannedAt.UTC().Format("20060102T150405.000000000Z")
	destination := filepath.Join(dir, id+".bin")
	if err := atomicWrite(dir, destination, data); err != nil {
		return Record{}, err
	}
	if err := s.prune(snap.Root, snap.Fingerprint); err != nil {
		return Record{}, err
	}
	return describe(id, snap), nil
}

// List returns retained snapshots newest first. Foreign, malformed, and
// incompatible files are ignored so a damaged record does not hide good ones.
func (s *Store) List(root, fingerprint string) ([]Record, error) {
	dir := s.keyDir(root, fingerprint)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".bin" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".bin")
		if !validID(id) {
			continue
		}
		snap, err := s.loadPath(filepath.Join(dir, entry.Name()), root, fingerprint)
		if err != nil {
			continue
		}
		records = append(records, describe(id, snap))
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ScannedAt.After(records[j].ScannedAt) })
	return records, nil
}

// Load reads a listed record and verifies both its key and encoded identity.
func (s *Store) Load(record Record) (*index.Snapshot, error) {
	if record.Root == "" || record.Fingerprint == "" || !validID(record.ID) {
		return nil, errors.New("history: invalid record")
	}
	path := filepath.Join(s.keyDir(record.Root, record.Fingerprint), record.ID+".bin")
	return s.loadPath(path, record.Root, record.Fingerprint)
}

// Previous loads the newest snapshot scanned strictly before before. A zero
// time selects the newest retained snapshot.
func (s *Store) Previous(root, fingerprint string, before time.Time) (*index.Snapshot, error) {
	records, err := s.List(root, fingerprint)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if before.IsZero() || record.ScannedAt.Before(before) {
			return s.Load(record)
		}
	}
	return nil, fs.ErrNotExist
}

func (s *Store) prune(root, fingerprint string) error {
	records, err := s.List(root, fingerprint)
	if err != nil {
		return err
	}
	cutoff := s.now().Add(-MaxAge)
	for i, record := range records {
		if i < MaxRecords && !record.ScannedAt.Before(cutoff) {
			continue
		}
		path := filepath.Join(s.keyDir(root, fingerprint), record.ID+".bin")
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) loadPath(path, root, fingerprint string) (*index.Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	snap, err := index.Unmarshal(data, fingerprint)
	if err != nil {
		return nil, err
	}
	if snap.Root != root {
		return nil, index.ErrIncompatible
	}
	return snap, nil
}

func (s *Store) keyDir(root, fingerprint string) string {
	sum := sha256.Sum256([]byte(root + "\x00" + fingerprint))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16]))
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

func atomicWrite(dir, destination string, data []byte) error {
	if existing, err := os.ReadFile(destination); err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return errors.New("history: scan timestamp collides with a different snapshot")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".history-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, destination); err != nil {
		return fmt.Errorf("publish history: %w", err)
	}
	return nil
}
