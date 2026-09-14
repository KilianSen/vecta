package proto

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// fakeStatusServer answers one status request. With requireProxy it rejects
// connections that don't start with a PROXY v2 header, like Paper with
// proxies.proxy-protocol enabled.
func fakeStatusServer(t *testing.T, requireProxy bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		if requireProxy {
			hdr := make([]byte, 16)
			if _, err := io.ReadFull(br, hdr); err != nil || !bytes.Equal(hdr[:12], proxyV2Local[:12]) {
				return
			}
		}
		if _, _, err := ReadFrame(br); err != nil { // handshake
			return
		}
		if _, _, err := ReadFrame(br); err != nil { // status request
			return
		}
		c.Write(StatusResponse(map[string]any{
			"version": map[string]any{"name": "1.21.11", "protocol": 774},
			"players": map[string]any{"online": 3, "max": 20},
		}))
	}()
	return ln.Addr().String()
}

func TestPingProxyProtocol(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	st, err := Ping(ctx, fakeStatusServer(t, true), -1, ProxyV2Local(), nil)
	if err != nil || st.Version.Protocol != 774 || st.Players.Online != 3 {
		t.Fatalf("with PROXY header: %+v, %v", st, err)
	}
	if _, err := Ping(ctx, fakeStatusServer(t, true), -1, nil, nil); err == nil {
		t.Fatal("ping without PROXY header must fail against a PROXY-only server")
	}
	if _, err := Ping(ctx, fakeStatusServer(t, false), -1, nil, nil); err != nil {
		t.Fatalf("plain ping: %v", err)
	}
}
