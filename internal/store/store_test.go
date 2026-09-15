package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSetGetExpiry(t *testing.T) {
	s, _ := Open("")
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }

	s.Set("pending", "steve", "survival", time.Minute)
	s.Set("sticky", "steve", "creative", 0)
	if v, ok := s.Get("pending", "steve"); !ok || v != "survival" {
		t.Fatalf("got %q %v", v, ok)
	}
	now = now.Add(2 * time.Minute)
	if _, ok := s.Get("pending", "steve"); ok {
		t.Fatal("expired entry still returned")
	}
	if v, ok := s.Get("sticky", "steve"); !ok || v != "creative" {
		t.Fatal("entry without ttl expired")
	}
	s.Delete("sticky", "steve")
	if s.Len("sticky") != 0 {
		t.Fatal("delete failed")
	}
}

// Regression: on shutdown the periodic flush and the final flush raced; the
// final one returned early while the other hadn't renamed its temp file yet.
func TestConcurrentFlushCompletes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		s.Set("pending", fmt.Sprintf("player%d", i), "survival", time.Hour)
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.Flush(); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		// Every flush has returned: the snapshot must be fully on disk.
		if _, err := os.Stat(path + ".tmp"); err == nil {
			t.Fatalf("iteration %d: temp file left behind after all flushes returned", i)
		}
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Len("pending") != 50 {
		t.Fatalf("reloaded %d entries, want 50", s2.Len("pending"))
	}
}

func TestFailedFlushStaysDirty(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, []byte("x"), 0o600)
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(blocker, "state.json") // parent is a file: writes fail
	s.Set("sticky", "a", "b", 0)
	if err := s.Flush(); err == nil {
		t.Fatal("flush into a file path succeeded")
	}
	s.path = filepath.Join(dir, "state.json")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path); err != nil {
		t.Fatal("failed flush was not retried")
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Set("sticky", "alex", "skyblock", time.Hour)
	s.Set("pending", "gone", "x", time.Nanosecond)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // expiry has second resolution

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := s2.Get("sticky", "alex"); !ok || v != "skyblock" {
		t.Fatalf("not persisted: %q %v", v, ok)
	}
	if s2.Len("pending") != 0 {
		t.Fatal("expired entry survived reload")
	}
}
