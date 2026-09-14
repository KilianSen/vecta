package netguard

import (
	"context"
	"net"
	"net/netip"
	"testing"
)

func TestDefaultPolicy(t *testing.T) {
	var p Policy
	for _, bad := range []string{"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "224.0.0.1", "::ffff:127.0.0.1"} {
		if err := p.CheckIP(netip.MustParseAddr(bad)); err == nil {
			t.Errorf("%s allowed", bad)
		}
	}
	for _, ok := range []string{"10.0.0.5", "172.18.0.4", "192.168.1.2", "8.8.8.8", "2001:db8::1"} {
		if err := p.CheckIP(netip.MustParseAddr(ok)); err != nil {
			t.Errorf("%s denied: %v", ok, err)
		}
	}
}

func TestAllowedNetworks(t *testing.T) {
	nets, err := ParsePrefixes([]string{"10.0.0.0/8", "192.168.5.7"})
	if err != nil {
		t.Fatal(err)
	}
	p := Policy{Allowed: nets}
	if p.CheckIP(netip.MustParseAddr("10.9.8.7")) != nil || p.CheckIP(netip.MustParseAddr("192.168.5.7")) != nil {
		t.Fatal("allowed address denied")
	}
	if p.CheckIP(netip.MustParseAddr("192.168.5.8")) == nil || p.CheckIP(netip.MustParseAddr("172.16.0.1")) == nil {
		t.Fatal("address outside allowed networks accepted")
	}
	if _, err := ParsePrefixes([]string{"nope"}); err == nil {
		t.Fatal("bad prefix parsed")
	}
}

func TestDialRespectsPolicy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	if _, err := (Policy{}).DialContext(context.Background(), "tcp", ln.Addr().String()); err == nil {
		t.Fatal("dial to loopback allowed by default policy")
	}
	conn, err := Policy{AllowLoopback: true}.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, err := (Policy{}).Resolve(context.Background(), "localhost:25565"); err == nil {
		t.Fatal("localhost hostname resolved to an allowed address")
	}
}
