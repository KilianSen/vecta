// Package sideport exposes extra backend ports (voice chat, maps, votes, ...)
// on public ports the gateway assigns from a pool. Traffic is forwarded as raw
// TCP or UDP; the gateway never looks inside it. Owners learn their assigned
// port from the registration response and run a callback to configure the
// mod (docs/side-ports.md).
package sideport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"vecta/internal/metrics"
	"vecta/internal/registry"
)

// Error strings reported to owners in SidePort.Error.
const (
	ErrDisabled = "side ports are disabled on this gateway"
	ErrNoPort   = "no free public port"
)

// DialFunc dials a backend under the gateway's address policy.
type DialFunc func(ctx context.Context, s registry.Server, network, address string) (net.Conn, error)

type Config struct {
	// Listen is the host to bind public ports on; empty means all interfaces.
	Listen string
	// PublicHost is the host players use to reach side ports.
	PublicHost string
	// MinPort..MaxPort is the pool. MaxPort == 0 disables side ports.
	MinPort, MaxPort int
	// UDPIdle closes UDP flows without traffic for this long (default 2m).
	UDPIdle time.Duration
	// ReleaseGrace keeps a released port reserved for its previous owner
	// (default 2m), so a quick restart gets the same port back.
	ReleaseGrace time.Duration
	// MaxUDPFlows caps UDP flows across all ports (default 4096).
	MaxUDPFlows int
	// MaxFlowsPerPort caps UDP flows and TCP connections per public port
	// (default 512).
	MaxFlowsPerPort int

	Registry *registry.Registry
	Dial     DialFunc
	Metrics  *metrics.Registry
	Log      *slog.Logger

	now func() time.Time
}

type key struct{ server, name, protocol string }

func (k key) String() string { return k.server + "/" + k.name + "/" + k.protocol }

// binding is one public port assigned to one side port of one server.
type binding struct {
	key
	owner  string
	public int
	// backend is the backend port; heartbeats may change it.
	backend atomic.Int64
	// heldUntil is set once released: the port stays reserved until then.
	heldUntil time.Time
	closer    io.Closer
}

type Manager struct {
	cfg Config
	log *slog.Logger

	mu     sync.Mutex
	byKey  map[key]*binding
	byPort map[string]map[int]*binding // protocol -> public port
	broken map[string]map[int]time.Time
	flows  atomic.Int64

	m struct {
		conns   *metrics.CounterVec
		bytes   *metrics.CounterVec
		dropped *metrics.CounterVec
	}
}

func New(cfg Config) *Manager {
	if cfg.UDPIdle <= 0 {
		cfg.UDPIdle = 2 * time.Minute
	}
	if cfg.ReleaseGrace <= 0 {
		cfg.ReleaseGrace = 2 * time.Minute
	}
	if cfg.MaxUDPFlows <= 0 {
		cfg.MaxUDPFlows = 4096
	}
	if cfg.MaxFlowsPerPort <= 0 {
		cfg.MaxFlowsPerPort = 512
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.Dial == nil {
		cfg.Dial = func(ctx context.Context, _ registry.Server, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, address)
		}
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Manager{
		cfg:    cfg,
		log:    log,
		byKey:  map[key]*binding{},
		byPort: map[string]map[int]*binding{"tcp": {}, "udp": {}},
		broken: map[string]map[int]time.Time{"tcp": {}, "udp": {}},
	}
	if r := cfg.Metrics; r != nil {
		m.m.conns = r.Counter("vecta_sideport_connections_total", "Side port TCP connections and UDP flows opened", "protocol")
		m.m.bytes = r.Counter("vecta_sideport_bytes_total", "Bytes forwarded through side ports", "protocol", "direction")
		m.m.dropped = r.Counter("vecta_sideport_dropped_total", "Side port connections or datagrams dropped", "reason")
		r.GaugeFunc("vecta_sideport_assigned", "Side ports with an active public port", func() float64 { return float64(m.Assigned()) })
		r.GaugeFunc("vecta_sideport_flows", "Active side port UDP flows", func() float64 { return float64(m.flows.Load()) })
	}
	return m
}

// Enabled reports whether a port pool is configured.
func (m *Manager) Enabled() bool { return m.cfg.MaxPort > 0 }

// Assigned returns the number of active (not released) bindings.
func (m *Manager) Assigned() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, b := range m.byKey {
		if b.heldUntil.IsZero() {
			n++
		}
	}
	return n
}

// Sync assigns public ports for a registered server's side ports, releases
// ports it no longer declares, and returns the side ports with Public or
// Error filled in. The result is also stored in the registry.
func (m *Manager) Sync(srv registry.Server) []registry.SidePort {
	out := append([]registry.SidePort(nil), srv.SidePorts...)
	m.mu.Lock()
	wanted := map[key]bool{}
	for i := range out {
		p := &out[i]
		k := key{srv.ID, p.Name, p.Protocol}
		wanted[k] = true
		if !m.Enabled() {
			p.Error = ErrDisabled
			continue
		}
		b := m.assign(k, srv.Owner, p.PreferredPort)
		if b == nil {
			p.Error = ErrNoPort
			continue
		}
		b.backend.Store(int64(p.Port))
		p.Public = &registry.PublicAddr{Host: m.cfg.PublicHost, Port: b.public}
	}
	for k, b := range m.byKey {
		if k.server == srv.ID && !wanted[k] {
			m.release(b)
		}
	}
	m.mu.Unlock()
	if m.cfg.Registry != nil {
		m.cfg.Registry.SetSidePorts(srv.ID, out)
	}
	return out
}

