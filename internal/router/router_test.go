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

	"vecta/internal/proto"
	"vecta/internal/registry"
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
	r    *Router
}

func startRouter(t *testing.T, servers ...registry.Server) *harness {
	t.Helper()
	return startRouterWith(t, nil, servers...)
}

// startRouterWith lets a test adjust the router config.
func startRouterWith(t *testing.T, tweak func(*Config), servers ...registry.Server) *harness {
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
	cfg := Config{Domain: "play.test", CookieSecret: []byte("secret"), ProbeWindow: 300 * time.Millisecond}
	if tweak != nil {
		tweak(&cfg)
	}
	r := New(cfg, reg, log)
	go r.ServeListener(ctx, ln)
	return &harness{t: t, addr: ln.Addr().String(), reg: reg, r: r}
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

// TestNeoForgeClientProbedBeforeBrand checks the NeoForge query precedes the
// brand (else NeoForge clients with required mods disconnect) and that the
// channel list in the reply drives matching.
func TestNeoForgeClientProbedBeforeBrand(t *testing.T) {
	neo, paper := startBackend(t), startBackend(t)
	h := startRouter(t,
		registry.Server{ID: "neo-pack", Name: "Neo Pack", Address: neo.addr, Loader: "neoforge", MinProtocol: 767, MaxProtocol: 767, Mods: []string{"jei"}},
		registry.Server{ID: "paper", Name: "Paper", Address: paper.addr, Loader: "paper", MinProtocol: 767, MaxProtocol: 767},
	)
	c := h.dial()
	c.login(767, "play.test", proto.IntentLogin, "Neo")
	c.expect(proto.LoginSuccessID)
	c.send(proto.NewPacket(proto.LoginAcknowledgedID).Frame())

	b := c.expect(proto.CfgPluginMessageOutID)
	if ch, _ := b.String(256); ch != "neoforge:register" {
		t.Fatalf("first plugin message is %q, want neoforge:register", ch)
	}
	// Reply: one protocol with one required payload jei:main.
	reply := proto.NewPacket(0).VarInt(1).VarInt(4).VarInt(1).
		String("jei:main").String("1").Bool(true).VarInt(1).Bool(false).Frame()
	_, body, _ := proto.ReadFrame(bufio.NewReader(bytes.NewReader(reply)))
	c.send(
		proto.NewPacket(proto.CfgPluginMessageInID).String("neoforge:register").Raw(body[1:]).Frame(),
		proto.NewPacket(proto.CfgPluginMessageInID).String("minecraft:brand").String("neoforge").Frame(),
	)

	b = c.expect(proto.CfgStoreCookieID)
	b.String(256)
	cookie, _ := b.ByteArray(5120)
	b = c.expect(proto.CfgTransferID)
	host, _ := b.String(256)
	c.c.Close()

	c = h.dial()
	c.login(767, host, proto.IntentTransfer, "Neo")
	c.expect(proto.LoginCookieRequestID)
	c.send(proto.NewPacket(proto.LoginCookieResponseID).String(CookieKey).Bool(true).ByteArray(cookie).Frame())
	c.expectBackend()
	waitFrames(t, neo)
}

// TestFabricCommonRegisterHandshake: a real Fabric client reveals its play
// channels only after the server's c:version and c:register.
func TestFabricCommonRegisterHandshake(t *testing.T) {
	vanilla, pack := startBackend(t), startBackend(t)
	h := startRouter(t, fabricPackServers(vanilla.addr, pack.addr)...)
	c := h.dial()
	c.login(767, "play.test", proto.IntentLogin, "RealFabric")
	c.expect(proto.LoginSuccessID)
	c.send(
		proto.NewPacket(proto.LoginAcknowledgedID).Frame(),
		proto.NewPacket(proto.CfgPluginMessageInID).String("minecraft:brand").String("fabric").Frame(),
	)
	var sawVersion bool
	for {
		b := c.expect(proto.CfgPluginMessageOutID)
		ch, _ := b.String(256)
		if ch == "c:version" {
			sawVersion = true
		}
		if ch == "c:register" {
			break
		}
	}
	if !sawVersion {
		t.Fatal("c:register sent before c:version")
	}
	c.send(
		proto.NewPacket(proto.CfgPluginMessageInID).String("c:version").VarInt(1).VarInt(1).Frame(),
		proto.NewPacket(proto.CfgPluginMessageInID).String("c:register").VarInt(1).String("play").
			VarInt(2).String("create:main").String("fabric:registry/sync").Frame(),
	)
	b := c.expect(proto.CfgStoreCookieID)
	b.String(256)
	cookie, _ := b.ByteArray(5120)
	c.expect(proto.CfgTransferID)
	c.c.Close()

	c = h.dial()
	c.login(767, "play.test", proto.IntentTransfer, "RealFabric")
	c.expect(proto.LoginCookieRequestID)
	c.send(proto.NewPacket(proto.LoginCookieResponseID).String(CookieKey).Bool(true).ByteArray(cookie).Frame())
	c.expectBackend()
	waitFrames(t, pack)
}

func TestParseNeoForgeQuery(t *testing.T) {
	reply := proto.NewPacket(0).VarInt(2).
		VarInt(4).VarInt(2).
		String("jei:main").String("1").Bool(false).Bool(true).
		String("create:sync").String("2").Bool(true).VarInt(0).Bool(false).
		VarInt(1).VarInt(1).
		String("mekanism:net").String("10").Bool(false).Bool(false).
		Frame()
	_, body, _ := proto.ReadFrame(bufio.NewReader(bytes.NewReader(reply)))
	got := parseNeoForgeQuery(body[1:])
	want := []string{"jei:main", "create:sync", "mekanism:net"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
	if ids := parseNeoForgeQuery(body[1:8]); len(ids) > 1 {
		t.Fatalf("truncated input parsed too much: %v", ids)
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
	for _, want := range []string{"minecraft:multi_action", "vecta:join/alpha", "vecta:join/beta"} {
		if !bytes.Contains(dialog, []byte(want)) {
			t.Fatalf("dialog missing %q", want)
		}
	}
	c.send(proto.NewPacket(proto.CfgCustomClickActionID).String("vecta:join/beta").Bool(false).Frame())
	c.expect(proto.CfgStoreCookieID)
	c.expect(proto.CfgTransferID)
}

// readUntil reads frames until one contains all of the given substrings.
func (c *client) readUntil(subs ...string) []byte {
	c.t.Helper()
	for i := 0; i < 50; i++ {
		_, p, err := proto.ReadFrame(c.br)
		if err != nil {
			c.t.Fatalf("waiting for %q: %v", subs, err)
		}
		ok := true
		for _, s := range subs {
			ok = ok && bytes.Contains(p, []byte(s))
		}
		if ok {
			return p
		}
	}
	c.t.Fatalf("no frame containing %q", subs)
	return nil
}

func TestLegacyClientLimboMenuThenReconnect(t *testing.T) {
	a, b := startBackend(t), startBackend(t)
	h := startRouter(t,
		registry.Server{ID: "alpha", Name: "Alpha", Address: a.addr, MinProtocol: 763, MaxProtocol: 763},
		registry.Server{ID: "beta", Name: "Beta", Address: b.addr, MinProtocol: 763, MaxProtocol: 763},
	)
	c := h.dial()
	c.login(763, "play.test", proto.IntentLogin, "Old")
	c.expect(proto.LoginSuccessID)
	c.readUntil("/join alpha")
	c.readUntil("/join beta")
	// 1.20.1 chat_command (0x04): command, timestamp, salt, no signatures, ack state.
	c.send(proto.NewPacket(0x04).String("join beta").Int64(0).Int64(0).VarInt(0).VarInt(0).Raw(make([]byte, 3)).Frame())
	c.readUntil("Reconnect now")
	c.c.Close()

	c = h.dial()
	c.login(763, "play.test", proto.IntentLogin, "Old")
	c.expectBackend()
	if got := waitFrames(t, b); len(got) != 2 {
		t.Fatalf("beta got %d frames", len(got))
	}
}

func TestLobbyHostForcesMenu(t *testing.T) {
	a := startBackend(t)
	h := startRouter(t,
		registry.Server{ID: "alpha", Name: "Alpha", Address: a.addr, MinProtocol: 763, MaxProtocol: 763},
		registry.Server{ID: "newer", Name: "Newer", Address: a.addr, MinProtocol: 774, MaxProtocol: 774},
	)
	c := h.dial()
	c.login(763, "lobby.play.test", proto.IntentLogin, "Chooser")
	c.expect(proto.LoginSuccessID)
	c.readUntil("/join alpha")
}

// forgeServers registers two Forge 1.20.1 packs with advertised channels.
func forgeServers(t *testing.T) (*harness, *backend) {
	create := startBackend(t)
	mek := startBackend(t)
	h := startRouter(t,
		registry.Server{ID: "create-pack", Name: "Create Pack", Address: create.addr, Loader: "forge", MinProtocol: 763, MaxProtocol: 763, Mods: []string{"create"}},
		registry.Server{ID: "mek-pack", Name: "Mek Pack", Address: mek.addr, Loader: "forge", MinProtocol: 763, MaxProtocol: 763, Mods: []string{"mekanism"}},
	)
	h.reg.SetHealth("create-pack", create.addr, registry.Health{Online: true, Protocol: 763, VersionName: "test",
		ForgeChannels: []proto.ForgeChannel{{Name: "create:main", Version: "1"}}})
	h.reg.SetHealth("mek-pack", mek.addr, registry.Health{Online: true, Protocol: 763, VersionName: "test",
		ForgeChannels: []proto.ForgeChannel{{Name: "mekanism:network", Version: "10.4"}}})
	return h, create
}

// readFMLQuery reads the gateway's S2CModList and returns the message id and
// the inner handshake bytes.
func (c *client) readFMLQuery() (int32, []byte) {
	c.t.Helper()
	b := c.expect(proto.LoginPluginRequestID)
	id, _ := b.VarInt()
	if ch, _ := b.String(256); ch != "fml:loginwrapper" {
		c.t.Fatalf("channel %q", ch)
	}
	b.String(256)
	inner, _ := b.ByteArray(1 << 20)
	return id, inner
}

func (c *client) replyFMLMods(msgID int32, mods ...string) {
	inner := proto.NewPacket(2).VarInt(int32(len(mods)))
	for _, m := range mods {
		inner.String(m)
	}
	inner.VarInt(1).String(mods[0] + ":main").String("1").VarInt(0)
	payload := inner.Frame()
	_, body, _ := proto.ReadFrame(bufio.NewReader(bytes.NewReader(payload)))
	wrapper := proto.NewPacket(proto.LoginPluginResponseID).VarInt(msgID).Bool(true).
		String("fml:handshake").ByteArray(body)
	c.send(wrapper.Frame())
}

func TestForgeClientQueriedWithBackendChannels(t *testing.T) {
	h, _ := forgeServers(t)
	c := h.dial()
	c.login(763, "play.test\x00FML3\x00", proto.IntentLogin, "Forgey")
	id, inner := c.readFMLQuery()
	if !bytes.Contains(inner, []byte("create:main")) && !bytes.Contains(inner, []byte("mekanism:network")) {
		t.Fatalf("query lacks backend channels: % x", inner)
	}
	c.replyFMLMods(id, "create")
	reason, _ := c.expect(proto.LoginDisconnectID).String(262144)
	if !strings.Contains(reason, "Create Pack") {
		t.Fatalf("expected match with Create Pack, got %s", reason)
	}
}

func TestForgeRejectionSkipsBackendNextTime(t *testing.T) {
	h, _ := forgeServers(t)
	c := h.dial()
	c.login(763, "play.test\x00FML3\x00", proto.IntentLogin, "Picky")
	_, first := c.readFMLQuery()
	c.c.Close() // client refused that mod list

	firstWasCreate := bytes.Contains(first, []byte("create:main"))
	time.Sleep(100 * time.Millisecond)
	c = h.dial()
	c.login(763, "play.test\x00FML3\x00", proto.IntentLogin, "Picky")
	_, second := c.readFMLQuery()
	if bytes.Contains(second, []byte("create:main")) == firstWasCreate {
		t.Fatalf("second query probed the rejected backend again")
	}
}

func TestTooOldClientGetsExplanation(t *testing.T) {
	a := startBackend(t)
	h := startRouter(t, registry.Server{ID: "alpha", Name: "Alpha", Address: a.addr, MinProtocol: 767, MaxProtocol: 767},
		registry.Server{ID: "beta", Name: "Beta", Address: a.addr, MinProtocol: 767, MaxProtocol: 767})
	c := h.dial()
	c.login(47, "play.test", proto.IntentLogin, "Ancient")
	reason, _ := c.expect(proto.LoginDisconnectID).String(262144)
	if !strings.Contains(reason, "No online server accepts 1.8.x") {
		t.Fatalf("disconnect %s", reason)
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
