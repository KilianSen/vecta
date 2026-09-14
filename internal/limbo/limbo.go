// Package limbo hosts players of Minecraft 1.7.10-1.20.4 in an empty world with
// a clickable chat menu. It never performs a Forge handshake, so modded
// clients (legacy Forge, Forge 1.13+, Fabric) join it like a vanilla server.
//
// Packet IDs and registry codecs are generated from PrismarineJS
// minecraft-data by tools/limbogen.
package limbo

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"vecta/internal/proto"
)

//go:embed data/*.nbt.gz
var dataFS embed.FS

// Protocol versions with behavior changes the limbo branches on.
const (
	p1_8    = 47
	p1_9    = 107
	p1_9_1  = 108
	p1_12_2 = 340
	p1_13   = 393
	p1_14   = 477
	p1_15   = 573
	p1_16   = 735
	p1_16_2 = 751
	p1_17   = 755
	p1_18   = 757
	p1_19   = 759
	p1_19_4 = 762
	p1_20   = 763
	p1_20_2 = 764
	p1_20_3 = 765
)

// Supported reports whether the limbo speaks this protocol version.
func Supported(protocol int32) bool {
	_, ok := packetTable[protocol]
	return ok
}

// Entry is one server line in the menu.
type Entry struct {
	ID          string
	Name        string
	Detail      string
	Recommended bool
}

type Options struct {
	Protocol int32
	Name     string
	UUID     [16]byte // zero value: offline-mode UUID derived from Name
	Title    string
	// Entries returns the current menu.
	Entries func() []Entry
	// Choose handles "/join <id>". A non-nil disconnect ends the session with
	// that message; otherwise errText is shown in chat.
	Choose func(id string) (disconnect *proto.Text, errText string)
	Log    *slog.Logger
}

type session struct {
	conn net.Conn
	o    Options
	ids  packetIDs
	p    int32
}

