// Package guard implements anymcp's signed PROXY protocol v2 header. The
// gateway attaches it to connections to guarded backends; the guard in the
// universal server jar verifies it and refuses everything else, so players
// cannot bypass the gateway. Wire format: docs/guard-protocol.md.
package guard

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	// TLVType is the PROXY v2 TLV carrying the signature (custom range).
	TLVType = 0xE0
	// Version of the signed TLV value.
	Version = 1
	// NonceLen and MACLen are the TLV value field sizes.
	NonceLen = 12
	MACLen   = 32
	// MaxSkew is the allowed clock difference between gateway and guard.
	MaxSkew = 120 * time.Second
	// MaxHeaderLen bounds the header a guard reads before verifying.
	MaxHeaderLen = 16 + 1024

	tlvValueLen = 1 + 8 + NonceLen + MACLen // 53
	cmdLocal    = 0x20
	cmdProxy    = 0x21
	famUnspec   = 0x00
	famTCP4     = 0x11
	famTCP6     = 0x21
	macContext  = "anymcp-guard-v1"
	keyContext  = "anymcp-guard-key-v1:"
)

var signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

// Key derives the HMAC key from a shared secret: the owner token for
// registered servers, guardSecret for static ones.
func Key(secret string) []byte {
	sum := sha256.Sum256([]byte(keyContext + secret))
	return sum[:]
}

// NewNonce returns a random nonce.
func NewNonce() []byte {
	n := make([]byte, NonceLen)
	if _, err := rand.Read(n); err != nil {
		panic("guard: crypto/rand failed: " + err.Error())
	}
	return n
}

// Header builds a signed PROXY header for a proxied connection src -> dst.
// If either address is not TCP, it falls back to a signed LOCAL header.
func Header(key []byte, src, dst net.Addr, now time.Time, nonce []byte) []byte {
	s, ok1 := src.(*net.TCPAddr)
	d, ok2 := dst.(*net.TCPAddr)
	if !ok1 || !ok2 {
		return LocalHeader(key, now, nonce)
	}
	var addr []byte
	fam := byte(famTCP4)
	if s4, d4 := s.IP.To4(), d.IP.To4(); s4 != nil && d4 != nil {
		addr = append(append(addr, s4...), d4...)
	} else {
		fam = famTCP6
		addr = append(append(addr, s.IP.To16()...), d.IP.To16()...)
	}
	addr = binary.BigEndian.AppendUint16(addr, uint16(s.Port))
	addr = binary.BigEndian.AppendUint16(addr, uint16(d.Port))
	return build(key, cmdProxy, fam, addr, now, nonce)
}

// LocalHeader builds a signed LOCAL header, used for health checks.
func LocalHeader(key []byte, now time.Time, nonce []byte) []byte {
	return build(key, cmdLocal, famUnspec, nil, now, nonce)
}

func mac(key []byte, verCmd, fam byte, addr []byte, ts uint64, nonce []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(macContext))
	h.Write([]byte{verCmd, fam})
	h.Write(addr)
	h.Write([]byte{Version})
	var tsb [8]byte
	binary.BigEndian.PutUint64(tsb[:], ts)
	h.Write(tsb[:])
	h.Write(nonce)
	return h.Sum(nil)
}

func build(key []byte, verCmd, fam byte, addr []byte, now time.Time, nonce []byte) []byte {
	if len(nonce) != NonceLen {
		panic("guard: nonce must be 12 bytes")
	}
	ts := uint64(now.Unix())
	tlv := []byte{TLVType, 0, tlvValueLen, Version}
	tlv = binary.BigEndian.AppendUint64(tlv, ts)
	tlv = append(tlv, nonce...)
	tlv = append(tlv, mac(key, verCmd, fam, addr, ts, nonce)...)

	out := append([]byte{}, signature...)
	out = append(out, verCmd, fam)
	out = binary.BigEndian.AppendUint16(out, uint16(len(addr)+len(tlv)))
	out = append(out, addr...)
	return append(out, tlv...)
}

// ReadHeader reads one complete PROXY v2 header (signature through TLVs).
func ReadHeader(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	if !bytes.Equal(hdr[:12], signature) {
		return nil, errors.New("guard: not a PROXY v2 header")
	}
	n := int(binary.BigEndian.Uint16(hdr[14:16]))
	if 16+n > MaxHeaderLen {
		return nil, fmt.Errorf("guard: header too long (%d)", n)
	}
	out := make([]byte, 16+n)
	copy(out, hdr)
	if _, err := io.ReadFull(r, out[16:]); err != nil {
		return nil, err
	}
	return out, nil
}

// Result describes a verified header.
type Result struct {
	Local    bool
	Src, Dst *net.TCPAddr // nil for LOCAL
}

// Verify checks a complete header. seen reports (and records) whether a
// nonce was used before; pass nil to skip replay protection.
func Verify(key, header []byte, now time.Time, seen func(nonce []byte) bool) (Result, error) {
	var res Result
	if len(header) < 16 || !bytes.Equal(header[:12], signature) {
		return res, errors.New("guard: not a PROXY v2 header")
	}
	verCmd, fam := header[12], header[13]
	body := header[16:]
	if int(binary.BigEndian.Uint16(header[14:16])) != len(body) {
		return res, errors.New("guard: length mismatch")
	}
	var addrLen int
	switch {
	case verCmd == cmdLocal:
		res.Local = true
		addrLen = 0
	case verCmd == cmdProxy && fam == famTCP4:
		addrLen = 12
	case verCmd == cmdProxy && fam == famTCP6:
		addrLen = 36
	default:
		return res, fmt.Errorf("guard: unsupported command/family %#x/%#x", verCmd, fam)
	}
	if len(body) < addrLen {
		return res, errors.New("guard: short address block")
	}
	addr, tlvs := body[:addrLen], body[addrLen:]

	var value []byte
	for len(tlvs) > 0 {
		if len(tlvs) < 3 {
			return res, errors.New("guard: truncated TLV")
		}
		t, l := tlvs[0], int(binary.BigEndian.Uint16(tlvs[1:3]))
		if len(tlvs) < 3+l {
			return res, errors.New("guard: truncated TLV")
		}
		if t == TLVType {
			if value != nil {
				return res, errors.New("guard: duplicate signature TLV")
			}
			value = tlvs[3 : 3+l]
		}
		tlvs = tlvs[3+l:]
	}
	if len(value) != tlvValueLen || value[0] != Version {
		return res, errors.New("guard: missing or invalid signature TLV")
	}
	ts := binary.BigEndian.Uint64(value[1:9])
	nonce := value[9 : 9+NonceLen]
	got := value[9+NonceLen:]
	if !hmac.Equal(got, mac(key, verCmd, fam, addr, ts, nonce)) {
		return res, errors.New("guard: bad signature")
	}
	if d := now.Sub(time.Unix(int64(ts), 0)); d > MaxSkew || d < -MaxSkew {
		return res, fmt.Errorf("guard: timestamp outside allowed skew (%s)", d)
	}
	if seen != nil && seen(nonce) {
		return res, errors.New("guard: replayed nonce")
	}
	if !res.Local {
		ipLen := 4
		if fam == famTCP6 {
			ipLen = 16
		}
		res.Src = &net.TCPAddr{IP: net.IP(append([]byte{}, addr[:ipLen]...)), Port: int(binary.BigEndian.Uint16(addr[2*ipLen:]))}
		res.Dst = &net.TCPAddr{IP: net.IP(append([]byte{}, addr[ipLen:2*ipLen]...)), Port: int(binary.BigEndian.Uint16(addr[2*ipLen+2:]))}
	}
	return res, nil
}
