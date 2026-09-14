package proto

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Status is the parsed server list ping response.
type Status struct {
	Version struct {
		Name     string `json:"name"`
		Protocol int32  `json:"protocol"`
	} `json:"version"`
	Players struct {
		Max    int `json:"max"`
		Online int `json:"online"`
	} `json:"players"`
	Description json.RawMessage `json:"description,omitempty"`
	ForgeData   *ForgeData      `json:"forgeData,omitempty"`
	ModInfo     *struct {
		Type    string `json:"type"`
		ModList []struct {
			ModID string `json:"modid"`
		} `json:"modList"`
	} `json:"modinfo,omitempty"`
	IsModded bool `json:"isModded,omitempty"` // NeoForge
}

// ForgeData is the `forgeData` object of a Forge 1.13-1.20.1 status response.
// Up to 1.18.1 mods and channels are plain JSON; later versions put them in
// the compressed `d` string (the plain fields are then placeholders).
type ForgeData struct {
	Channels []struct {
		Res      string `json:"res"`
		Version  string `json:"version"`
		Required bool   `json:"required"`
	} `json:"channels"`
	Mods []struct {
		ModID  string `json:"modId"`
		Marker string `json:"modmarker"`
	} `json:"mods"`
	FMLNetworkVersion int    `json:"fmlNetworkVersion"`
	Truncated         bool   `json:"truncated"`
	D                 string `json:"d,omitempty"`
}

// ForgeChannel is a network channel and its exact protocol version string.
type ForgeChannel struct {
	Name    string
	Version string
	// AcceptsAbsent is Forge's "required" flag: true when the server accepts
	// clients that lack this channel.
	AcceptsAbsent bool
}

// ForgeMod is a mod advertised by a Forge server.
type ForgeMod struct {
	ID      string
	Version string // empty when the mod is marked IGNORESERVERONLY
}

// Decode returns the server's mods and channels. truncated reports that the
// server cut the lists short, so channels may be missing.
func (f *ForgeData) Decode() (mods []ForgeMod, channels []ForgeChannel, truncated bool, err error) {
	if f.D == "" {
		for _, m := range f.Mods {
			mods = append(mods, ForgeMod{ID: m.ModID, Version: m.Marker})
		}
		for _, c := range f.Channels {
			channels = append(channels, ForgeChannel{Name: c.Res, Version: c.Version, AcceptsAbsent: c.Required})
		}
		return mods, channels, f.Truncated, nil
	}
	data, err := decodeForgeOptimized(f.D)
	if err != nil {
		return nil, nil, false, err
	}
	b := NewBuffer(data)
	if truncated, err = b.Bool(); err != nil {
		return nil, nil, false, err
	}
	modCount, err := b.Uint16()
	if err != nil {
		return nil, nil, false, err
	}
	for i := 0; i < int(modCount); i++ {
		flags, err := b.VarInt()
		if err != nil {
			return mods, channels, truncated, err
		}
		id, err := b.String(32767)
		if err != nil {
			return mods, channels, truncated, err
		}
		m := ForgeMod{ID: id}
		if flags&1 == 0 {
			if m.Version, err = b.String(32767); err != nil {
				return mods, channels, truncated, err
			}
		}
		mods = append(mods, m)
		for j := 0; j < int(uint32(flags)>>1); j++ {
			ch, err := readForgeChannel(b)
			if err != nil {
				return mods, channels, truncated, err
			}
			ch.Name = id + ":" + ch.Name
			channels = append(channels, ch)
		}
	}
	n, err := b.VarInt()
	if err != nil {
		return mods, channels, truncated, err
	}
	for i := int32(0); i < n; i++ {
		ch, err := readForgeChannel(b)
		if err != nil {
			return mods, channels, truncated, err
		}
		channels = append(channels, ch)
	}
	return mods, channels, truncated, nil
}

