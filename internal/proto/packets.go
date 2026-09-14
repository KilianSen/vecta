package proto

import (
	"fmt"
	"strings"
)

// Handshake intents.
const (
	IntentStatus   = 1
	IntentLogin    = 2
	IntentTransfer = 3
)

// Protocol version numbers the gateway branches on.
const (
	Proto1_13   = 393
	Proto1_18   = 757
	Proto1_20_1 = 763
	Proto1_20_2 = 764
	Proto1_20_5 = 766 // cookies + transfer
	Proto1_21_2 = 768 // login success loses strictErrorHandling
	Proto1_21_6 = 771 // dialogs
)

// Login state packet IDs (stable from 1.20.5 through 26.x).
const (
	LoginDisconnectID     = 0x00 // clientbound, JSON text
	LoginSuccessID        = 0x02
	LoginPluginRequestID  = 0x04
	LoginCookieRequestID  = 0x05
	LoginStartID          = 0x00 // serverbound
	LoginPluginResponseID = 0x02
	LoginAcknowledgedID   = 0x03
	LoginCookieResponseID = 0x04
)

// Configuration state packet IDs (1.20.5+; appended-only through 26.x).
const (
	CfgPluginMessageOutID = 0x01
	CfgDisconnectID       = 0x02 // NBT text
	CfgKeepAliveOutID     = 0x04
	CfgStoreCookieID      = 0x0A
	CfgTransferID         = 0x0B
	CfgShowDialogID       = 0x12 // 1.21.6+

	CfgClientInformationID = 0x00
	CfgCookieResponseID    = 0x01
	CfgPluginMessageInID   = 0x02
	CfgKeepAliveInID       = 0x04
	CfgCustomClickActionID = 0x08 // 1.21.6+
)

// Handshake is the first packet of every connection.
type Handshake struct {
	Protocol int32
	Address  string // raw, including any Forge marker
	Port     uint16
	Intent   int32
}

func ParseHandshake(payload []byte) (Handshake, error) {
	var h Handshake
	b := NewBuffer(payload)
	id, err := b.VarInt()
	if err != nil {
		return h, err
	}
	if id != 0 {
		return h, fmt.Errorf("proto: expected handshake, got packet 0x%02x", id)
	}
	if h.Protocol, err = b.VarInt(); err != nil {
		return h, err
	}
	if h.Address, err = b.String(1024); err != nil { // Forge markers exceed 255
		return h, err
	}
	if h.Port, err = b.Uint16(); err != nil {
		return h, err
	}
	h.Intent, err = b.VarInt()
	return h, err
}

func (h Handshake) Frame() []byte {
	return NewPacket(0).VarInt(h.Protocol).String(h.Address).Uint16(h.Port).VarInt(h.Intent).Frame()
}

