package router

import (
	"strings"
	"sync"
	"time"
)

// pendingRoutes remembers a routing decision for clients that cannot be
// transferred (pre-1.20.5): the player reconnects and is sent straight to the
// chosen server.
type pendingRoutes struct {
	mu sync.Mutex
	m  map[string]pendingRoute
}

type pendingRoute struct {
	server  string
	expires time.Time
}

func newPendingRoutes() *pendingRoutes {
	return &pendingRoutes{m: map[string]pendingRoute{}}
}

func (p *pendingRoutes) Set(player, server string, ttl time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[strings.ToLower(player)] = pendingRoute{server, time.Now().Add(ttl)}
}

func (p *pendingRoutes) Get(player string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := strings.ToLower(player)
	r, ok := p.m[key]
	if !ok {
		return "", false
	}
	if time.Now().After(r.expires) {
		delete(p.m, key)
		return "", false
	}
	return r.server, true
}

func (p *pendingRoutes) prune() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, r := range p.m {
		if now.After(r.expires) {
			delete(p.m, k)
		}
	}
}
