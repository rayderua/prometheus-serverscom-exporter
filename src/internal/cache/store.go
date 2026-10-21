package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// fileFormatVersion is bumped on incompatible changes of the cache file;
// files with another version are ignored on load.
const fileFormatVersion = 2

// Store publishes snapshots and optionally persists them to disk, so that a
// restart does not have to download everything again.
type Store struct {
	current atomic.Pointer[Snapshot]

	// writeMu serialises writers; readers never take it.
	writeMu sync.Mutex
	dirty   atomic.Bool

	path   string
	logger *slog.Logger
}

// NewStore creates a store. An empty path disables persistence.
func NewStore(path string, logger *slog.Logger) *Store {
	s := &Store{path: path, logger: logger}
	s.current.Store(NewSnapshot())
	return s
}

// Snapshot returns the current snapshot. It must not be modified.
func (s *Store) Snapshot() *Snapshot {
	return s.current.Load()
}

// Update applies fn to a copy of the current snapshot and publishes the
// result. fn may add, replace or delete map entries, but must not modify
// values it got from the snapshot (see Snapshot).
func (s *Store) Update(fn func(*Snapshot)) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	next := s.current.Load().clone()
	fn(next)
	s.current.Store(next)
	s.dirty.Store(true)
}

// cacheFile is the on-disk representation.
type cacheFile struct {
	Version  int       `json:"version"`
	SavedAt  time.Time `json:"saved_at"`
	Snapshot *Snapshot `json:"snapshot"`
}

// Load reads the cache file, if persistence is enabled and the file exists.
// A missing, unreadable or outdated file is not an error: the exporter just
// starts with an empty cache.
func (s *Store) Load() {
	if s.path == "" {
		return
	}

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.logger.Info("Cache file does not exist yet, starting empty", "path", s.path)
		return
	}
	if err != nil {
		s.logger.Warn("Cannot read cache file, starting empty", "path", s.path, "err", err)
		return
	}

	var file cacheFile
	if err := json.Unmarshal(data, &file); err != nil || file.Snapshot == nil {
		s.logger.Warn("Cannot parse cache file, starting empty", "path", s.path, "err", err)
		return
	}
	if file.Version != fileFormatVersion {
		s.logger.Info("Cache file has another format version, starting empty",
			"path", s.path, "version", file.Version, "expected", fileFormatVersion)
		return
	}

	file.Snapshot.ensureMaps()
	s.current.Store(file.Snapshot)
	s.logger.Info("Loaded cache file", "path", s.path, "saved_at", file.SavedAt,
		"hosts", len(file.Snapshot.Hosts), "l2_segments", len(file.Snapshot.Segments))
}

// Save writes the current snapshot if it changed since the last save.
// The file is replaced atomically, so a crash never leaves a broken file.
func (s *Store) Save() error {
	if s.path == "" || !s.dirty.Swap(false) {
		return nil
	}

	data, err := json.Marshal(cacheFile{
		Version:  fileFormatVersion,
		SavedAt:  time.Now(),
		Snapshot: s.current.Load(),
	})
	if err != nil {
		s.dirty.Store(true)
		return fmt.Errorf("encode cache: %w", err)
	}

	if err := writeFileAtomic(s.path, data); err != nil {
		s.dirty.Store(true)
		return err
	}
	s.logger.Debug("Cache saved", "path", s.path, "bytes", len(data))
	return nil
}

// RunPersister saves the cache every interval until ctx is cancelled,
// and once more on the way out.
func (s *Store) RunPersister(ctx context.Context, interval time.Duration) {
	if s.path == "" {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if err := s.Save(); err != nil {
				s.logger.Error("Cannot save cache on shutdown", "path", s.path, "err", err)
			}
			return
		case <-ticker.C:
			if err := s.Save(); err != nil {
				s.logger.Error("Cannot save cache", "path", s.path, "err", err)
			}
		}
	}
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp cache file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close cache file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace cache file: %w", err)
	}
	return nil
}
