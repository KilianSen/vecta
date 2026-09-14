package router

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"vecta/internal/metrics"
	"vecta/internal/proto"
	"vecta/internal/registry"
)

func twoServers763(a, b *backend) []registry.Server {
	return []registry.Server{
		{ID: "alpha", Name: "Alpha", Address: a.addr, MinProtocol: 763, MaxProtocol: 767},
		{ID: "beta", Name: "Beta", Address: b.addr, MinProtocol: 763, MaxProtocol: 767},
	}
}

func TestStickyLastServer(t *testing.T) {
	a, b := startBackend(t), startBackend(t)
	h := startRouter(t, twoServers763(a, b)...)

	c := h.dial()
	c.login(763, "beta.play.test", proto.IntentLogin, "Sticky")
	c.expectBackend()
	waitFrames(t, b)
	c.c.Close()

	// A plain join with two candidates would open the limbo; sticky sends the
	// player straight back to beta.
	c = h.dial()
	c.login(763, "play.test", proto.IntentLogin, "Sticky")
	c.expectBackend()
	waitFrames(t, b)

	// lobby.<domain> still opens the menu.
	c = h.dial()
	c.login(763, "lobby.play.test", proto.IntentLogin, "Sticky")
	c.expect(proto.LoginSuccessID)
}

func TestStickyDisabled(t *testing.T) {
	a, b := startBackend(t), startBackend(t)
	h := startRouterWith(t, func(c *Config) { c.StickyTTL = -1 }, twoServers763(a, b)...)
	c := h.dial()
	c.login(763, "beta.play.test", proto.IntentLogin, "NoSticky")
	c.expectBackend()
	c.c.Close()

	c = h.dial()
	c.login(763, "play.test", proto.IntentLogin, "NoSticky")
	c.expect(proto.LoginSuccessID) // limbo, not beta
}

func TestPersonalizedMOTD(t *testing.T) {
	a, b := startBackend(t), startBackend(t)
	h := startRouterWith(t, func(c *Config) { c.PersonalizedMOTD = true }, twoServers763(a, b)...)
	c := h.dial()
	c.login(763, "beta.play.test", proto.IntentLogin, "Motd")
	c.expectBackend()
	c.c.Close()

	c = h.dial()
	c.send(proto.Handshake{Protocol: 763, Address: "play.test", Port: 25565, Intent: proto.IntentStatus}.Frame(),
		proto.NewPacket(0).Frame())
	js, _ := c.expect(0x00).String(32767)
	var doc struct {
		Description json.RawMessage
	}
	json.Unmarshal([]byte(js), &doc)
	if text := string(doc.Description); !strings.Contains(text, "Back to ") || !strings.Contains(text, "Beta") {
		t.Fatalf("description %s", text)
	}
}

func TestIssueTicket(t *testing.T) {
	a, old := startBackend(t), startBackend(t)
	h := startRouter(t,
		registry.Server{ID: "alpha", Name: "Alpha", Address: a.addr, MinProtocol: 767, MaxProtocol: 767},
		registry.Server{ID: "old", Name: "Old", Address: old.addr, MinProtocol: 763, MaxProtocol: 763},
	)

	tk, err := h.r.IssueTicket("Steve", 767, "Alpha")
	if err != nil || tk.Mode != "transfer" || tk.Host != "play.test" || tk.Port != 25565 || tk.CookieKey != CookieKey {
		t.Fatalf("transfer ticket %+v %v", tk, err)
	}
	cookie, _ := base64.StdEncoding.DecodeString(tk.Cookie)
	if id, err := verifyRoute([]byte("secret"), cookie, "steve", time.Now()); err != nil || id != "alpha" {
		t.Fatalf("cookie route %q %v", id, err)
	}

	if _, err := h.r.IssueTicket("Steve", 763, "alpha"); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("incompatible: %v", err)
	}
	if _, err := h.r.IssueTicket("Steve", 767, "ghost"); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("unknown: %v", err)
	}

	// Pre-1.20.5: remembered route, then a plain reconnect lands on it.
	tk, err = h.r.IssueTicket("Oldie", 763, "old")
	if err != nil || tk.Mode != "reconnect" || !strings.Contains(tk.Message, "Old") || tk.Address != "play.test" {
		t.Fatalf("reconnect ticket %+v %v", tk, err)
	}
	c := h.dial()
	c.login(763, "play.test", proto.IntentLogin, "Oldie")
	c.expectBackend()
	waitFrames(t, old)

	// Lobby ticket for 1.20.5+: the cookie opens the menu instead of a server.
	tk, err = h.r.IssueTicket("Hubber", 767, "lobby")
	if err != nil || tk.Mode != "transfer" {
		t.Fatalf("lobby ticket %+v %v", tk, err)
	}
	cookie, _ = base64.StdEncoding.DecodeString(tk.Cookie)
	c = h.dial()
	c.login(767, "play.test", proto.IntentTransfer, "Hubber")
	c.expect(proto.LoginCookieRequestID)
	c.send(proto.NewPacket(proto.LoginCookieResponseID).String(CookieKey).Bool(true).ByteArray(cookie).Frame())
	c.expect(proto.LoginSuccessID)

	if tk, err := h.r.IssueTicket("Legacy", 47, "lobby"); err != nil || tk.Address != "lobby.play.test" {
		t.Fatalf("legacy lobby ticket %+v %v", tk, err)
	}
}

func TestRateLimitRefusesConnections(t *testing.T) {
	a := startBackend(t)
	m := metrics.New()
	h := startRouterWith(t, func(c *Config) {
		c.RateLimitPerSecond = 0.001
		c.RateLimitBurst = 1
		c.Metrics = m
	}, registry.Server{ID: "alpha", Address: a.addr, MinProtocol: 767, MaxProtocol: 767})

	c := h.dial()
	c.send(proto.Handshake{Protocol: 767, Address: "play.test", Port: 25565, Intent: proto.IntentStatus}.Frame(),
		proto.NewPacket(0).Frame())
	c.expect(0x00)

	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	conn.Write(proto.Handshake{Protocol: 767, Address: "play.test", Port: 25565, Intent: proto.IntentStatus}.Frame())
	// Refused connections are closed without a reply: EOF, or a reset on
	// platforms that report unread data that way.
	n, err := conn.Read(make([]byte, 16))
	if n != 0 || err == nil {
		t.Fatalf("second connection not closed: n=%d err=%v", n, err)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("second connection left open until timeout")
	}
	_ = io.EOF
	var sb strings.Builder
	m.Write(&sb)
	if !strings.Contains(sb.String(), `vecta_connections_rejected_total{reason="rate"} 1`) {
		t.Fatalf("metric missing:\n%s", sb.String())
	}
}
