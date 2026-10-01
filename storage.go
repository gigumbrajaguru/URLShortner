package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// errDuplicate is returned by Store.Save when the short code already exists.
var errDuplicate = errors.New("duplicate short code")

// URLRecord is the data model for a single short URL entry.
// It is serialised to JSON for persistence and returned by the /info endpoint.
type URLRecord struct {
	ShortCode string    `json:"short_code" bson:"short_code"` // 6-character alphanumeric identifier
	LongURL   string    `json:"long_url"   bson:"long_url"`   // original destination URL
	CreatedAt time.Time `json:"created_at" bson:"created_at"` // UTC timestamp of creation
	Clicks    int64     `json:"clicks"     bson:"clicks"`     // total number of redirect visits
}

// Store defines the persistence contract for URL records. Implementations must
// be safe for concurrent use by multiple goroutines.
type Store interface {
	// Save persists a new shortCode → longURL mapping. Returns errDuplicate
	// if the short code is already in use.
	Save(ctx context.Context, shortCode, longURL string) error

	// GetByCode retrieves the record for a short code.
	// Returns (nil, nil) when the code does not exist.
	GetByCode(ctx context.Context, shortCode string) (*URLRecord, error)

	// IncrementClicks atomically increments the click counter for a short
	// code and reports whether the code exists.
	IncrementClicks(ctx context.Context, shortCode string) (bool, error)

	// Ping reports whether the store is usable; it backs the health check.
	Ping(ctx context.Context) error

	// Close flushes any pending writes and releases resources.
	Close(ctx context.Context) error
}

// flushInterval is how often JSONStore persists click-count changes. New
// links are written immediately; clicks are batched so a popular link does
// not rewrite the whole file on every visit.
const flushInterval = time.Second

// JSONStore is a thread-safe, in-memory Store backed by a JSON file on disk.
// All records are kept in a map for O(1) lookups. New links are written to
// disk before Save returns; click counts are flushed in the background every
// flushInterval and on Close. Files are replaced atomically (write to a temp
// file, then rename) so a crash never leaves a truncated store behind.
//
// It is suitable for low-to-medium traffic; for high-volume deployments use
// MongoStore or another implementation of the Store interface.
type JSONStore struct {
	mu      sync.RWMutex
	records map[string]*URLRecord
	version uint64 // bumped on every mutation; guarded by mu

	fileMu   sync.Mutex    // serialises writes to filePath
	written  atomic.Uint64 // version of the snapshot last written to disk
	filePath string

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewJSONStore opens the JSON store at filePath, loading any previously saved
// records into memory. Missing parent directories and the file itself are
// created immediately, so permission problems surface at startup rather than
// as failed requests later. Returns an error if the file cannot be read,
// parsed, or written.
func NewJSONStore(filePath string) (*JSONStore, error) {
	s := &JSONStore{
		records:  make(map[string]*URLRecord),
		version:  1, // ahead of written, so the startup flush below always writes
		filePath: filePath,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}

	data, err := os.ReadFile(filePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case len(bytes.TrimSpace(data)) > 0:
		var list []URLRecord
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("parse %s: %w", filePath, err)
		}
		for i := range list {
			s.records[list[i].ShortCode] = &list[i]
		}
	}

	if err := s.flush(); err != nil {
		return nil, fmt.Errorf("write %s: %w", filePath, err)
	}

	go s.flushLoop()
	return s, nil
}

// Save inserts a new short code → long URL mapping and writes it to disk
// before returning. Returns errDuplicate if the short code already exists.
// If the write fails the record is removed again, so memory never serves a
// link that was not persisted.
func (s *JSONStore) Save(_ context.Context, shortCode, longURL string) error {
	s.mu.Lock()
	if _, exists := s.records[shortCode]; exists {
		s.mu.Unlock()
		return errDuplicate
	}
	s.records[shortCode] = &URLRecord{
		ShortCode: shortCode,
		LongURL:   longURL,
		CreatedAt: time.Now().UTC(),
	}
	s.version++
	list, version := s.snapshotLocked()
	s.mu.Unlock()

	if err := s.writeSnapshot(list, version); err != nil {
		s.mu.Lock()
		delete(s.records, shortCode)
		s.version++
		s.mu.Unlock()
		return err
	}
	return nil
}

// GetByCode retrieves a URLRecord by its short code. Returns nil, nil if not found.
func (s *JSONStore) GetByCode(_ context.Context, shortCode string) (*URLRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	r, ok := s.records[shortCode]
	if !ok {
		return nil, nil
	}
	rec := *r
	return &rec, nil
}

// IncrementClicks increments the click counter for a short code and reports
// whether it exists. The new count is persisted by the background flusher.
func (s *JSONStore) IncrementClicks(_ context.Context, shortCode string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.records[shortCode]
	if !ok {
		return false, nil
	}
	r.Clicks++
	s.version++
	return true, nil
}

// Ping always succeeds: the store lives in memory and write failures are
// reported by Save itself.
func (s *JSONStore) Ping(context.Context) error { return nil }

// Close stops the background flusher and writes any pending changes.
// It is safe to call more than once.
func (s *JSONStore) Close(context.Context) error {
	s.closeOnce.Do(func() { close(s.stop) })
	<-s.done
	return s.flush()
}

// flushLoop periodically persists changes until Close is called.
func (s *JSONStore) flushLoop() {
	defer close(s.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			if err := s.flush(); err != nil {
				log.Printf("json store: flush %s: %v", s.filePath, err)
			}
		}
	}
}

// flush writes the current state to disk if it changed since the last write.
func (s *JSONStore) flush() error {
	s.mu.RLock()
	if s.version == s.written.Load() {
		s.mu.RUnlock()
		return nil
	}
	list, version := s.snapshotLocked()
	s.mu.RUnlock()
	return s.writeSnapshot(list, version)
}

// snapshotLocked copies every record so it can be encoded without holding
// the lock. The caller must hold s.mu.
func (s *JSONStore) snapshotLocked() ([]URLRecord, uint64) {
	list := make([]URLRecord, 0, len(s.records))
	for _, r := range s.records {
		list = append(list, *r)
	}
	return list, s.version
}

// writeSnapshot persists a snapshot taken at the given version. A snapshot
// holds every mutation up to its version, so one that is not newer than the
// file already on disk is skipped: that file covers it.
func (s *JSONStore) writeSnapshot(list []URLRecord, version uint64) error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	if version <= s.written.Load() {
		return nil
	}
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.filePath, data); err != nil {
		return err
	}
	s.written.Store(version)
	return nil
}

// writeFileAtomic replaces path with data via a synced temp file and a
// rename, so readers and crashes only ever see the old or the new content.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