// Release frees all ports of a server (it was unregistered).
func (m *Manager) Release(serverID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, b := range m.byKey {
		if k.server == serverID {
			m.release(b)
		}
	}
}

// assign returns the active binding for k, reusing or reactivating an
// existing one, or allocating a new public port. Called with mu held.
func (m *Manager) assign(k key, owner string, preferred int) *binding {
	if b := m.byKey[k]; b != nil {
		if b.owner != owner {
			m.drop(b) // server ID was taken over by another owner
		} else if b.heldUntil.IsZero() {
			return b
		} else if err := m.open(b); err == nil {
			b.heldUntil = time.Time{}
			m.log.Info("side port reactivated", "sideport", k, "port", b.public)
			return b
		} else {
			m.log.Warn("side port reopen failed", "sideport", k, "port", b.public, "err", err)
			m.drop(b)
		}
	}
	try := func(port int) *binding {
		if port < m.cfg.MinPort || port > m.cfg.MaxPort || m.byPort[k.protocol][port] != nil {
			return nil
		}
		if until, ok := m.broken[k.protocol][port]; ok && m.cfg.now().Before(until) {
			return nil
		}
		b := &binding{key: k, owner: owner, public: port}
		if err := m.open(b); err != nil {
			m.log.Warn("side port listen failed", "protocol", k.protocol, "port", port, "err", err)
			m.broken[k.protocol][port] = m.cfg.now().Add(time.Minute)
			return nil
		}
		m.byKey[k] = b
		m.byPort[k.protocol][port] = b
		m.log.Info("side port assigned", "sideport", k, "port", port, "owner", owner)
		return b
	}
	if b := try(preferred); b != nil {
		return b
	}
	for port := m.cfg.MinPort; port <= m.cfg.MaxPort; port++ {
		if b := try(port); b != nil {
			return b
		}
	}
	return nil
}

// release stops forwarding and keeps the port reserved for the grace period.
func (m *Manager) release(b *binding) {
	if !b.heldUntil.IsZero() {
		return
	}
	if b.closer != nil {
		b.closer.Close()
		b.closer = nil
	}
	b.heldUntil = m.cfg.now().Add(m.cfg.ReleaseGrace)
	m.log.Info("side port released", "sideport", b.key, "port", b.public)
}

// drop releases a binding and frees its port immediately.
func (m *Manager) drop(b *binding) {
	if b.closer != nil {
		b.closer.Close()
		b.closer = nil
	}
	delete(m.byKey, b.key)
	if m.byPort[b.protocol][b.public] == b {
		delete(m.byPort[b.protocol], b.public)
	}
}

func (m *Manager) open(b *binding) error {
	addr := net.JoinHostPort(m.cfg.Listen, strconv.Itoa(b.public))
	switch b.protocol {
	case "tcp":
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		t := &tcpPort{m: m, b: b, ln: ln, sem: make(chan struct{}, m.cfg.MaxFlowsPerPort), conns: map[net.Conn]struct{}{}}
		b.closer = t
		go t.serve()
	case "udp":
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return err
		}
		u := &udpPort{m: m, b: b, conn: pc.(*net.UDPConn), flows: map[string]*flow{}}
		b.closer = u
		go u.serve()
	default:
		return fmt.Errorf("unknown protocol %q", b.protocol)
	}
	return nil
}

// Run releases ports of servers that left the registry, frees ports whose
// grace period ended and expires idle UDP flows, until ctx ends. Then it
// closes every listener.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			for _, b := range m.byKey {
				m.drop(b)
			}
			m.mu.Unlock()
			return
		case <-t.C:
			m.sweep()
		}
	}
}

func (m *Manager) sweep() {
	now := m.cfg.now()
	var udp []*udpPort
	m.mu.Lock()
	for _, b := range m.byKey {
		switch {
		case !b.heldUntil.IsZero():
			if now.After(b.heldUntil) {
				m.drop(b)
			}
		case m.cfg.Registry != nil && !m.stillRegistered(b):
			m.release(b)
		default:
			if u, ok := b.closer.(*udpPort); ok {
				udp = append(udp, u)
			}
		}
	}
	for proto, ports := range m.broken {
		for port, until := range ports {
			if now.After(until) {
				delete(m.broken[proto], port)
			}
		}
	}
	m.mu.Unlock()
	for _, u := range udp {
		u.expire(now.Add(-m.cfg.UDPIdle))
	}
}

func (m *Manager) stillRegistered(b *binding) bool {
	s, ok := m.cfg.Registry.Get(b.server)
	if !ok || s.Owner != b.owner {
		return false
	}
	for _, p := range s.SidePorts {
		if p.Name == b.name && p.Protocol == b.protocol {
			return true
		}
	}
	return false
}

// dial connects to the backend side of a binding.
func (m *Manager) dial(b *binding) (net.Conn, error) {
	if m.cfg.Registry == nil {
		return nil, errors.New("no registry")
	}
	srv, ok := m.cfg.Registry.Get(b.server)
	if !ok || srv.Owner != b.owner {
		return nil, errors.New("server not registered")
	}
	host, _, err := net.SplitHostPort(srv.Address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := net.JoinHostPort(host, strconv.FormatInt(b.backend.Load(), 10))
	return m.cfg.Dial(ctx, srv, b.protocol, addr)
}
