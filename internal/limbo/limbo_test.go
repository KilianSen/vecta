package limbo

import (
	"bufio"
	"bytes"
	"net"
	"sort"
	"testing"
	"time"

	"anymcp/internal/proto"
)

// TestAllVersions runs every supported protocol through login, the optional
// configuration phase, spawn, the menu and a /join command.
func TestAllVersions(t *testing.T) {
	protocols := make([]int, 0, len(packetTable))
	for p := range packetTable {
		protocols = append(protocols, int(p))
	}
	sort.Ints(protocols)
	for _, p := range protocols {
		p := int32(p)
		t.Run(proto.VersionName(p), func(t *testing.T) {
			t.Parallel()
			runClient(t, p)
		})
	}
}

func runClient(t *testing.T, p int32) {
	ids := packetTable[p]
	srv, cli := net.Pipe()
	defer cli.Close()
	chosen := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(srv, bufio.NewReader(srv), Options{
			Protocol: p,
			Name:     "Tester",
			Entries: func() []Entry {
				return []Entry{{ID: "alpha", Name: "Alpha", Detail: "paper", Recommended: true}, {ID: "beta", Name: "Beta"}}
			},
			Choose: func(id string) (*proto.Text, string) {
				chosen <- id
				msg := proto.T("Reconnect to join " + id)
				return &msg, ""
			},
		})
		srv.Close()
	}()

	cli.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(cli)
	next := func() (int32, []byte) {
		_, pl, err := proto.ReadFrame(br)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		b := proto.NewBuffer(pl)
		id, _ := b.VarInt()
		return id, b.Remaining()
	}
	expect := func(want int32, what string) []byte {
		for i := 0; i < 64; i++ {
			if id, body := next(); id == want {
				return body
			}
		}
		t.Fatalf("%s (0x%02x) not received", what, want)
		return nil
	}
	send := func(frame []byte) {
		if _, err := cli.Write(frame); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	expect(proto.LoginSuccessID, "login success")
	if p >= p1_20_2 {
		send(proto.NewPacket(proto.LoginAcknowledgedID).Frame())
		body := expect(ids.CfgRegistryData, "registry data")
		if len(body) < 100 || body[0] != 10 {
			t.Fatalf("registry data looks wrong: % x", body[:min(len(body), 8)])
		}
		expect(ids.CfgFinish, "finish configuration")
		send(proto.NewPacket(ids.InCfgFinish).Frame())
	}

	join := expect(ids.Login, "join game")
	if p >= p1_16 && p < p1_20_2 && !bytes.Contains(join, []byte("minecraft:overworld")) {
		t.Fatal("join game lacks overworld dimension")
	}
	expect(ids.Position, "position")
	menuID := ids.Chat
	if p >= p1_19 {
		menuID = ids.SystemChat
	}
	for {
		body := expect(menuID, "menu chat")
		if bytes.Contains(body, []byte("/join beta")) {
			break
		}
	}

	if p >= p1_19 {
		send(proto.NewPacket(ids.InChatCommand).String("join beta").Int64(0).Int64(0).VarInt(0).Frame())
	} else {
		send(proto.NewPacket(ids.InChat).String("/join beta").Frame())
	}
	body := expect(ids.Disconnect, "disconnect")
	if !bytes.Contains(body, []byte("Reconnect to join beta")) {
		t.Fatalf("disconnect body % x", body)
	}
	if got := <-chosen; got != "beta" {
		t.Fatalf("chose %q", got)
	}
	cli.Close()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}
