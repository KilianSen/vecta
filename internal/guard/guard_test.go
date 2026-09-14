package guard

import (
	"bytes"
	"encoding/hex"
	"net"
	"testing"
	"time"
)

var (
	testKey   = Key("tok-plugin")
	testNow   = time.Unix(1_800_000_000, 0)
	testNonce = bytes.Repeat([]byte{7}, NonceLen)
	src4      = &net.TCPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 51234}
	dst4      = &net.TCPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 25565}
)

func TestRoundTrip(t *testing.T) {
	src6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 4000}
	dst6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::2"), Port: 25565}
	for name, h := range map[string][]byte{
		"ipv4":  Header(testKey, src4, dst4, testNow, testNonce),
		"ipv6":  Header(testKey, src6, dst6, testNow, testNonce),
		"local": LocalHeader(testKey, testNow, testNonce),
	} {
		read, err := ReadHeader(bytes.NewReader(append(h, "rest"...)))
		if err != nil || !bytes.Equal(read, h) {
			t.Fatalf("%s: ReadHeader: %v", name, err)
		}
		res, err := Verify(testKey, h, testNow.Add(30*time.Second), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		switch name {
		case "ipv4":
			if res.Local || res.Src.String() != src4.String() || res.Dst.String() != dst4.String() {
				t.Fatalf("ipv4 result %+v", res)
			}
		case "ipv6":
			if res.Src.String() != src6.String() || res.Dst.String() != dst6.String() {
				t.Fatalf("ipv6 result %+v", res)
			}
		case "local":
			if !res.Local || res.Src != nil {
				t.Fatalf("local result %+v", res)
			}
		}
	}
}

func TestRejects(t *testing.T) {
	good := Header(testKey, src4, dst4, testNow, testNonce)
	tampered := append([]byte{}, good...)
	tampered[16] ^= 1 // source IP
	plain := append(append([]byte{}, signature...), 0x21, 0x11, 0, 12, 1, 2, 3, 4, 5, 6, 7, 8, 0, 1, 0, 2)

	cases := map[string]struct {
		key    []byte
		header []byte
		now    time.Time
	}{
		"wrong key":     {Key("other"), good, testNow},
		"tampered addr": {testKey, tampered, testNow},
		"unsigned":      {testKey, plain, testNow},
		"too old":       {testKey, good, testNow.Add(MaxSkew + time.Second)},
		"from future":   {testKey, good, testNow.Add(-MaxSkew - time.Second)},
		"garbage":       {testKey, []byte("\x10\x00GET / HTTP/1.1"), testNow},
	}
	for name, c := range cases {
		if _, err := Verify(c.key, c.header, c.now, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReplay(t *testing.T) {
	seen := map[string]bool{}
	check := func(n []byte) bool {
		k := string(n)
		was := seen[k]
		seen[k] = true
		return was
	}
	h := LocalHeader(testKey, testNow, testNonce)
	if _, err := Verify(testKey, h, testNow, check); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(testKey, h, testNow, check); err == nil {
		t.Fatal("replayed header accepted")
	}
}

// TestVector pins the wire format; the Java guard has the same vector.
func TestVector(t *testing.T) {
	h := Header(testKey, src4, dst4, testNow, testNonce)
	const want = "0d0a0d0a000d0a515549540a" + "2111" + "0044" +
		"cb007109" + "0a000005" + "c822" + "63dd" +
		"e00035" + "01" + "000000006b49d200" + "070707070707070707070707"
	got := hex.EncodeToString(h)
	if got[:len(want)] != want {
		t.Fatalf("header prefix\n got %s\nwant %s", got[:len(want)], want)
	}
	t.Logf("vector: %s", got)
}

func FuzzVerify(f *testing.F) {
	f.Add(Header(testKey, src4, dst4, testNow, testNonce))
	f.Add(LocalHeader(testKey, testNow, testNonce))
	f.Fuzz(func(t *testing.T, data []byte) {
		if h, err := ReadHeader(bytes.NewReader(data)); err == nil {
			Verify(testKey, h, testNow, nil)
		}
		Verify(testKey, data, testNow, nil)
	})
}