// Serve runs the limbo on a connection whose handshake and login start have
// already been read. It returns when the player leaves or chooses a server.
func Serve(conn net.Conn, br *bufio.Reader, o Options) error {
	ids, ok := packetTable[o.Protocol]
	if !ok {
		return fmt.Errorf("limbo: protocol %d not supported", o.Protocol)
	}
	if o.UUID == ([16]byte{}) {
		o.UUID = OfflineUUID(o.Name)
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &session{conn: conn, o: o, ids: ids, p: o.Protocol}
	conn.SetDeadline(time.Now().Add(15 * time.Minute))

	frames := make(chan []byte, 32)
	go func() {
		defer close(frames)
		for {
			_, payload, err := proto.ReadFrame(br)
			if err != nil {
				return
			}
			frames <- payload
		}
	}()

	if err := s.login(frames); err != nil {
		return err
	}
	if err := s.spawn(); err != nil {
		return err
	}
	s.sendMenu()
	return s.loop(frames)
}

func (s *session) send(frames ...[]byte) error {
	for _, f := range frames {
		if _, err := s.conn.Write(f); err != nil {
			return err
		}
	}
	return nil
}

// --- login and configuration ------------------------------------------------

func (s *session) login(frames <-chan []byte) error {
	p := proto.NewPacket(proto.LoginSuccessID)
	if s.p < p1_16 {
		p.String(uuidString(s.o.UUID))
	} else {
		p.Raw(s.o.UUID[:])
	}
	p.String(s.o.Name)
	if s.p >= p1_19 {
		p.VarInt(0) // properties
	}
	if err := s.send(p.Frame()); err != nil {
		return err
	}
	if s.p < p1_20_2 {
		return nil
	}

	if err := waitFor(frames, func(pl []byte) bool { return pl[0] == proto.LoginAcknowledgedID }); err != nil {
		return err
	}
	codec, err := loadCompound(codecFiles[s.p])
	if err != nil {
		return err
	}
	err = s.send(
		// NeoForge query (1.20.2-1.20.4 layout: empty configuration and play
		// sets) before the brand: a NeoForge client that sees a brand first
		// takes the vanilla path and disconnects if it has required channels.
		// Other clients ignore the unknown channel.
		proto.NewPacket(s.ids.CfgPluginMessage).String("neoforge:register").VarInt(0).VarInt(0).Frame(),
		proto.NewPacket(s.ids.CfgPluginMessage).String("minecraft:brand").String("vecta").Frame(),
		proto.NewPacket(s.ids.CfgRegistryData).Raw(proto.CompoundPayloadNBT(codec, false)).Frame(),
		proto.NewPacket(s.ids.CfgFeatureFlags).VarInt(1).String("minecraft:vanilla").Frame(),
		proto.NewPacket(s.ids.CfgFinish).Frame(),
	)
	if err != nil {
		return err
	}
	return waitFor(frames, func(pl []byte) bool { return packetID(pl) == s.ids.InCfgFinish })
}

func waitFor(frames <-chan []byte, match func([]byte) bool) error {
	timeout := time.After(20 * time.Second)
	for {
		select {
		case pl, ok := <-frames:
			if !ok {
				return io.EOF
			}
			if len(pl) > 0 && match(pl) {
				return nil
			}
		case <-timeout:
			return errors.New("limbo: client did not respond")
		}
	}
}

func packetID(pl []byte) int32 {
	id, err := proto.NewBuffer(pl).VarInt()
	if err != nil {
		return -1
	}
	return id
}

// --- world ----------------------------------------------------------------------

const (
	gamemodeAdventure = 2
	spawnY            = 400 // above any world height: no chunks needed before 1.20.2
	viewDistance      = 2
	maxPlayers        = 20
	overworld         = "minecraft:overworld"
)

func (s *session) spawn() error {
	if s.p <= 5 {
		time.Sleep(100 * time.Millisecond) // 1.7 clients drop play packets sent too early
	}
	join, err := s.joinGame()
	if err != nil {
		return err
	}
	out := [][]byte{join}
	if s.p >= p1_13 && s.p < p1_20_2 {
		out = append(out, proto.NewPacket(s.ids.PluginMessage).String("minecraft:brand").String("vecta").Frame())
	}
	out = append(out,
		// invulnerable | flying | allow flying
		proto.NewPacket(s.ids.Abilities).Byte(0x01|0x02|0x04).Float32(0.05).Float32(0.1).Frame(),
		s.spawnPosition(),
		s.position(),
	)
	if s.p >= p1_13 {
		out = append(out, s.commands())
	}
	if s.p >= p1_20_3 {
		out = append(out, proto.NewPacket(s.ids.GameEvent).Byte(13).Float32(0).Frame()) // start waiting for chunks
	}
	if s.p >= p1_20_2 {
		out = append(out,
			proto.NewPacket(s.ids.CenterChunk).VarInt(0).VarInt(0).Frame(),
			s.emptyChunk(),
		)
	}
	return s.send(out...)
}

func (s *session) joinGame() ([]byte, error) {
	p := proto.NewPacket(s.ids.Login).Int32(1)
	switch {
	case s.p < p1_14:
		p.Byte(gamemodeAdventure)
		if s.p < p1_9_1 {
			p.Byte(0)
		} else {
			p.Int32(0)
		}
		p.Byte(0).Byte(maxPlayers).String("flat")
		if s.p >= p1_8 {
			p.Bool(false)
		}
	case s.p < p1_15:
		p.Byte(gamemodeAdventure).Int32(0).Byte(maxPlayers).String("flat").VarInt(viewDistance).Bool(false)
	case s.p < p1_16:
		p.Byte(gamemodeAdventure).Int32(0).Int64(0).Byte(maxPlayers).String("flat").
			VarInt(viewDistance).Bool(false).Bool(true)
	case s.p < p1_16_2:
		codec, err := loadCompound(codecFiles[s.p])
		if err != nil {
			return nil, err
		}
		p.Byte(gamemodeAdventure).Byte(0xFF).VarInt(1).String(overworld).
			Raw(proto.CompoundPayloadNBT(codec, true)).
			String(overworld).String(overworld).Int64(0).Byte(maxPlayers).VarInt(viewDistance).
			Bool(false).Bool(true).Bool(false).Bool(true)
	case s.p < p1_20_2:
		codec, err := loadCompound(codecFiles[s.p])
		if err != nil {
			return nil, err
		}
		p.Bool(false).Byte(gamemodeAdventure).Byte(0xFF).VarInt(1).String(overworld).
			Raw(proto.CompoundPayloadNBT(codec, true))
		if s.p < p1_19 {
			dim, err := loadCompound(dimensionFiles[s.p])
			if err != nil {
				return nil, err
			}
			p.Raw(proto.CompoundPayloadNBT(dim, true))
		} else {
			p.String(overworld)
		}
		p.String(overworld).Int64(0).VarInt(maxPlayers).VarInt(viewDistance)
		if s.p >= p1_18 {
			p.VarInt(viewDistance) // simulation distance
		}
		p.Bool(false).Bool(true).Bool(false).Bool(true)
		if s.p >= p1_19 {
			p.Bool(false) // no death location
		}
		if s.p >= p1_20 {
			p.VarInt(0) // portal cooldown
		}
	default:
		p.Bool(false).VarInt(1).String(overworld).VarInt(maxPlayers).VarInt(viewDistance).VarInt(viewDistance).
			Bool(false).Bool(true).Bool(false).
			String(overworld).String(overworld).Int64(0).Byte(gamemodeAdventure).Byte(0xFF).
			Bool(false).Bool(true).Bool(false).VarInt(0)
	}
	return p.Frame(), nil
}

func (s *session) spawnPosition() []byte {
	p := proto.NewPacket(s.ids.SpawnPosition)
	switch {
	case s.p < p1_8:
		p.Int32(0).Int32(spawnY).Int32(0)
	case s.p < p1_14:
		p.Int64(int64(spawnY&0xFFF) << 26)
	default:
		p.Int64(int64(spawnY & 0xFFF))
	}
	if s.p >= p1_17 {
		p.Float32(0)
	}
	return p.Frame()
}

func (s *session) position() []byte {
	p := proto.NewPacket(s.ids.Position)
	if s.p < p1_8 {
		return p.Float64(0.5).Float64(spawnY + 1.62).Float64(0.5).Float32(0).Float32(0).Bool(false).Frame()
	}
	p.Float64(0.5).Float64(spawnY).Float64(0.5).Float32(0).Float32(0).Byte(0)
	if s.p >= p1_9 {
		p.VarInt(1) // teleport id
	}
	if s.p >= p1_17 && s.p < p1_19_4 {
		p.Bool(false) // dismount vehicle
	}
	return p.Frame()
}

// commands declares "/join <server>" and "/servers" so 1.13+ clients send them.
func (s *session) commands() []byte {
	p := proto.NewPacket(s.ids.Commands).VarInt(4)
	p.Byte(0x00).VarInt(2).VarInt(1).VarInt(2)      // 0: root
	p.Byte(0x01).VarInt(1).VarInt(3).String("join") // 1: literal join
	p.Byte(0x01 | 0x04).VarInt(0).String("servers") // 2: literal servers (executable)
	p.Byte(0x02 | 0x04).VarInt(0).String("server")  // 3: argument (executable)
	if s.p < p1_19 {
		p.String("brigadier:string")
	} else {
		p.VarInt(5) // brigadier:string
	}
	p.VarInt(0) // single word
	return p.VarInt(0).Frame()
}

// emptyChunk is an all-air chunk at 0,0 for 1.20.2+ (24 sections overworld).
func (s *session) emptyChunk() []byte {
	const sections = 24
	var data []byte
	for i := 0; i < sections; i++ {
		data = append(data, 0, 0)    // block count
		data = append(data, 0, 0, 0) // blocks: single-valued air, no data
		data = append(data, 0, 0, 0) // biomes: single-valued id 0, no data
	}
	lightBits := int64(1)<<(sections+2) - 1
	return proto.NewPacket(s.ids.ChunkData).Int32(0).Int32(0).
		Raw(proto.NetworkNBT(proto.Compound{{Name: "MOTION_BLOCKING", Value: make([]int64, 37)}})).
		ByteArray(data).
		VarInt(0).           // block entities
		VarInt(0).VarInt(0). // sky / block light masks
		VarInt(1).Int64(lightBits).
		VarInt(1).Int64(lightBits).
		VarInt(0).VarInt(0). // light arrays
		Frame()
}

// --- chat menu -------------------------------------------------------------------

func (s *session) chat(t proto.Text) []byte {
	switch {
	case s.p < p1_8:
		return proto.NewPacket(s.ids.Chat).String(t.JSON()).Frame()
	case s.p < p1_16:
		return proto.NewPacket(s.ids.Chat).String(t.JSON()).Byte(1).Frame()
	case s.p < p1_19:
		return proto.NewPacket(s.ids.Chat).String(t.JSON()).Byte(1).Raw(make([]byte, 16)).Frame()
	case s.p == p1_19:
		return proto.NewPacket(s.ids.SystemChat).String(t.JSON()).VarInt(1).Frame()
	case s.p < p1_20_3:
		return proto.NewPacket(s.ids.SystemChat).String(t.JSON()).Bool(false).Frame()
	default:
		return proto.NewPacket(s.ids.SystemChat).Raw(proto.NetworkNBT(t.NBT())).Bool(false).Frame()
	}
}

func (s *session) disconnect(t proto.Text) []byte {
	if s.p >= p1_20_3 {
		return proto.NewPacket(s.ids.Disconnect).Raw(proto.NetworkNBT(t.NBT())).Frame()
	}
	return proto.NewPacket(s.ids.Disconnect).String(t.JSON()).Frame()
}

func (s *session) sendMenu() {
	title := s.o.Title
	if title == "" {
		title = "Choose a server"
	}
	lines := []proto.Text{proto.C("", "white"), proto.Text{Text: "» " + title, Color: "gold", Bold: true}}
	entries := s.o.Entries()
	if len(entries) == 0 {
		lines = append(lines, proto.C("  No server accepts your version right now.", "red"))
	}
	for _, e := range entries {
		color := "aqua"
		if e.Recommended {
			color = "green"
		}
		hover := proto.T("Click to join " + e.Name)
		name := proto.Text{Text: e.Name, Color: color, Bold: e.Recommended, Click: "/join " + e.ID, Hover: &hover}
		arrow := proto.Text{Text: "  [Join] ", Color: "yellow", Click: "/join " + e.ID, Hover: &hover}
		lines = append(lines, proto.Join(arrow, name, proto.C("  "+e.Detail, "gray")))
	}
	lines = append(lines, proto.C("Click a server, or type /join <id>. /servers refreshes the list.", "dark_gray"))
	for _, l := range lines {
		s.send(s.chat(l))
	}
}

func (s *session) loop(frames <-chan []byte) error {
	keepAlive := time.NewTicker(5 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case pl, ok := <-frames:
			if !ok {
				return nil
			}
			if cmd, ok := s.command(pl); ok {
				if done := s.handleCommand(cmd); done {
					return nil
				}
			}
		case <-keepAlive.C:
			id := time.Now().Unix()
			p := proto.NewPacket(s.ids.KeepAlive)
			switch {
			case s.p < p1_8:
				p.Int32(int32(id))
			case s.p < p1_12_2:
				p.VarInt(int32(id & 0x7FFFFFFF))
			default:
				p.Int64(id)
			}
			if err := s.send(p.Frame()); err != nil {
				return err
			}
		}
	}
}

