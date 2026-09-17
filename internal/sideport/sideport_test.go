package sideport

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"vecta/internal/metrics"
	"vecta/internal/registry"
)

// freeRange finds n consecutive ports that are free for both TCP and UDP.
func freeRange(t *testing.T, n int) (int, int) {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		base := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		if base+n > 65535 {
			continue
		}
		ok := true
		for p := base; p < base+n && ok; p++ {
			addr := "127.0.0.1:" + strconv.Itoa(p)
			l, err1 := net.Listen("tcp", addr)
			u, err2 := net.ListenPacket("udp", addr)
			ok = err1 == nil && err2 == nil
			if l != nil {
				l.Close()
			}
			if u != nil {
				u.Close()
			}
		}
		if ok {
			return base, base + n - 1
		}
	}
	t.Fatal("no free port range")
	return 0, 0
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type harness struct {
	m     *Manager
	reg   *registry.Registry
	clock *clock
	min   int
	max   int
}

func newHarness(t *testing.T, size int, tweak func(*Config)) *harness {
	t.Helper()
	lo, hi := freeRange(t, size)
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	reg := registry.New()
	cfg := Config{
		Listen: "127.0.0.1", PublicHost: "voice.test", MinPort: lo, MaxPort: hi,
		Registry: reg, Metrics: metrics.New(), now: c.now,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	m := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx, time.Hour) // sweeps are triggered by hand
	return &harness{m: m, reg: reg, clock: c, min: lo, max: hi}
}

