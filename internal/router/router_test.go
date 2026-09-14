package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"anymcp/internal/proto"
	"anymcp/internal/registry"
)

// backend is a fake Minecraft server that records the first two frames of
// each connection and answers with a marker.
type backend struct {
	addr   string
	frames chan [][]byte
}

func startBackend(t *testing.T) *backend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	b := &backend{addr: ln.Addr().String(), frames: make(chan [][]byte, 4)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				var got [][]byte
				for i := 0; i < 2; i++ {
					_, p, err := proto.ReadFrame(br)
					if err != nil {
						return
					}
					got = append(got, p)
				}
				b.frames <- got
				c.Write([]byte("BACKEND"))
			}()
		}
	}()
	return b
}

type harness struct {
	t    *testing.T
	addr string
	reg  *registry.Registry
}

func startRouter(t *testing.T, servers ...registry.Server) *harness {
	t.Helper()
	reg := registry.New()
	for _, s := range servers {
		if _, err := reg.Upsert("test", s, 0); err != nil {
			t.Fatal(err)
		}
		reg.SetHealth(s.ID, s.Address, registry.Health{Online: true, Protocol: s.MaxProtocol, VersionName: "test"})
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(Config{Domain: "play.test", CookieSecret: []byte("secret"), ProbeWindow: 300 * time.Millisecond}, reg, log)
	go r.ServeListener(ctx, ln)
	return &harness{t: t, addr: ln.Addr().String(), reg: reg}
}

type client struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
}

func (h *harness) dial() *client {
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		h.t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	h.t.Cleanup(func() { c.Close() })
	return &client{t: h.t, c: c, br: bufio.NewReader(c)}
}

func (c *client) send(frames ...[]byte) {
	for _, f := range frames {
		if _, err := c.c.Write(f); err != nil {
			c.t.Fatal(err)
		}
	}
}

func (c *client) login(protocol int32, host string, intent int32, name string) {
	hs := proto.Handshake{Protocol: protocol, Address: host, Port: 25565, Intent: intent}
	var uuid [16]byte
	c.send(hs.Frame(), proto.NewPacket(proto.LoginStartID).String(name).Raw(uuid[:]).Frame())
}

// expect reads frames until one with the given packet ID arrives.
func (c *client) expect(id byte) *proto.Buffer {
	c.t.Helper()
	for i := 0; i < 10; i++ {
		_, p, err := proto.ReadFrame(c.br)
		if err != nil {
			c.t.Fatalf("waiting for packet 0x%02x: %v", id, err)
		}
		if p[0] == id {
			b := proto.NewBuffer(p)
			b.VarInt()
			return b
		}
	}
	c.t.Fatalf("packet 0x%02x not received", id)
	return nil
}

func (c *client) expectBackend() {
	c.t.Helper()
	buf := make([]byte, 7)
	if _, err := io.ReadFull(c.br, buf); err != nil || string(buf) != "BACKEND" {
		c.t.Fatalf("expected backend marker, got %q (%v)", buf, err)
	}
}

func waitFrames(t *testing.T, b *backend) [][]byte {
	t.Helper()
	select {
	case f := <-b.frames:
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("backend received nothing")
		return nil
	}
}

func TestSubdomainPassthrough(t *testing.T) {
	be := startBackend(t)
	h := startRouter(t,
		registry.Server{ID: "survival", Address: be.addr, MinProtocol: 766, MaxProtocol: 776},
		registry.Server{ID: "other", Address: "127.0.0.1:1", MinProtocol: 766, MaxProtocol: 776},
	)
	c := h.dial()
	c.login(774, "survival.play.test", proto.IntentLogin, "Steve")
	c.expectBackend()
	got := waitFrames(t, be)
	hs, err := proto.ParseHandshake(got[0])
	if err != nil || hs.Host() != "survival.play.test" || hs.Intent != proto.IntentLogin {
		t.Fatalf("backend handshake = %+v, %v", hs, err)
	}
}

// fabricPackServers returns a vanilla-family server and a Fabric pack that
// requires the "create" mod, both accepting the given protocol.
func fabricPackServers(vanillaAddr, packAddr string) []registry.Server {
	return []registry.Server{
		{ID: "survival", Address: vanillaAddr, Loader: "paper", MinProtocol: 767, MaxProtocol: 776},
		{ID: "createpack", Address: packAddr, Loader: "fabric", MinProtocol: 767, MaxProtocol: 776,
			Mods: []string{"create", "sodium"}, RequiredClientMods: []string{"create"}},
	}
}

