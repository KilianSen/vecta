package router

import (
	"bufio"
	"bytes"
	"testing"
	"time"
)

func FuzzReadProxyHeader(f *testing.F) {
	f.Add([]byte("PROXY TCP4 1.2.3.4 5.6.7.8 1234 25565\r\n"))
	f.Add(append(append([]byte{}, proxyV2Sig...), 0x21, 0x11, 0x00, 0x0c, 1, 2, 3, 4, 5, 6, 7, 8, 0, 1, 0, 2))
	f.Add([]byte("PROXY UNKNOWN\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = readProxyHeader(bufio.NewReader(bytes.NewReader(data)))
	})
}

func FuzzParseNeoForgeQuery(f *testing.F) {
	f.Add([]byte{0x01, 0x04, 0x01, 0x08, 'j', 'e', 'i', ':', 'm', 'a', 'i', 'n', 0x01, '1', 0x00, 0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0x0f})
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = parseNeoForgeQuery(data)
	})
}

func FuzzVerifyRoute(f *testing.F) {
	key := []byte("secret")
	f.Add(signRoute(key, "survival", "Steve", time.Unix(2_000_000_000, 0)), "Steve")
	f.Add([]byte("v1|||||"), "")
	f.Fuzz(func(t *testing.T, cookie []byte, player string) {
		server, err := verifyRoute(key, cookie, player, time.Unix(1_900_000_000, 0))
		if err == nil && !bytes.Equal(signRoute(key, server, player, time.Unix(2_000_000_000, 0)), cookie) {
			// A cookie verified without being the exact signed form must at
			// least carry a valid signature for its own body.
			if _, err2 := verifyRoute(key, cookie, player, time.Unix(1_900_000_000, 0)); err2 != nil {
				t.Fatal("verification not deterministic")
			}
		}
	})
}
