package proto

import (
	"encoding/binary"
	"fmt"
)

// NBT tag type IDs.
const (
	tagEnd       = 0
	tagByte      = 1
	tagInt       = 3
	tagString    = 8
	tagList      = 9
	tagCompound  = 10
	tagLongArray = 12
)

// Field is one named entry of a Compound; order is preserved on the wire.
type Field struct {
	Name  string
	Value any
}

// Compound is an ordered NBT compound. Supported value types: string, int32,
// int, bool (as byte), Compound and List.
type Compound []Field

// List is a homogeneous NBT list.
type List []any

// NetworkNBT encodes v as nameless-root network NBT (1.20.2+).
func NetworkNBT(v any) []byte {
	out := []byte{tagID(v)}
	return appendNBT(out, v)
}

func tagID(v any) byte {
	switch v.(type) {
	case string:
		return tagString
	case int32, int:
		return tagInt
	case bool:
		return tagByte
	case Compound:
		return tagCompound
	case List:
		return tagList
	case []int64:
		return tagLongArray
	}
	panic(fmt.Sprintf("nbt: unsupported type %T", v))
}

// NamedRootNBT encodes a compound with an empty root name, as used by the
// protocol before 1.20.2.
func NamedRootNBT(c Compound) []byte {
	return appendNBT([]byte{tagCompound, 0, 0}, c)
}

// CompoundPayloadNBT wraps a pre-encoded compound payload (entries + end tag)
// as network NBT, named (pre-1.20.2) or nameless.
func CompoundPayloadNBT(payload []byte, named bool) []byte {
	out := []byte{tagCompound}
	if named {
		out = append(out, 0, 0)
	}
	return append(out, payload...)
}

func appendNBT(b []byte, v any) []byte {
	switch t := v.(type) {
	case string:
		return appendNBTString(b, t)
	case int32:
		return binary.BigEndian.AppendUint32(b, uint32(t))
	case int:
		return binary.BigEndian.AppendUint32(b, uint32(int32(t)))
	case bool:
		if t {
			return append(b, 1)
		}
		return append(b, 0)
	case Compound:
		for _, f := range t {
			b = append(b, tagID(f.Value))
			b = appendNBTString(b, f.Name)
			b = appendNBT(b, f.Value)
		}
		return append(b, tagEnd)
	case List:
		if len(t) == 0 {
			b = append(b, tagEnd)
			return binary.BigEndian.AppendUint32(b, 0)
		}
		b = append(b, tagID(t[0]))
		b = binary.BigEndian.AppendUint32(b, uint32(len(t)))
		for _, e := range t {
			b = appendNBT(b, e)
		}
		return b
	case []int64:
		b = binary.BigEndian.AppendUint32(b, uint32(len(t)))
		for _, e := range t {
			b = binary.BigEndian.AppendUint64(b, uint64(e))
		}
		return b
	}
	panic(fmt.Sprintf("nbt: unsupported type %T", v))
}

// appendNBTString writes a Java "modified UTF-8" string with a u16 length.
func appendNBTString(b []byte, s string) []byte {
	var enc []byte
	for _, r := range s {
		switch {
		case r != 0 && r < 0x80:
			enc = append(enc, byte(r))
		case r < 0x800:
			enc = append(enc, 0xc0|byte(r>>6), 0x80|byte(r&0x3f))
		case r < 0x10000:
			enc = append(enc, 0xe0|byte(r>>12), 0x80|byte((r>>6)&0x3f), 0x80|byte(r&0x3f))
		default: // supplementary: encode each UTF-16 surrogate as 3 bytes
			r -= 0x10000
			for _, s := range []rune{0xd800 + (r >> 10), 0xdc00 + (r & 0x3ff)} {
				enc = append(enc, 0xe0|byte(s>>12), 0x80|byte((s>>6)&0x3f), 0x80|byte(s&0x3f))
			}
		}
	}
	if len(enc) > 0xffff {
		enc = enc[:0xffff]
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(enc)))
	return append(b, enc...)
}
