package proto

import (
	"bufio"
	"bytes"
	"testing"
)

func TestVarIntRoundTrip(t *testing.T) {
	for _, v := range []int32{0, 1, 127, 128, 255, 25565, 2097151, 2147483647, -1, -2147483648} {
		enc := AppendVarInt(nil, v)
		got, err := ReadVarInt(bytes.NewReader(enc))
		if err != nil || got != v {
			t.Fatalf("%d: got %d, %v", v, got, err)
		}
	}
	if len(AppendVarInt(nil, -1)) != 5 {
		t.Fatal("-1 must encode to 5 bytes")
	}
}

func TestHandshakeRoundTripAndMarker(t *testing.T) {
	in := Handshake{Protocol: 763, Address: "Pack.Play.Test.\x00FML3\x00", Port: 25565, Intent: IntentLogin}
	raw, payload, err := ReadFrame(bufio.NewReader(bytes.NewReader(in.Frame())))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, in.Frame()) {
		t.Fatal("raw frame differs")
	}
	out, err := ParseHandshake(payload)
	if err != nil || out != in {
		t.Fatalf("got %+v, %v", out, err)
	}
	if out.Host() != "pack.play.test" || out.Marker() != "FML3" {
		t.Fatalf("host %q marker %q", out.Host(), out.Marker())
	}
}

func TestNBTEncoding(t *testing.T) {
	got := NetworkNBT(Compound{{Name: "text", Value: "hi"}, {Name: "bold", Value: true}})
	want := []byte{
		10,                          // compound, nameless root
		8, 0, 4, 't', 'e', 'x', 't', // string "text"
		0, 2, 'h', 'i',
		1, 0, 4, 'b', 'o', 'l', 'd', 1, // byte "bold" = 1
		0, // end
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x\nwant % x", got, want)
	}
	if b := NetworkNBT("\x00"); !bytes.Equal(b, []byte{8, 0, 2, 0xc0, 0x80}) {
		t.Fatalf("modified utf-8 null: % x", b)
	}
}

func TestFMLModListReplyParse(t *testing.T) {
	inner := AppendVarInt(nil, 2)
	inner = AppendVarInt(inner, 2)
	inner = NewPacket(0).Raw(inner).String("jei").String("create").b[1:]
	inner = AppendVarInt(inner, 1)
	inner = NewPacket(0).Raw(inner).String("create:main").String("1").b[1:]
	inner = AppendVarInt(inner, 0)
	data := NewPacket(0).String("fml:handshake").ByteArray(inner).b[1:]

	mods, channels, err := ParseFMLModListReply(data)
	if err != nil || len(mods) != 2 || mods[1] != "create" || len(channels) != 1 || channels[0] != "create:main" {
		t.Fatalf("mods %v channels %v err %v", mods, channels, err)
	}
}