// command extracts a slash command (without the slash) from a chat packet.
func (s *session) command(pl []byte) (string, bool) {
	b := proto.NewBuffer(pl)
	id, err := b.VarInt()
	if err != nil {
		return "", false
	}
	switch {
	case s.p >= p1_19 && id == s.ids.InChatCommand:
		cmd, err := b.String(32767)
		return cmd, err == nil
	case s.p < p1_19 && id == s.ids.InChat:
		msg, err := b.String(32767)
		if err != nil || !strings.HasPrefix(msg, "/") {
			return "", false
		}
		return msg[1:], true
	}
	return "", false
}

func (s *session) handleCommand(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToLower(fields[0]) {
	case "join", "server":
		if len(fields) < 2 {
			s.send(s.chat(proto.C("Usage: /join <server>", "red")))
			return false
		}
		msg, errText := s.o.Choose(strings.ToLower(fields[1]))
		if msg != nil {
			s.send(s.disconnect(*msg))
			return true
		}
		s.send(s.chat(proto.C(errText, "red")))
	case "servers", "menu", "list":
		s.sendMenu()
	default:
		s.send(s.chat(proto.C("Unknown command. Use /join <server> or /servers.", "red")))
	}
	return false
}

// --- helpers --------------------------------------------------------------------

var compoundCache sync.Map

// loadCompound returns an embedded compound payload (entries + end tag).
func loadCompound(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("limbo: no registry data for this version")
	}
	if v, ok := compoundCache.Load(path); ok {
		return v.([]byte), nil
	}
	raw, err := dataFS.ReadFile(path)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	compoundCache.Store(path, data)
	return data, nil
}

// OfflineUUID is the UUID an offline-mode server assigns to a player name.
func OfflineUUID(name string) [16]byte {
	u := md5.Sum([]byte("OfflinePlayer:" + name))
	u[6] = u[6]&0x0f | 0x30
	u[8] = u[8]&0x3f | 0x80
	return u
}

func uuidString(u [16]byte) string {
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

var _ = binary.BigEndian