func readForgeChannel(b *Buffer) (ForgeChannel, error) {
	var ch ForgeChannel
	var err error
	if ch.Name, err = b.String(32767); err != nil {
		return ch, err
	}
	if ch.Version, err = b.String(32767); err != nil {
		return ch, err
	}
	ch.AcceptsAbsent, err = b.Bool()
	return ch, err
}

// decodeForgeOptimized reverses Forge's packing of bytes into 15-bit chars:
// two chars of length, then the payload bits little-endian.
func decodeForgeOptimized(s string) ([]byte, error) {
	chars := []rune(s)
	if len(chars) < 2 {
		return nil, fmt.Errorf("proto: forgeData d too short")
	}
	size := int(chars[0]) | int(chars[1])<<15
	out := make([]byte, 0, size)
	var buf uint32
	bits := 0
	for _, c := range chars[2:] {
		if c > 0x7FFF {
			return nil, fmt.Errorf("proto: forgeData d has invalid char")
		}
		buf |= uint32(c) << bits
		bits += 15
		for bits >= 8 {
			out = append(out, byte(buf))
			buf >>= 8
			bits -= 8
		}
	}
	for len(out) < size && bits > 0 {
		out = append(out, byte(buf))
		buf >>= 8
		bits -= 8
	}
	if len(out) < size {
		return nil, fmt.Errorf("proto: forgeData d shorter than declared size")
	}
	return out[:size], nil
}

// ModIDs returns mod IDs advertised in forgeData (1.13+) or modinfo (<=1.12).
func (s *Status) ModIDs() []string {
	var ids []string
	if s.ForgeData != nil {
		mods, _, _, _ := s.ForgeData.Decode()
		for _, m := range mods {
			ids = append(ids, m.ID)
		}
	}
	if s.ModInfo != nil {
		for _, m := range s.ModInfo.ModList {
			ids = append(ids, m.ModID)
		}
	}
	return ids
}

// proxyV2Local is a PROXY protocol v2 header with the LOCAL command: required
// by backends that expect PROXY protocol, and tells them to use the real
// socket address for this health-check connection.
var proxyV2Local = []byte("\r\n\r\n\x00\r\nQUIT\n\x20\x00\x00\x00")

// ProxyV2Local returns a copy of the unsigned PROXY v2 LOCAL header.
func ProxyV2Local() []byte { return append([]byte{}, proxyV2Local...) }

// Ping performs a server list ping against address (host:port). preamble,
// if not nil, is sent first (e.g. a PROXY v2 LOCAL header). dial may be nil
// for direct dialing.
func Ping(ctx context.Context, address string, protocol int32, preamble []byte,
	dial func(ctx context.Context, network, address string) (net.Conn, error)) (*Status, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(5 * time.Second))
	}

	hs := Handshake{Protocol: protocol, Address: host, Port: uint16(port), Intent: IntentStatus}
	req := append([]byte{}, preamble...)
	req = append(req, hs.Frame()...)
	req = append(req, NewPacket(0).Frame()...)
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}
	_, payload, err := ReadFrame(bufio.NewReader(conn))
	if err != nil {
		return nil, err
	}
	b := NewBuffer(payload)
	if id, err := b.VarInt(); err != nil || id != 0 {
		return nil, fmt.Errorf("proto: unexpected status packet")
	}
	js, err := b.String(32767 * 4)
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal([]byte(js), &st); err != nil {
		return nil, fmt.Errorf("proto: bad status json: %w", err)
	}
	return &st, nil
}

// StatusResponse builds a status response frame for the given JSON document.
func StatusResponse(doc any) []byte {
	b, _ := json.Marshal(doc)
	return NewPacket(0).String(string(b)).Frame()
}

// DescriptionText flattens a status description to plain text (best effort).
func DescriptionText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var t struct {
		Text  string            `json:"text"`
		Extra []json.RawMessage `json:"extra"`
	}
	if json.Unmarshal(raw, &t) != nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(t.Text)
	for _, e := range t.Extra {
		sb.WriteString(DescriptionText(e))
	}
	return sb.String()
}
