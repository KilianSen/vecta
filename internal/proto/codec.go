// Package proto implements the small subset of the Minecraft Java protocol the
// gateway needs: framing, handshake, login and configuration packets.
// Compression and encryption are never enabled by the gateway itself; once a
// player is routed the connection becomes an opaque byte pipe.
package proto

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// MaxFrameLen is the largest frame the vanilla protocol allows (3-byte VarInt).
const MaxFrameLen = 2097151

var (
	ErrVarIntTooBig = errors.New("proto: varint too big")
	ErrShortBuffer  = errors.New("proto: unexpected end of packet")
	ErrFrameTooBig  = errors.New("proto: frame too big")
)

// ReadVarInt reads a protocol VarInt (max 5 bytes).
func ReadVarInt(r io.ByteReader) (int32, error) {
	var v uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint32(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return int32(v), nil
		}
	}
	return 0, ErrVarIntTooBig
}

// AppendVarInt appends v encoded as VarInt.
func AppendVarInt(b []byte, v int32) []byte {
	u := uint32(v)
	for u >= 0x80 {
		b = append(b, byte(u)|0x80)
		u >>= 7
	}
	return append(b, byte(u))
}

// ReadFrame reads one length-prefixed frame. raw holds the frame exactly as it
// appeared on the wire (length prefix included) so it can be replayed to a
// backend; payload is the packet ID followed by the packet body.
func ReadFrame(r *bufio.Reader) (raw, payload []byte, err error) {
	var prefix []byte
	var n uint32
	for i := 0; ; i++ {
		if i == 3 {
			return nil, nil, ErrFrameTooBig
		}
		b, err := r.ReadByte()
		if err != nil {
			return nil, nil, err
		}
		prefix = append(prefix, b)
		n |= uint32(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			break
		}
	}
	if n == 0 || n > MaxFrameLen {
		return nil, nil, fmt.Errorf("proto: invalid frame length %d", n)
	}
	raw = make([]byte, len(prefix)+int(n))
	copy(raw, prefix)
	if _, err := io.ReadFull(r, raw[len(prefix):]); err != nil {
		return nil, nil, err
	}
	return raw, raw[len(prefix):], nil
}

// Buffer reads fields from a packet payload.
type Buffer struct {
	b   []byte
	off int
}

func NewBuffer(b []byte) *Buffer { return &Buffer{b: b} }

func (b *Buffer) ReadByte() (byte, error) {
	if b.off >= len(b.b) {
		return 0, ErrShortBuffer
	}
	c := b.b[b.off]
	b.off++
	return c, nil
}

func (b *Buffer) VarInt() (int32, error) { return ReadVarInt(b) }

func (b *Buffer) Bytes(n int) ([]byte, error) {
	if n < 0 || b.off+n > len(b.b) {
		return nil, ErrShortBuffer
	}
	s := b.b[b.off : b.off+n]
	b.off += n
	return s, nil
}

func (b *Buffer) Bool() (bool, error) {
	c, err := b.ReadByte()
	return c != 0, err
}

func (b *Buffer) Uint16() (uint16, error) {
	s, err := b.Bytes(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(s), nil
}

func (b *Buffer) Int64() (int64, error) {
	s, err := b.Bytes(8)
	if err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(s)), nil
}

// ByteArray reads a VarInt-prefixed byte array.
func (b *Buffer) ByteArray(max int) ([]byte, error) {
	n, err := b.VarInt()
	if err != nil {
		return nil, err
	}
	if int(n) > max {
		return nil, fmt.Errorf("proto: byte array length %d exceeds %d", n, max)
	}
	return b.Bytes(int(n))
}

// String reads a VarInt-prefixed UTF-8 string of at most max characters.
func (b *Buffer) String(max int) (string, error) {
	s, err := b.ByteArray(max * 3)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(s) || utf8.RuneCount(s) > max {
		return "", fmt.Errorf("proto: invalid string")
	}
	return string(s), nil
}

func (b *Buffer) Remaining() []byte { return b.b[b.off:] }

// Builder encodes a packet.
type Builder struct{ b []byte }

func NewPacket(id int32) *Builder { return &Builder{b: AppendVarInt(nil, id)} }

func (p *Builder) VarInt(v int32) *Builder { p.b = AppendVarInt(p.b, v); return p }
func (p *Builder) Raw(v []byte) *Builder   { p.b = append(p.b, v...); return p }

func (p *Builder) Bool(v bool) *Builder {
	if v {
		p.b = append(p.b, 1)
	} else {
		p.b = append(p.b, 0)
	}
	return p
}

func (p *Builder) Uint16(v uint16) *Builder { p.b = binary.BigEndian.AppendUint16(p.b, v); return p }
func (p *Builder) Int64(v int64) *Builder {
	p.b = binary.BigEndian.AppendUint64(p.b, uint64(v))
	return p
}

func (p *Builder) ByteArray(v []byte) *Builder {
	p.b = AppendVarInt(p.b, int32(len(v)))
	p.b = append(p.b, v...)
	return p
}

func (p *Builder) String(v string) *Builder { return p.ByteArray([]byte(v)) }

// Frame returns the length-prefixed wire encoding.
func (p *Builder) Frame() []byte {
	out := AppendVarInt(make([]byte, 0, len(p.b)+3), int32(len(p.b)))
	return append(out, p.b...)
}
