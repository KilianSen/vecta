package router

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

var proxyV2Sig = []byte("\r\n\r\n\x00\r\nQUIT\n")

// readProxyHeader consumes a PROXY protocol v1 or v2 header (as sent by
// nginx / Nginx Proxy Manager streams with proxy_protocol on) and returns the
// original client address. A nil address means the header carried none
// (LOCAL / UNKNOWN).
func readProxyHeader(br *bufio.Reader) (net.Addr, error) {
	peek, err := br.Peek(12)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(peek, proxyV2Sig) {
		return readProxyV2(br)
	}
	if string(peek[:6]) == "PROXY " {
		return readProxyV1(br)
	}
	return nil, errors.New("missing PROXY protocol header")
}

func readProxyV1(br *bufio.Reader) (net.Addr, error) {
	line, err := br.ReadSlice('\n')
	if err != nil {
		return nil, fmt.Errorf("proxy v1: %w", err)
	}
	f := strings.Fields(strings.TrimSpace(string(line)))
	if len(f) >= 2 && f[1] == "UNKNOWN" {
		return nil, nil
	}
	if len(f) != 6 {
		return nil, errors.New("proxy v1: malformed header")
	}
	ip := net.ParseIP(f[2])
	port, err := strconv.Atoi(f[4])
	if ip == nil || err != nil {
		return nil, errors.New("proxy v1: bad source address")
	}
	return &net.TCPAddr{IP: ip, Port: port}, nil
}

func readProxyV2(br *bufio.Reader) (net.Addr, error) {
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return nil, err
	}
	verCmd, fam := hdr[12], hdr[13]
	n := binary.BigEndian.Uint16(hdr[14:16])
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	if verCmd>>4 != 2 {
		return nil, errors.New("proxy v2: bad version")
	}
	if verCmd&0x0f == 0 { // LOCAL
		return nil, nil
	}
	switch fam >> 4 {
	case 1: // IPv4
		if len(body) < 12 {
			return nil, errors.New("proxy v2: short ipv4 block")
		}
		return &net.TCPAddr{IP: net.IP(body[0:4]), Port: int(binary.BigEndian.Uint16(body[8:10]))}, nil
	case 2: // IPv6
		if len(body) < 36 {
			return nil, errors.New("proxy v2: short ipv6 block")
		}
		return &net.TCPAddr{IP: net.IP(body[0:16]), Port: int(binary.BigEndian.Uint16(body[32:34]))}, nil
	}
	return nil, nil
}

// proxyV2Header builds a PROXY v2 header announcing src -> dst.
func proxyV2Header(src, dst net.Addr) []byte {
	s, ok1 := src.(*net.TCPAddr)
	d, ok2 := dst.(*net.TCPAddr)
	out := append([]byte{}, proxyV2Sig...)
	if !ok1 || !ok2 {
		return append(out, 0x20, 0x00, 0x00, 0x00) // LOCAL
	}
	if s4, d4 := s.IP.To4(), d.IP.To4(); s4 != nil && d4 != nil {
		out = append(out, 0x21, 0x11, 0x00, 12)
		out = append(out, s4...)
		out = append(out, d4...)
	} else {
		out = append(out, 0x21, 0x21, 0x00, 36)
		out = append(out, s.IP.To16()...)
		out = append(out, d.IP.To16()...)
	}
	out = binary.BigEndian.AppendUint16(out, uint16(s.Port))
	return binary.BigEndian.AppendUint16(out, uint16(d.Port))
}
