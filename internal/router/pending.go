package router

import (
	"strings"
	"time"

	"anymcp/internal/store"
)

// Store buckets.
const (
	bucketPending  = "pending"        // player -> server chosen, awaiting reconnect
	bucketRejected = "forge-rejected" // player|server -> client refused that mod list
	bucketSticky   = "sticky"         // player -> last server played
	bucketIPPlayer = "ip-player"      // client IP -> last player name (MOTD)
)

// pendingRoutes is the router's per-player memory, backed by a store so it
// survives restarts.
type pendingRoutes struct {
	st *store.Store
}

func newPendingRoutes(st *store.Store) *pendingRoutes {
	if st == nil {
		st, _ = store.Open("")
	}
	return &pendingRoutes{st: st}
}

func playerKey(player string) string { return strings.ToLower(player) }

// Set remembers a routing decision for clients that cannot be transferred
// (pre-1.20.5): the player reconnects and is sent straight to the server.
func (p *pendingRoutes) Set(player, server string, ttl time.Duration) {
	p.st.Set(bucketPending, playerKey(player), server, ttl)
}

func (p *pendingRoutes) Get(player string) (string, bool) {
	return p.st.Get(bucketPending, playerKey(player))
}

func (p *pendingRoutes) Clear(player string) {
	p.st.Delete(bucketPending, playerKey(player))
}

func (p *pendingRoutes) Reject(player, server string, ttl time.Duration) {
	p.st.Set(bucketRejected, playerKey(player)+"|"+server, "1", ttl)
}

func (p *pendingRoutes) Rejected(player, server string) bool {
	_, ok := p.st.Get(bucketRejected, playerKey(player)+"|"+server)
	return ok
}

func (p *pendingRoutes) SetSticky(player, server string, ttl time.Duration) {
	p.st.Set(bucketSticky, playerKey(player), server, ttl)
}

func (p *pendingRoutes) Sticky(player string) (string, bool) {
	return p.st.Get(bucketSticky, playerKey(player))
}

func (p *pendingRoutes) SetIPPlayer(ip, player string, ttl time.Duration) {
	p.st.Set(bucketIPPlayer, ip, player, ttl)
}

func (p *pendingRoutes) IPPlayer(ip string) (string, bool) {
	return p.st.Get(bucketIPPlayer, ip)
}

func (p *pendingRoutes) prune() { p.st.Prune() }
