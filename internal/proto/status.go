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
	ForgeData   *struct {
		Mods []struct {
			ModID string `json:"modId"`
		} `json:"mods"`
		FMLNetworkVersion int `json:"fmlNetworkVersion"`
	} `json:"forgeData,omitempty"`
	ModInfo *struct {
		Type    string `json:"type"`
		ModList []struct {
			ModID string `json:"modid"`
		} `json:"modList"`
	} `json:"modinfo,omitempty"`
	IsModded bool `json:"isModded,omitempty"` // NeoForge
}

// ModIDs returns mod IDs advertised in forgeData (1.13+) or modinfo (<=1.12).
func (s *Status) ModIDs() []string {
	var ids []string
	if s.ForgeData != nil {
		for _, m := range s.ForgeData.Mods {
			ids = append(ids, m.ModID)
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

// Ping performs a server list ping against address (host:port). With
// proxyProtocol set, the connection starts with a PROXY v2 LOCAL header.
func Ping(ctx context.Context, address string, protocol int32, proxyProtocol bool) (*Status, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", address)
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
	var req []byte
	if proxyProtocol {
		req = append(req, proxyV2Local...)
	}
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