// register upserts a server and syncs its side ports, like the API does.
func (h *harness) register(t *testing.T, owner, id, backend string, ports ...registry.SidePort) []registry.SidePort {
	t.Helper()
	saved, err := h.reg.Upsert(owner, registry.Server{ID: id, Address: backend, SidePorts: ports}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return h.m.Sync(saved)
}

func udp(name string, port int) registry.SidePort {
	return registry.SidePort{Name: name, Protocol: "udp", Port: port}
}

func tcp(name string, port int) registry.SidePort {
	return registry.SidePort{Name: name, Protocol: "tcp", Port: port}
}

func publicPort(t *testing.T, p registry.SidePort) int {
	t.Helper()
	if p.Public == nil {
		t.Fatalf("side port %s not assigned: %q", p.Name, p.Error)
	}
	return p.Public.Port
}

func TestAllocation(t *testing.T) {
	h := newHarness(t, 3, nil)

	a := h.register(t, "alice", "a", "127.0.0.1:1", udp("voice", 24454), tcp("map", 8100))
	if a[0].Public.Host != "voice.test" {
		t.Fatalf("host = %q", a[0].Public.Host)
	}
	// TCP and UDP are numbered independently.
	if publicPort(t, a[0]) != h.min || publicPort(t, a[1]) != h.min {
		t.Fatalf("ports = %d, %d; want both %d", a[0].Public.Port, a[1].Public.Port, h.min)
	}
	// Heartbeats keep the assignment.
	again := h.register(t, "alice", "a", "127.0.0.1:1", udp("voice", 24454), tcp("map", 8100))
	if publicPort(t, again[0]) != h.min {
		t.Fatalf("heartbeat moved port to %d", again[0].Public.Port)
	}
	if got, _ := h.reg.Get("a"); got.SidePorts[0].Public == nil {
		t.Fatal("assignment not stored in registry")
	}

	// A free preferred port is honored; a taken one is not.
	b := h.register(t, "bob", "b", "127.0.0.1:1", registry.SidePort{Name: "voice", Protocol: "udp", Port: 1, PreferredPort: h.max})
	if publicPort(t, b[0]) != h.max {
		t.Fatalf("preferred port ignored: %d", b[0].Public.Port)
	}
	c := h.register(t, "bob", "c", "127.0.0.1:1", registry.SidePort{Name: "voice", Protocol: "udp", Port: 1, PreferredPort: h.min})
	if publicPort(t, c[0]) != h.min+1 {
		t.Fatalf("got %d, want first free %d", c[0].Public.Port, h.min+1)
	}

	// The pool is exhausted.
	d := h.register(t, "bob", "d", "127.0.0.1:1", udp("voice", 1))
	if d[0].Public != nil || d[0].Error != ErrNoPort {
		t.Fatalf("exhausted pool: %+v", d[0])
	}

	// Dropping a side port from the registration releases it, but the port
	// stays reserved for the grace period.
	h.register(t, "alice", "a", "127.0.0.1:1", tcp("map", 8100))
	if n := h.m.Assigned(); n != 3 {
		t.Fatalf("assigned = %d, want 3", n)
	}
	if d := h.register(t, "bob", "d", "127.0.0.1:1", udp("voice", 1)); d[0].Public != nil {
		t.Fatal("held port was reassigned during grace")
	}
	// Re-declaring it within the grace period gets the same port back.
	back := h.register(t, "alice", "a", "127.0.0.1:1", udp("voice", 24454), tcp("map", 8100))
	if publicPort(t, back[0]) != h.min {
		t.Fatalf("not reactivated: %d", back[0].Public.Port)
	}

	// Unregistering and letting the grace period pass frees the port.
	h.m.Release("a")
	h.clock.add(3 * time.Minute)
	h.m.sweep()
	if d := h.register(t, "bob", "d", "127.0.0.1:1", udp("voice", 1)); publicPort(t, d[0]) != h.min {
		t.Fatalf("freed port not reused: %d", d[0].Public.Port)
	}
}

func TestExpiredRegistrationReleases(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.register(t, "alice", "a", "127.0.0.1:1", udp("voice", 1))
	if err := h.reg.Remove("alice", "a"); err != nil {
		t.Fatal(err)
	}
	h.m.sweep()
	if n := h.m.Assigned(); n != 0 {
		t.Fatalf("assigned = %d after server left", n)
	}
}

func TestDisabled(t *testing.T) {
	reg := registry.New()
	m := New(Config{Registry: reg})
	saved, _ := reg.Upsert("alice", registry.Server{ID: "a", Address: "127.0.0.1:1", SidePorts: []registry.SidePort{udp("voice", 1)}}, time.Hour)
	out := m.Sync(saved)
	if out[0].Error != ErrDisabled || out[0].Public != nil {
		t.Fatalf("got %+v", out[0])
	}
}

// udpBackend answers every datagram with the sender address it saw.
func udpBackend(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo([]byte(string(buf[:n])+"|"+addr.String()), addr)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func roundTrip(t *testing.T, c net.Conn, msg string) string {
	t.Helper()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	// The first datagram may race the flow's dial; retry until answered.
	buf := make([]byte, 2048)
	for i := 0; i < 20; i++ {
		c.Write([]byte(msg))
		c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		n, err := c.Read(buf)
		if err == nil {
			return string(buf[:n])
		}
	}
	t.Fatalf("no reply to %q", msg)
	return ""
}

func dialUDP(t *testing.T, port int) net.Conn {
	t.Helper()
	c, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestUDPFlowsPerClient(t *testing.T) {
	h := newHarness(t, 1, nil)
	be := udpBackend(t)
	out := h.register(t, "alice", "a", "127.0.0.1:25565", udp("voice", be))
	port := publicPort(t, out[0])

	c1, c2 := dialUDP(t, port), dialUDP(t, port)
	r1 := roundTrip(t, c1, "one")
	r2 := roundTrip(t, c2, "two")
	up1, up2 := r1[len("one|"):], r2[len("two|"):]
	if r1[:4] != "one|" || r2[:4] != "two|" {
		t.Fatalf("replies crossed: %q %q", r1, r2)
	}
	if up1 == up2 {
		t.Fatalf("clients share upstream address %s", up1)
	}
	// The same client keeps its upstream address.
	if again := roundTrip(t, c1, "one"); again != r1 {
		t.Fatalf("upstream changed: %q -> %q", r1, again)
	}
	if h.m.flows.Load() != 2 {
		t.Fatalf("flows = %d", h.m.flows.Load())
	}

	// Idle flows expire.
	h.clock.add(3 * time.Minute)
	h.m.sweep()
	if n := h.m.flows.Load(); n != 0 {
		t.Fatalf("flows after idle = %d", n)
	}
	// A new datagram opens a fresh flow.
	roundTrip(t, c1, "one")

	// Releasing the port stops forwarding.
	h.register(t, "alice", "a", "127.0.0.1:25565")
	if n := h.m.flows.Load(); n != 0 {
		t.Fatalf("flows after release = %d", n)
	}
	c1.Write([]byte("late"))
	c1.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := c1.Read(make([]byte, 64)); err == nil {
		t.Fatal("released port still forwards")
	}
}

func TestUDPFlowCap(t *testing.T) {
	h := newHarness(t, 1, func(c *Config) { c.MaxUDPFlows = 1 })
	be := udpBackend(t)
	port := publicPort(t, h.register(t, "alice", "a", "127.0.0.1:25565", udp("voice", be))[0])
	roundTrip(t, dialUDP(t, port), "one")

	c2 := dialUDP(t, port)
	c2.Write([]byte("two"))
	c2.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c2.Read(make([]byte, 64)); err == nil {
		t.Fatal("flow over the cap was forwarded")
	}
	if v := h.m.m.dropped.Value("flows-full"); v < 1 {
		t.Fatalf("dropped{flows-full} = %v", v)
	}
}

func TestTCPForward(t *testing.T) {
	h := newHarness(t, 1, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	be := ln.Addr().(*net.TCPAddr).Port
	port := publicPort(t, h.register(t, "alice", "a", "127.0.0.1:25565", tcp("votes", be))[0])

	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("vote"))
	c.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(c)
	if string(got) != "vote" {
		t.Fatalf("echo = %q", got)
	}

	// Release ends open connections.
	open, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	open.Write([]byte("x"))
	open.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(open, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	h.m.Release("a")
	if _, err := open.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection survived release")
	}
}

func TestDialPolicyApplies(t *testing.T) {
	denied := errors.New("denied")
	h := newHarness(t, 1, func(c *Config) {
		c.Dial = func(ctx context.Context, s registry.Server, network, address string) (net.Conn, error) {
			if s.Owner != "alice" || network != "tcp" {
				t.Errorf("dial got owner %q network %q", s.Owner, network)
			}
			return nil, denied
		}
	})
	port := publicPort(t, h.register(t, "alice", "a", "127.0.0.1:25565", tcp("votes", 8192))[0])
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected the connection to be closed")
	}
	if v := h.m.m.dropped.Value("dial"); v != 1 {
		t.Fatalf("dropped{dial} = %v", v)
	}
}
