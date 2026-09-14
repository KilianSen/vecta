package api

import (
	"context"
	"sync"
	"time"
)

// GlobalMessage is one cross-server broadcast.
type GlobalMessage struct {
	Seq     int64  `json:"seq"`
	Server  string `json:"server"`
	Player  string `json:"player"`
	Message string `json:"message"`
	Time    int64  `json:"time"`
}

// broker keeps the recent global messages and lets pollers wait for new ones.
// Server jars post an op's /global here and long-poll for everyone's.
type broker struct {
	mu     sync.Mutex
	seq    int64
	ring   []GlobalMessage
	max    int
	notify chan struct{}
}

func newBroker() *broker {
	return &broker{max: 256, notify: make(chan struct{})}
}

// publish appends a message and wakes pollers. It returns the assigned seq.
func (b *broker) publish(server, player, message string) int64 {
	b.mu.Lock()
	b.seq++
	m := GlobalMessage{Seq: b.seq, Server: server, Player: player, Message: message, Time: time.Now().Unix()}
	b.ring = append(b.ring, m)
	if len(b.ring) > b.max {
		b.ring = b.ring[len(b.ring)-b.max:]
	}
	seq := b.seq
	old := b.notify
	b.notify = make(chan struct{})
	b.mu.Unlock()
	close(old) // wake everyone waiting
	return seq
}

// current returns the latest seq, for a poller initializing its position.
func (b *broker) current() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// since returns messages newer than after, and the current max seq.
func (b *broker) since(after int64) ([]GlobalMessage, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []GlobalMessage
	for _, m := range b.ring {
		if m.Seq > after {
			out = append(out, m)
		}
	}
	return out, b.seq
}

// wait blocks until a message newer than after exists or ctx/timeout ends, then
// returns the messages since after and the current max seq.
func (b *broker) wait(ctx context.Context, after int64, timeout time.Duration) ([]GlobalMessage, int64) {
	deadline := time.After(timeout)
	for {
		if msgs, max := b.since(after); len(msgs) > 0 {
			return msgs, max
		}
		b.mu.Lock()
		ch := b.notify
		b.mu.Unlock()
		select {
		case <-ch:
		case <-deadline:
			return b.since(after)
		case <-ctx.Done():
			return b.since(after)
		}
	}
}
