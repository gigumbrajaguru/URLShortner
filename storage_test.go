package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// testStoreContract checks the behaviour every Store implementation must share.
func testStoreContract(t *testing.T, s Store) {
	ctx := context.Background()

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	before := time.Now().Add(-time.Second)
	if err := s.Save(ctx, "abc123", "https://example.com/a"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Save(ctx, "abc123", "https://example.com/b"); !errors.Is(err, errDuplicate) {
		t.Fatalf("Save duplicate: got %v, want errDuplicate", err)
	}

	rec, err := s.GetByCode(ctx, "abc123")
	if err != nil || rec == nil {
		t.Fatalf("GetByCode: rec=%v err=%v", rec, err)
	}
	if rec.ShortCode != "abc123" || rec.LongURL != "https://example.com/a" || rec.Clicks != 0 {
		t.Errorf("GetByCode: unexpected record %+v", rec)
	}
	if rec.CreatedAt.Before(before) || rec.CreatedAt.After(time.Now().Add(time.Second)) {
		t.Errorf("CreatedAt %v is not around now", rec.CreatedAt)
	}

	if rec, err := s.GetByCode(ctx, "nope00"); rec != nil || err != nil {
		t.Errorf("GetByCode missing: rec=%v err=%v, want nil, nil", rec, err)
	}

	for i := 0; i < 3; i++ {
		found, err := s.IncrementClicks(ctx, "abc123")
		if err != nil || !found {
			t.Fatalf("IncrementClicks: found=%v err=%v", found, err)
		}
	}
	if found, err := s.IncrementClicks(ctx, "nope00"); found || err != nil {
		t.Errorf("IncrementClicks missing: found=%v err=%v, want false, nil", found, err)
	}
	if rec, _ := s.GetByCode(ctx, "abc123"); rec == nil || rec.Clicks != 3 {
		t.Errorf("clicks = %+v, want 3", rec)
	}
}

func newTestJSONStore(t *testing.T) (*JSONStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "urls.json")
	s, err := NewJSONStore(path)
	if err != nil {
		t.Fatalf("NewJSONStore: %v", err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s, path
}

func readStoreFile(t *testing.T, path string) map[string]URLRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store file: %v", err)
	}
	var list []URLRecord
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("parse store file: %v", err)
	}
	m := make(map[string]URLRecord, len(list))
	for _, r := range list {
		m[r.ShortCode] = r
	}
	return m
}

func TestJSONStoreContract(t *testing.T) {
	s, _ := newTestJSONStore(t)
	testStoreContract(t, s)
}

func TestMongoStoreContract(t *testing.T) {
	uri := os.Getenv("MONGODB_TEST_URI")
	if uri == "" {
		t.Skip("set MONGODB_TEST_URI to run MongoDB tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	coll := fmt.Sprintf("urls_test_%d", time.Now().UnixNano())
	s, err := NewMongoStore(ctx, uri, "urlshortner_test", coll)
	if err != nil {
		t.Fatalf("NewMongoStore: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		s.collection.Drop(ctx)
		s.Close(ctx)
	})
	testStoreContract(t, s)
}

func TestMongoStoreUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := NewMongoStore(ctx, "mongodb://127.0.0.1:1", "db", "c"); err == nil {
		t.Fatal("NewMongoStore with unreachable server: want error")
	}
}

func TestJSONStoreCreatesFileAndDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "urls.json")
	s, err := NewJSONStore(path)
	if err != nil {
		t.Fatalf("NewJSONStore: %v", err)
	}
	defer s.Close(context.Background())
	if got := readStoreFile(t, path); len(got) != 0 {
		t.Errorf("new store file has %d records, want 0", len(got))
	}
}

func TestJSONStoreEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "urls.json")
	if err := os.WriteFile(path, []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewJSONStore(path)
	if err != nil {
		t.Fatalf("NewJSONStore on empty file: %v", err)
	}
	s.Close(context.Background())
}

func TestJSONStoreCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "urls.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewJSONStore(path); err == nil {
		t.Fatal("NewJSONStore on corrupt file: want error")
	}
}

func TestJSONStorePersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "urls.json")

	s, err := NewJSONStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "keep01", "https://example.com/keep"); err != nil {
		t.Fatal(err)
	}
	// Save is durable on its own, before any background flush.
	if _, ok := readStoreFile(t, path)["keep01"]; !ok {
		t.Fatal("record not on disk right after Save")
	}
	for i := 0; i < 5; i++ {
		s.IncrementClicks(ctx, "keep01")
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	s2, err := NewJSONStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close(ctx)
	rec, _ := s2.GetByCode(ctx, "keep01")
	if rec == nil || rec.LongURL != "https://example.com/keep" || rec.Clicks != 5 {
		t.Fatalf("after restart got %+v, want 5 clicks", rec)
	}
	if rec.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", rec.CreatedAt.Location())
	}
}

func TestJSONStoreFlushesClicksInBackground(t *testing.T) {
	ctx := context.Background()
	s, path := newTestJSONStore(t)
	if err := s.Save(ctx, "bg0001", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	s.IncrementClicks(ctx, "bg0001")

	deadline := time.Now().Add(5 * flushInterval)
	for time.Now().Before(deadline) {
		if readStoreFile(t, path)["bg0001"].Clicks == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("click count was not flushed to disk in the background")
}

func TestJSONStoreSaveRollsBackOnWriteFailure(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "gone")
	s, err := NewJSONStore(filepath.Join(dir, "urls.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Removing the directory makes every write fail, even when running as root.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "fail01", "https://example.com"); err == nil {
		t.Fatal("Save: want error when the store file cannot be written")
	}
	if rec, _ := s.GetByCode(ctx, "fail01"); rec != nil {
		t.Fatal("record kept in memory after a failed write")
	}
	os.MkdirAll(dir, 0o755) // let Close flush cleanly
	s.Close(ctx)
}

func TestJSONStoreConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	s, path := newTestJSONStore(t)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code := fmt.Sprintf("c%05d", i)
			if err := s.Save(ctx, code, "https://example.com/"+code); err != nil {
				t.Errorf("Save %s: %v", code, err)
				return
			}
			for j := 0; j < 10; j++ {
				s.IncrementClicks(ctx, code)
				s.GetByCode(ctx, code)
			}
		}()
	}
	wg.Wait()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}

	onDisk := readStoreFile(t, path)
	if len(onDisk) != n {
		t.Fatalf("file has %d records, want %d", len(onDisk), n)
	}
	for code, r := range onDisk {
		if r.Clicks != 10 {
			t.Errorf("%s: clicks = %d, want 10", code, r.Clicks)
		}
	}
	if leftovers, _ := filepath.Glob(path + ".tmp-*"); len(leftovers) > 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}
