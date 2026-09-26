package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Store durably records committed configurations. Only full committed
// snapshots are persisted; uncommitted builder edits never reach the store,
// so after a restart the service recovers exactly the last successfully
// committed version and never an intermediate edit.
type Store interface {
	// Save persists one complete snapshot. Implementations must make the
	// write atomic: a crash during Save leaves either the previous record or
	// the new one, never a torn file.
	Save(v Version, createdAt time.Time, spec ConfigSpec) error
	// Load returns the last persisted record, or ErrNoCommitted when empty.
	Load() (v Version, createdAt time.Time, spec ConfigSpec, err error)
}

// persistedRecord is the on-disk / in-memory envelope. The entire committed
// configuration is one JSON document so it can only be read back whole.
type persistedRecord struct {
	Version   Version    `json:"version"`
	CreatedAt time.Time  `json:"created_at"`
	Spec      ConfigSpec `json:"spec"`
}

// MemoryStore keeps the last committed snapshot in memory. It is safe for
// concurrent use and primarily intended for tests.
type MemoryStore struct {
	rec *persistedRecord
}

// NewMemoryStore creates an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// Save replaces the in-memory record atomically.
func (s *MemoryStore) Save(v Version, createdAt time.Time, spec ConfigSpec) error {
	s.rec = &persistedRecord{Version: v, CreatedAt: createdAt, Spec: cloneSpec(spec)}
	return nil
}

// Load returns the stored record or ErrNoCommitted.
func (s *MemoryStore) Load() (Version, time.Time, ConfigSpec, error) {
	if s.rec == nil {
		return 0, time.Time{}, ConfigSpec{}, ErrNoCommitted
	}
	return s.rec.Version, s.rec.CreatedAt, cloneSpec(s.rec.Spec), nil
}

func cloneSpec(s ConfigSpec) ConfigSpec {
	out := ConfigSpec{
		Resources: make([]ResourceConfig, len(s.Resources)),
		Groups:    make([]GroupSpec, len(s.Groups)),
	}
	copy(out.Resources, s.Resources)
	for i, g := range s.Groups {
		g.Hard = cloneInt64Map(g.Hard)
		g.Soft = cloneInt64Map(g.Soft)
		out.Groups[i] = g
	}
	return out
}

// FileStore persists snapshots as a single JSON file. Writes go to a
// temporary file in the same directory and are atomically renamed into
// place, so readers (including a restarting process) can only ever observe a
// complete document.
type FileStore struct {
	path string
}

// NewFileStore creates a file-backed store at path.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

// Save atomically writes one record.
func (f *FileStore) Save(v Version, createdAt time.Time, spec ConfigSpec) error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return fmt.Errorf("quota: create store dir: %w", err)
	}
	rec := persistedRecord{Version: v, CreatedAt: createdAt, Spec: spec}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("quota: marshal snapshot: %w", err)
	}
	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".quota-*.tmp")
	if err != nil {
		return fmt.Errorf("quota: create temp store: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("quota: write temp store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("quota: sync temp store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("quota: close temp store: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return fmt.Errorf("quota: rename store: %w", err)
	}
	return nil
}

// Load reads and decodes the record file.
func (f *FileStore) Load() (Version, time.Time, ConfigSpec, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, time.Time{}, ConfigSpec{}, ErrNoCommitted
		}
		return 0, time.Time{}, ConfigSpec{}, fmt.Errorf("quota: read store: %w", err)
	}
	var rec persistedRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return 0, time.Time{}, ConfigSpec{}, fmt.Errorf("quota: corrupt store: %w", err)
	}
	return rec.Version, rec.CreatedAt, rec.Spec, nil
}
