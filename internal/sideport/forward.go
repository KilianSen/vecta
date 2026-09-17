package sideport

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

type tcpPort struct {
	m   *Manager
	b   *binding
	ln  net.Listener
	sem chan struct{}

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// Close stops accepting and ends open connections.
func (t *tcpPort) Close() error {
	t.mu.Lock()
	t.closed = true
	conns := t.conns
	t.conns = nil
	t.mu.Unlock()
	err := t.ln.Close()
	for c := range conns {
		c.Close()
	}
	return err
}

func (t *tcpPort) track(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.conns[c] = struct{}{}
	return true
}

func (t *tcpPort) untrack(c net.Conn) {
	t.mu.Lock()
	delete(t.conns, c)
	t.mu.Unlock()
}

func (t *tcpPort) serve() {
	for {
		c, err := t.ln.Accept()
		if err != nil {
			return // closed on release
		}
		select {
		case t.sem <- struct{}{}:
		default:
			t.m.m.dropped.Inc("port-full")
			c.Close()
			continue
		}
		go func() {
			defer func() { <-t.sem }()
			t.handle(c)
		}()
	}
}

func (t *tcpPort) handle(c net.Conn) {
	defer c.Close()
	backend, err := t.m.dial(t.b)
	if err != nil {
		t.m.m.dropped.Inc("dial")
		t.m.log.Debug("side port dial failed", "sideport", t.b.key, "err", err)
		return
	}
	defer backend.Close()
	if !t.track(c) || !t.track(backend) {
		return
	}
	defer t.untrack(c)
	defer t.untrack(backend)
	t.m.m.conns.Inc("tcp")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(backend, c)
		t.m.m.bytes.Add(float64(n), "tcp", "in")
		closeWrite(backend)
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(c, backend)
		t.m.m.bytes.Add(float64(n), "tcp", "out")
		closeWrite(c)
	}()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
		return
	}
	c.Close()
}

// udpPort relays datagrams. Each client address gets its own upstream socket,
// because backends such as Simple Voice Chat tell players apart by the source
// address of their datagrams.
type udpPort struct {
	m    *Manager
	b    *binding
	conn *net.UDPConn

	mu     sync.Mutex
	flows  map[string]*flow
	closed bool
}

type flow struct {
	client netip.AddrPort
	up     net.Conn // nil while dialing
	last   atomic.Int64
}

const maxDatagram = 65535

func (u *udpPort) serve() {
	buf := make([]byte, maxDatagram)
	for {
		n, client, err := u.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return // closed on release
		}
		u.m.m.bytes.Add(float64(n), "udp", "in")
		now := u.m.cfg.now().UnixNano()
		ck := client.String()

		u.mu.Lock()
		f := u.flows[ck]
		if f == nil {
			if reason := u.admit(); reason != "" {
				u.mu.Unlock()
				u.m.m.dropped.Inc(reason)
				continue
			}
			f = &flow{client: client}
			f.last.Store(now)
			u.flows[ck] = f
			u.m.flows.Add(1)
			first := append([]byte(nil), buf[:n]...)
			u.mu.Unlock()
			go u.start(ck, f, first)
			continue
		}
		up := f.up
		u.mu.Unlock()
		f.last.Store(now)
		if up == nil {
			u.m.m.dropped.Inc("dialing")
			continue
		}
		up.Write(buf[:n])
	}
}

// admit returns why a new flow is refused, or "". Called with mu held.
func (u *udpPort) admit() string {
	switch {
	case u.closed:
		return "closed"
	case len(u.flows) >= u.m.cfg.MaxFlowsPerPort:
		return "port-full"
	case int(u.m.flows.Load()) >= u.m.cfg.MaxUDPFlows:
		return "flows-full"
	}
	return ""
}

// start dials the backend for a new flow, forwards its first datagram and
// relays replies until the upstream socket closes.
func (u *udpPort) start(ck string, f *flow, first []byte) {
	up, err := u.m.dial(u.b)
	if err != nil {
		u.m.m.dropped.Inc("dial")
		u.m.log.Debug("side port dial failed", "sideport", u.b.key, "err", err)
		u.remove(ck, f)
		return
	}
	u.mu.Lock()
	if u.closed || u.flows[ck] != f {
		u.mu.Unlock()
		up.Close()
		return
	}
	f.up = up
	u.mu.Unlock()
	u.m.m.conns.Inc("udp")
	up.Write(first)

	buf := make([]byte, maxDatagram)
	for {
		n, err := up.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			// ICMP port unreachable surfaces as a read error on a connected
			// socket; the backend may just be restarting, so keep the flow
			// until it idles out unless the socket was closed.
			if errors.Is(err, net.ErrClosed) {
				break
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		f.last.Store(u.m.cfg.now().UnixNano())
		u.m.m.bytes.Add(float64(n), "udp", "out")
		u.conn.WriteToUDPAddrPort(buf[:n], f.client)
	}
	u.remove(ck, f)
}

func (u *udpPort) remove(ck string, f *flow) {
	u.mu.Lock()
	if u.flows[ck] == f {
		delete(u.flows, ck)
		u.m.flows.Add(-1)
	}
	up := f.up
	u.mu.Unlock()
	if up != nil {
		up.Close()
	}
}

// expire closes flows idle since before cutoff.
func (u *udpPort) expire(cutoff time.Time) {
	c := cutoff.UnixNano()
	var idle []*flow
	var keys []string
	u.mu.Lock()
	for ck, f := range u.flows {
		if f.last.Load() < c {
			idle = append(idle, f)
			keys = append(keys, ck)
		}
	}
	u.mu.Unlock()
	for i, f := range idle {
		u.remove(keys[i], f)
	}
}

func (u *udpPort) Close() error {
	u.mu.Lock()
	u.closed = true
	var ups []net.Conn
	for _, f := range u.flows {
		if f.up != nil {
			ups = append(ups, f.up)
		}
	}
	u.m.flows.Add(-int64(len(u.flows)))
	u.flows = map[string]*flow{}
	u.mu.Unlock()
	err := u.conn.Close()
	for _, up := range ups {
		up.Close()
	}
	return err
}
