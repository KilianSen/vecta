package api

import (
	"context"
	"testing"
	"time"
)

func TestBrokerPublishAndSince(t *testing.T) {
	b := newBroker()
	if b.current() != 0 {
		t.Fatal("fresh broker should be at seq 0")
	}
	b.publish("survival", "Alice", "hi")
	seq := b.publish("creative", "Bob", "yo")
	if seq != 2 {
		t.Fatalf("second publish seq = %d, want 2", seq)
	}
	// A poller that initialized at seq 1 sees only the second message.
	msgs, max := b.since(1)
	if len(msgs) != 1 || msgs[0].Player != "Bob" || max != 2 {
		t.Fatalf("since(1) = %+v max=%d", msgs, max)
	}
	// Caught up: nothing new.
	if msgs, _ := b.since(2); len(msgs) != 0 {
		t.Fatalf("since(2) returned %d messages", len(msgs))
	}
}

func TestBrokerWaitWakesOnPublish(t *testing.T) {
	b := newBroker()
	done := make(chan []GlobalMessage, 1)
	go func() {
		msgs, _ := b.wait(context.Background(), 0, 2*time.Second)
		done <- msgs
	}()
	time.Sleep(50 * time.Millisecond) // let the waiter block
	b.publish("survival", "Alice", "ping")
	select {
	case msgs := <-done:
		if len(msgs) != 1 || msgs[0].Message != "ping" {
			t.Fatalf("wait returned %+v", msgs)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not wake on publish")
	}
}

func TestBrokerWaitTimesOut(t *testing.T) {
	b := newBroker()
	b.publish("survival", "Alice", "old")
	start := time.Now()
	msgs, max := b.wait(context.Background(), 1, 150*time.Millisecond)
	if len(msgs) != 0 || max != 1 {
		t.Fatalf("expected no messages at max 1, got %+v max=%d", msgs, max)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("wait returned before its timeout")
	}
}

func TestBrokerRingCap(t *testing.T) {
	b := newBroker()
	b.max = 4
	for i := 0; i < 10; i++ {
		b.publish("s", "p", "m")
	}
	msgs, max := b.since(0)
	if max != 10 || len(msgs) != 4 || msgs[0].Seq != 7 {
		t.Fatalf("ring: max=%d len=%d first=%d", max, len(msgs), msgsSeq(msgs))
	}
}

func msgsSeq(m []GlobalMessage) int64 {
	if len(m) == 0 {
		return -1
	}
	return m[0].Seq
}
