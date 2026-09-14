package router

import (
	"bufio"
	"net"
	"testing"
	"time"

	"vecta/internal/guard"
	"vecta/internal/proto"
	"vecta/internal/registry"
)

// startGuardedBackend accepts only connections with a valid signed header
// and reports each verification result.
func startGuardedBackend(t *testing.T, key []byte) (string, chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	results := make(chan error, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				br := bufio.NewReader(c)
				h, err := guard.ReadHeader(br)
				if err == nil {
					var res guard.Result
					res, err = guard.Verify(key, h, time.Now(), nil)
					if err == nil && (res.Local || res.Src == nil) {
						err = errFake("expected a PROXY header with the client address")
					}
				}
				results <- err
				if err != nil {
					return
				}
				for i := 0; i < 2; i++ {
					if _, _, err := proto.ReadFrame(br); err != nil {
						return
					}
				}
				c.Write([]byte("BACKEND"))
			}()
		}
	}()
	return ln.Addr().String(), results
}

type errFake string

func (e errFake) Error() string { return string(e) }

func TestGuardedBackend(t *testing.T) {
	key := guard.Key("tok-guard")
	addr, results := startGuardedBackend(t, key)
	srv := registry.Server{ID: "guarded", Address: addr, MinProtocol: 767, MaxProtocol: 767, Guard: true}

	h := startRouterWith(t, func(c *Config) {
		c.GuardKeys = func(s registry.Server) []byte { return key }
	}, srv)
	c := h.dial()
	c.login(767, "guarded.play.test", proto.IntentLogin, "Guarded")
	c.expectBackend()
	if err := <-results; err != nil {
		t.Fatalf("backend rejected gateway header: %v", err)
	}

	// Health pings carry a signed LOCAL header.
	pre, err := registry.Backend{GuardKeys: func(registry.Server) []byte { return key }}.Preamble(srv)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := guard.Verify(key, pre, time.Now(), nil); err != nil || !res.Local {
		t.Fatalf("ping preamble: %+v %v", res, err)
	}
}

func TestGuardedBackendWithoutKey(t *testing.T) {
	addr, results := startGuardedBackend(t, guard.Key("tok-guard"))
	h := startRouter(t, registry.Server{ID: "guarded", Address: addr, MinProtocol: 767, MaxProtocol: 767, Guard: true})
	c := h.dial()
	c.login(767, "guarded.play.test", proto.IntentLogin, "NoKey")
	c.expect(proto.LoginDisconnectID)
	select {
	case err := <-results:
		t.Fatalf("gateway dialed a guarded backend without a key (%v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := (registry.Backend{}).Preamble(registry.Server{Guard: true}); err == nil {
		t.Fatal("ping preamble without key succeeded")
	}
}
