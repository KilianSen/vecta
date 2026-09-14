package router

import (
	"testing"
	"time"
)

func TestLimiterRate(t *testing.T) {
	l := newLimiter(2, 3, 0)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		rel, reason := l.acquire("1.2.3.4")
		if reason != "" {
			t.Fatalf("burst connection %d refused: %s", i, reason)
		}
		rel()
	}
	if _, reason := l.acquire("1.2.3.4"); reason != "rate" {
		t.Fatalf("4th connection within burst window: reason %q", reason)
	}
	if _, reason := l.acquire("5.6.7.8"); reason != "" {
		t.Fatal("other IP affected by rate limit")
	}
	now = now.Add(500 * time.Millisecond) // +1 token
	if _, reason := l.acquire("1.2.3.4"); reason != "" {
		t.Fatalf("token not refilled: %s", reason)
	}
}

func TestLimiterConcurrency(t *testing.T) {
	l := newLimiter(0, 1, 2)
	r1, _ := l.acquire("ip")
	r2, _ := l.acquire("ip")
	if _, reason := l.acquire("ip"); reason != "concurrency" {
		t.Fatalf("third concurrent connection: %q", reason)
	}
	r1()
	r1() // double release is harmless
	if rel, reason := l.acquire("ip"); reason != "" {
		t.Fatalf("slot not released: %s", reason)
	} else {
		rel()
	}
	r2()
	l.sweep()
	if len(l.ips) != 0 {
		t.Fatalf("idle IP not swept: %d", len(l.ips))
	}
}

func TestSemaphore(t *testing.T) {
	s := newSemaphore(1)
	if !s.tryAcquire() || s.tryAcquire() {
		t.Fatal("semaphore of 1 misbehaves")
	}
	s.release()
	if !s.tryAcquire() {
		t.Fatal("release did not free the slot")
	}
	var unlimited semaphore
	if !unlimited.tryAcquire() {
		t.Fatal("nil semaphore must be unlimited")
	}
	unlimited.release()
}
