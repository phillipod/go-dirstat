package index

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Store persists snapshots in a single directory.
type Store struct {
	dir string
}

// NewStore returns a Store rooted at <user cache dir>/dirstat, creating it.
func NewStore() (*Store, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "dirstat")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// pathFor derives a unique, filesystem-safe filename from the complete cache
// key. Hashing both values keeps names bounded and prevents a malformed
// fingerprint from introducing path separators.
func (s *Store) pathFor(root, fingerprint string) string {
	sum := sha256.Sum256([]byte(root + "\x00" + fingerprint))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16])+".bin")
}

// Load reads a snapshot for root/fingerprint. A missing file yields an error
// satisfying errors.Is(err, fs.ErrNotExist).
func (s *Store) Load(root, fingerprint string) (*Snapshot, error) {
	data, err := os.ReadFile(s.pathFor(root, fingerprint))
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
	return snap, nil
}

// Save atomically writes snap using its root and fingerprint as the cache key.
func (s *Store) Save(snap *Snapshot) error {
	if snap == nil {
		return errors.New("index: cannot save a nil snapshot")
	}
	if snap.Root == "" || snap.Fingerprint == "" {
		return errors.New("index: snapshot root and fingerprint are required")
	}
	if !snap.validTreeLayout() {
		return errors.New("index: snapshot tree layout is invalid")
	}
	data, err := snap.Marshal()
	if err != nil {
		return err
	}

	destination := s.pathFor(snap.Root, snap.Fingerprint)
	tmp, err := os.CreateTemp(s.dir, ".dirstat-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
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
	return os.Rename(tmpName, destination)
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
