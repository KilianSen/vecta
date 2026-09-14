package proto

import (
	"bufio"
	"bytes"
	"testing"
)

// Parsers see untrusted bytes from anyone who connects; they must never
// panic or allocate without bound.

func FuzzReadFrame(f *testing.F) {
	f.Add(Handshake{Protocol: 767, Address: "play.test", Port: 25565, Intent: 2}.Frame())
	f.Add([]byte{0xff, 0xff, 0xff, 0x7f})
	f.Add([]byte{0x05, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		br := bufio.NewReader(bytes.NewReader(data))
		for i := 0; i < 8; i++ {
			raw, payload, err := ReadFrame(br)
			if err != nil {
				return
			}
			if len(payload) > len(raw) || len(raw) > MaxFrameLen+3 {
				t.Fatalf("inconsistent frame: raw %d payload %d", len(raw), len(payload))
			}
		}
	})
}

func FuzzParseHandshake(f *testing.F) {
	hs := Handshake{Protocol: 763, Address: "pack.play.test\x00FML3\x00", Port: 25565, Intent: 2}.Frame()
	_, payload, _ := ReadFrame(bufio.NewReader(bytes.NewReader(hs)))
	f.Add(payload)
	f.Add([]byte{0x00, 0xff, 0xff, 0xff, 0xff, 0x0f})
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ParseHandshake(data)
		if err != nil {
			return
		}
		_ = h.Host()
		_ = h.Marker()
		if _, _, err := ReadFrame(bufio.NewReader(bytes.NewReader(h.Frame()))); err != nil && len(h.Address) < 30000 {
			t.Fatalf("re-encoded handshake unreadable: %v", err)
		}
	})
}

func FuzzParseLoginStart(f *testing.F) {
	f.Add([]byte{0x00, 0x05, 'S', 't', 'e', 'v', 'e'}, int32(767))
	f.Add([]byte{0x00, 0x7f}, int32(47))
	f.Fuzz(func(t *testing.T, data []byte, protocol int32) {
		ls, err := ParseLoginStart(data, protocol)
		if err == nil && len([]rune(ls.Name)) > 16 {
			t.Fatalf("name longer than 16: %q", ls.Name)
		}
	})
}

func FuzzParseFMLModListReply(f *testing.F) {
	inner := AppendVarInt(nil, 2)
	inner = AppendVarInt(inner, 1)
	inner = NewPacket(0).Raw(inner).String("create").b[1:]
	inner = AppendVarInt(inner, 0)
	f.Add(NewPacket(0).String("fml:handshake").ByteArray(inner).b[1:])
	f.Add([]byte{0x0d})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = ParseFMLModListReply(data)
	})
}

func FuzzForgeDataDecode(f *testing.F) {
	f.Add(string([]rune{4, 0, 0x4142, 0x0043}))
	f.Add("")
	f.Add(string([]rune{0x7fff, 0x7fff}))
	f.Fuzz(func(t *testing.T, d string) {
		fd := ForgeData{D: d}
		_, _, _, _ = fd.Decode()
	})
}

func FuzzBufferReads(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x03})
	f.Fuzz(func(t *testing.T, data []byte) {
		b := NewBuffer(data)
		for i := 0; i < 16; i++ {
			switch i % 5 {
			case 0:
				_, _ = b.VarInt()
			case 1:
				_, _ = b.String(32767)
			case 2:
				_, _ = b.ByteArray(1 << 16)
			case 3:
				_, _ = b.Int64()
			case 4:
				_, _ = b.Bool()
			}
		}
	})
}
