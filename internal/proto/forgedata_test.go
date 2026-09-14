package proto

import (
	"reflect"
	"testing"
)

// encodeForgeOptimized mirrors Forge's ServerStatusPing encoder: byte length
// in two 15-bit chars, then payload bytes packed 15 bits per char.
func encodeForgeOptimized(data []byte) string {
	chars := []rune{rune(len(data) & 0x7FFF), rune(len(data) >> 15 & 0x7FFF)}
	var buf uint32
	bits := 0
	for _, b := range data {
		buf |= uint32(b) << bits
		bits += 8
		if bits >= 15 {
			chars = append(chars, rune(buf&0x7FFF))
			buf >>= 15
			bits -= 15
		}
	}
	if bits > 0 {
		chars = append(chars, rune(buf&0x7FFF))
	}
	return string(chars)
}

func TestForgeDataCompressed(t *testing.T) {
	// create (with version, 2 channels), jei (IGNORESERVERONLY, no channels),
	// plus one non-mod channel.
	p := NewPacket(0).Bool(false).Uint16(2).
		VarInt(2 << 1).String("create").String("0.5.1.f").
		String("main").String("1").Bool(false).
		String("sync").String("2").Bool(true).
		VarInt(0<<1 | 1).String("jei").
		VarInt(1).String("forge:tier_sorting").String("1.0").Bool(true)
	data := p.b[1:]

	fd := ForgeData{D: encodeForgeOptimized(data), FMLNetworkVersion: 3}
	mods, channels, truncated, err := fd.Decode()
	if err != nil {
		t.Fatal(err)
	}
	wantMods := []ForgeMod{{ID: "create", Version: "0.5.1.f"}, {ID: "jei"}}
	wantCh := []ForgeChannel{
		{Name: "create:main", Version: "1"},
		{Name: "create:sync", Version: "2", AcceptsAbsent: true},
		{Name: "forge:tier_sorting", Version: "1.0", AcceptsAbsent: true},
	}
	if truncated || !reflect.DeepEqual(mods, wantMods) || !reflect.DeepEqual(channels, wantCh) {
		t.Fatalf("mods %+v channels %+v truncated %v", mods, channels, truncated)
	}
}

func TestForgeDataOptimizedLengths(t *testing.T) {
	for n := 0; n < 64; n++ {
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(i*37 + 11)
		}
		got, err := decodeForgeOptimized(encodeForgeOptimized(data))
		if err != nil || !reflect.DeepEqual(got, data) {
			t.Fatalf("len %d: got % x err %v", n, got, err)
		}
	}
}

func TestForgeDataPlainJSON(t *testing.T) {
	var fd ForgeData
	fd.Mods = append(fd.Mods, struct {
		ModID  string `json:"modId"`
		Marker string `json:"modmarker"`
	}{"mekanism", "10.0"})
	fd.Channels = append(fd.Channels, struct {
		Res      string `json:"res"`
		Version  string `json:"version"`
		Required bool   `json:"required"`
	}{"mekanism:network", "10.0", false})
	mods, channels, _, err := fd.Decode()
	if err != nil || mods[0].ID != "mekanism" || channels[0].Name != "mekanism:network" || channels[0].Version != "10.0" {
		t.Fatalf("mods %+v channels %+v err %v", mods, channels, err)
	}
}