func TestLobbyAutoMatchTransferAndCookie(t *testing.T) {
	vanilla, pack := startBackend(t), startBackend(t)
	h := startRouter(t, fabricPackServers(vanilla.addr, pack.addr)...)

	c := h.dial()
	c.login(767, "play.test", proto.IntentLogin, "Alex")
	c.expect(proto.LoginSuccessID)
	c.send(
		proto.NewPacket(proto.LoginAcknowledgedID).Frame(),
		proto.NewPacket(proto.CfgPluginMessageInID).String("minecraft:brand").String("fabric").Frame(),
		proto.NewPacket(proto.CfgPluginMessageInID).String("minecraft:register").Raw([]byte("create:main\x00sodium:net")).Frame(),
	)
	b := c.expect(proto.CfgStoreCookieID)
	if key, _ := b.String(256); key != CookieKey {
		t.Fatalf("cookie key %q", key)
	}
	cookie, _ := b.ByteArray(5120)
	b = c.expect(proto.CfgTransferID)
	host, _ := b.String(256)
	port, _ := b.VarInt()
	if host != "play.test" || port != 25565 {
		t.Fatalf("transfer to %s:%d", host, port)
	}
	c.c.Close()

	// Reconnect as the client would after the transfer.
	c = h.dial()
	c.login(767, host, proto.IntentTransfer, "Alex")
	b = c.expect(proto.LoginCookieRequestID)
	if key, _ := b.String(256); key != CookieKey {
		t.Fatalf("cookie request key %q", key)
	}
	c.send(proto.NewPacket(proto.LoginCookieResponseID).String(CookieKey).Bool(true).ByteArray(cookie).Frame())
	c.expectBackend()
	got := waitFrames(t, pack)
	hs, _ := proto.ParseHandshake(got[0])
	if hs.Intent != proto.IntentLogin {
		t.Fatalf("transfer intent not rewritten: %d", hs.Intent)
	}
	if ls, _ := proto.ParseLoginStart(got[1], 767); ls.Name != "Alex" {
		t.Fatalf("login start name %q", ls.Name)
	}
}

func TestCookieForOtherPlayerRejected(t *testing.T) {
	vanilla, pack := startBackend(t), startBackend(t)
	h := startRouter(t, fabricPackServers(vanilla.addr, pack.addr)...)
	cookie := signRoute([]byte("secret"), "createpack", "Alex", time.Now().Add(time.Minute))

	c := h.dial()
	c.login(767, "play.test", proto.IntentTransfer, "Mallory")
	c.expect(proto.LoginCookieRequestID)
	c.send(proto.NewPacket(proto.LoginCookieResponseID).String(CookieKey).Bool(true).ByteArray(cookie).Frame())
	// Falls through to the lobby instead of the pack.
	c.expect(proto.LoginSuccessID)
}

func TestDialogSelection(t *testing.T) {
	a, b := startBackend(t), startBackend(t)
	h := startRouter(t,
		registry.Server{ID: "alpha", Name: "Alpha", Address: a.addr, MinProtocol: 771, MaxProtocol: 776},
		registry.Server{ID: "beta", Name: "Beta", Address: b.addr, MinProtocol: 771, MaxProtocol: 776},
	)
	c := h.dial()
	c.login(776, "play.test", proto.IntentLogin, "Steve")
	c.expect(proto.LoginSuccessID)
	c.send(
		proto.NewPacket(proto.LoginAcknowledgedID).Frame(),
		proto.NewPacket(proto.CfgPluginMessageInID).String("minecraft:brand").String("vanilla").Frame(),
	)
	dialog := c.expect(proto.CfgShowDialogID).Remaining()
	for _, want := range []string{"minecraft:multi_action", "anymcp:join/alpha", "anymcp:join/beta"} {
		if !bytes.Contains(dialog, []byte(want)) {
			t.Fatalf("dialog missing %q", want)
		}
	}
	c.send(proto.NewPacket(proto.CfgCustomClickActionID).String("anymcp:join/beta").Bool(false).Frame())
	c.expect(proto.CfgStoreCookieID)
	c.expect(proto.CfgTransferID)
}

func TestLegacyClientGetsAddressList(t *testing.T) {
	a, b := startBackend(t), startBackend(t)
	h := startRouter(t,
		registry.Server{ID: "alpha", Name: "Alpha", Address: a.addr, MinProtocol: 763, MaxProtocol: 763},
		registry.Server{ID: "beta", Name: "Beta", Address: b.addr, MinProtocol: 763, MaxProtocol: 763},
	)
	c := h.dial()
	c.login(763, "play.test", proto.IntentLogin, "Old")
	reason, _ := c.expect(proto.LoginDisconnectID).String(262144)
	for _, want := range []string{"alpha.play.test", "beta.play.test"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("disconnect %s missing %q", reason, want)
		}
	}
}

func TestStatusListsServers(t *testing.T) {
	be := startBackend(t)
	h := startRouter(t, registry.Server{ID: "alpha", Name: "Alpha", Address: be.addr, MinProtocol: 767, MaxProtocol: 767})
	c := h.dial()
	c.send(proto.Handshake{Protocol: 767, Address: "play.test", Port: 25565, Intent: proto.IntentStatus}.Frame(),
		proto.NewPacket(0).Frame())
	js, _ := c.expect(0x00).String(32767)
	var doc struct {
		Version struct{ Protocol int32 }
		Players struct{ Sample []struct{ Name string } }
	}
	if err := json.Unmarshal([]byte(js), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version.Protocol != 767 || len(doc.Players.Sample) != 1 || !strings.Contains(doc.Players.Sample[0].Name, "Alpha") {
		t.Fatalf("status = %s", js)
	}
	c.send(proto.NewPacket(1).Int64(42).Frame())
	if v, _ := c.expect(0x01).Int64(); v != 42 {
		t.Fatalf("pong = %d", v)
	}
}
