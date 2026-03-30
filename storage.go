package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

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
	Save(shortCode, longURL string) error

	// GetByCode retrieves the record for a short code.
	// Returns (nil, nil) when the code does not exist.
	GetByCode(shortCode string) (*URLRecord, error)

	// IncrementClicks atomically increments the click counter for a short
	// code. It is a no-op (returns nil) if the code does not exist.
	IncrementClicks(shortCode string) error

	// GetAll returns a snapshot of every stored record in arbitrary order.
	GetAll() []URLRecord
}

// JSONStore is a thread-safe, in-memory Store backed by a JSON file on disk.
// All records are kept in a map for O(1) lookups; writes are flushed to the
// file immediately so data survives restarts. It is suitable for low-to-medium
// traffic; for high-volume deployments swap in a database-backed implementation
// of the Store interface without modifying any handler code.
type JSONStore struct {
	mu       sync.RWMutex
	records  map[string]*URLRecord
	filePath string
}

// NewJSONStore opens the JSON store at filePath, loading any previously saved
// records into memory. If the file does not exist it is created on the first
// write. Returns an error if the file exists but cannot be read or parsed.
func NewJSONStore(filePath string) (*JSONStore, error) {
	s := &JSONStore{
		records:  make(map[string]*URLRecord),
		filePath: filePath,
	}

	data, err := os.ReadFile(filePath)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		var list []URLRecord
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, err
		}
		for i := range list {
			r := list[i]
			s.records[r.ShortCode] = &r
		}
	}

	return s, nil
}

// save writes the current state to disk. Must be called with the write lock held.
func (s *JSONStore) save() error {
	list := make([]URLRecord, 0, len(s.records))
	for _, r := range s.records {
		list = append(list, *r)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filePath, data, 0644)
}

// Save inserts a new short code → long URL mapping.
// Returns errDuplicate if the short code already exists.
func (s *JSONStore) Save(shortCode, longURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.records[shortCode]; exists {
		return errDuplicate
	}

	s.records[shortCode] = &URLRecord{
		ShortCode: shortCode,
		LongURL:   longURL,
		CreatedAt: time.Now(),
		Clicks:    0,
	}
	return s.save()
}

// GetByCode retrieves a URLRecord by its short code. Returns nil, nil if not found.
func (s *JSONStore) GetByCode(shortCode string) (*URLRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	r, ok := s.records[shortCode]
	if !ok {
		return nil, nil
	}
	copy := *r
	return &copy, nil
}

// IncrementClicks increments the click counter for a short code.
func (s *JSONStore) IncrementClicks(shortCode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.records[shortCode]
	if !ok {
		return nil
	}
	r.Clicks++
	return s.save()
}

// GetAll returns a snapshot of all URL records.
func (s *JSONStore) GetAll() []URLRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := make([]URLRecord, 0, len(s.records))
	for _, r := range s.records {
		list = append(list, *r)
	}
	return list
}
