// Package store is a small persistent key-value store with per-key expiry.
// Data lives in memory and is snapshotted to a JSON file, which is enough for
// a single gateway's remembered routes and preferences.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type entry struct {
	Value   string `json:"v"`
	Expires int64  `json:"e,omitempty"` // unix seconds; 0 = never
}

type Store struct {
	mu    sync.Mutex
	path  string
	data  map[string]map[string]entry
	dirty bool
	now   func() time.Time
	// flushMu serializes whole flushes (write temp file + rename).
	flushMu sync.Mutex
}

// Open loads path if it exists. An empty path gives a memory-only store.
func Open(path string) (*Store, error) {
	s := &Store{path: path, data: map[string]map[string]entry{}, now: time.Now}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return nil, err
		}
	}
	s.Prune()
	return s, nil
}

// Set stores value under bucket/key. ttl <= 0 keeps it until deleted.
func (s *Store) Set(bucket, key, value string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.data[bucket]
	if b == nil {
		b = map[string]entry{}
		s.data[bucket] = b
	}
	e := entry{Value: value}
	if ttl > 0 {
		e.Expires = s.now().Add(ttl).Unix()
	}
	b[key] = e
	s.dirty = true
}

func (s *Store) Get(bucket, key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[bucket][key]
	if !ok {
		return "", false
	}
	if e.Expires != 0 && s.now().Unix() >= e.Expires {
		delete(s.data[bucket], key)
		s.dirty = true
		return "", false
	}
	return e.Value, true
}

func (s *Store) Delete(bucket, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[bucket][key]; ok {
		delete(s.data[bucket], key)
		s.dirty = true
	}
}

// Len returns the number of live entries in a bucket.
func (s *Store) Len(bucket string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	now := s.now().Unix()
	for _, e := range s.data[bucket] {
		if e.Expires == 0 || now < e.Expires {
			n++
		}
	}
	return n
}

// Prune removes expired entries.
func (s *Store) Prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	for _, b := range s.data {
		for k, e := range b {
			if e.Expires != 0 && now >= e.Expires {
				delete(b, k)
				s.dirty = true
			}
		}
	}
}

// Flush writes the snapshot atomically if anything changed. Concurrent calls
// wait for each other, so a Flush on shutdown cannot return (and let the
// process exit) while another flush is between writing and renaming.
func (s *Store) Flush() error {
	if s.path == "" {
		return nil
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	raw, err := json.Marshal(s.data)
	s.dirty = false
	s.mu.Unlock()
	if err == nil {
		err = s.write(raw)
	}
	if err != nil {
		s.mu.Lock()
		s.dirty = true // retry on the next flush
		s.mu.Unlock()
	}
	return err
}

func (s *Store) write(raw []byte) error {
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Run prunes and flushes every interval, and once more when ctx ends.
func (s *Store) Run(ctx context.Context, interval time.Duration, onError func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := s.Flush(); err != nil && onError != nil {
				onError(err)
			}
			return
		case <-t.C:
			s.Prune()
			if err := s.Flush(); err != nil && onError != nil {
				onError(err)
			}
		}
	}
}
