package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// URLRecord represents a stored short URL mapping.
type URLRecord struct {
	ShortCode string    `json:"short_code"`
	LongURL   string    `json:"long_url"`
	CreatedAt time.Time `json:"created_at"`
	Clicks    int64     `json:"clicks"`
}

// Store is the interface for URL storage operations.
type Store interface {
	Save(shortCode, longURL string) error
	GetByCode(shortCode string) (*URLRecord, error)
	IncrementClicks(shortCode string) error
	GetAll() []URLRecord
}

// JSONStore is an in-memory store backed by a JSON file for persistence.
type JSONStore struct {
	mu       sync.RWMutex
	records  map[string]*URLRecord
	filePath string
}

// NewJSONStore loads (or creates) the JSON store at the given file path.
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