// Host returns the hostname the client typed, without markers, trailing dot
// or case.
func (h Handshake) Host() string {
	host, _, _ := strings.Cut(h.Address, "\x00")
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// Marker returns the loader marker appended to the address ("FML", "FML2",
// "FML3", "FORGE..."), or "".
func (h Handshake) Marker() string {
	_, rest, ok := strings.Cut(h.Address, "\x00")
	if !ok {
		return ""
	}
	marker, _, _ := strings.Cut(rest, "\x00")
	return marker
}

// LoginStart holds the fields the gateway reads from the first login packet.
type LoginStart struct {
	Name    string
	UUID    [16]byte
	HasUUID bool
}

func ParseLoginStart(payload []byte, protocol int32) (LoginStart, error) {
	var ls LoginStart
	b := NewBuffer(payload)
	id, err := b.VarInt()
	if err != nil {
		return ls, err
	}
	if id != LoginStartID {
		return ls, fmt.Errorf("proto: expected login start, got packet 0x%02x", id)
	}
	if ls.Name, err = b.String(16); err != nil {
		return ls, err
	}
	if protocol >= Proto1_20_2 {
		u, err := b.Bytes(16)
		if err == nil {
			copy(ls.UUID[:], u)
			ls.HasUUID = true
		}
	}
	return ls, nil
}

// LoginDisconnect builds a login-state disconnect (JSON text in all versions).
func LoginDisconnect(t Text) []byte {
	return NewPacket(LoginDisconnectID).String(t.JSON()).Frame()
}

// LoginSuccess builds the login success packet for protocol >= 1.20.5.
func LoginSuccess(protocol int32, uuid [16]byte, name string) []byte {
	p := NewPacket(LoginSuccessID).Raw(uuid[:]).String(name).VarInt(0)
	if protocol < Proto1_21_2 {
		p.Bool(true) // strictErrorHandling
	}
	return p.Frame()
}

// VersionName maps a protocol number to a human readable release.
func VersionName(protocol int32) string {
	if v, ok := versionNames[protocol]; ok {
		return v
	}
	return fmt.Sprintf("protocol %d", protocol)
}

var versionNames = map[int32]string{
	5: "1.7.10", 47: "1.8.x", 110: "1.9.4", 210: "1.10.x", 316: "1.11.2", 340: "1.12.2",
	393: "1.13", 401: "1.13.1", 404: "1.13.2", 477: "1.14", 480: "1.14.1", 485: "1.14.2",
	490: "1.14.3", 498: "1.14.4", 573: "1.15", 575: "1.15.1", 578: "1.15.2", 735: "1.16",
	736: "1.16.1", 751: "1.16.2", 753: "1.16.3", 754: "1.16.4-5", 755: "1.17", 756: "1.17.1",
	757: "1.18-1.18.1", 758: "1.18.2", 759: "1.19", 760: "1.19.1-2", 761: "1.19.3",
	762: "1.19.4", 763: "1.20-1.20.1", 764: "1.20.2", 765: "1.20.3-4", 766: "1.20.5-6",
	767: "1.21-1.21.1", 768: "1.21.2-3", 769: "1.21.4", 770: "1.21.5", 771: "1.21.6",
	772: "1.21.7-8", 773: "1.21.9-10", 774: "1.21.11", 775: "26.1.x", 776: "26.2",
}

// FMLModListRequest builds a login plugin request wrapping an FML2/FML3
// S2CModList that advertises no mods, prompting the client to reply with its
// own mod list (C2SModListReply).
func FMLModListRequest(messageID int32, fml3 bool) []byte {
	inner := AppendVarInt(nil, 1)  // S2CModList
	inner = AppendVarInt(inner, 0) // mods
	inner = AppendVarInt(inner, 0) // channels
	inner = AppendVarInt(inner, 0) // registries
	if fml3 {
		inner = AppendVarInt(inner, 0) // data pack registries
	}
	wrapper := NewPacket(0).String("fml:handshake").ByteArray(inner)
	data := wrapper.b[1:] // drop the dummy packet ID
	return NewPacket(LoginPluginRequestID).VarInt(messageID).String("fml:loginwrapper").Raw(data).Frame()
}

// ParseFMLModListReply decodes a login plugin response payload carrying a
// C2SModListReply and returns the client's mod IDs and channel names.
func ParseFMLModListReply(data []byte) (mods, channels []string, err error) {
	b := NewBuffer(data)
	if ch, err := b.String(256); err != nil || ch != "fml:handshake" {
		return nil, nil, fmt.Errorf("proto: unexpected fml wrapper channel %q", ch)
	}
	inner, err := b.ByteArray(MaxFrameLen)
	if err != nil {
		return nil, nil, err
	}
	b = NewBuffer(inner)
	if id, err := b.VarInt(); err != nil || id != 2 {
		return nil, nil, fmt.Errorf("proto: expected C2SModListReply, got %d", id)
	}
	n, err := b.VarInt()
	if err != nil {
		return nil, nil, err
	}
	for i := int32(0); i < n; i++ {
		m, err := b.String(256)
		if err != nil {
			return nil, nil, err
		}
		mods = append(mods, m)
	}
	if n, err = b.VarInt(); err != nil {
		return mods, nil, nil
	}
	for i := int32(0); i < n; i++ {
		c, err := b.String(256)
		if err != nil {
			return mods, channels, nil
		}
		if _, err := b.String(256); err != nil {
			return mods, channels, nil
		}
		channels = append(channels, c)
	}
	return mods, channels, nil
}
