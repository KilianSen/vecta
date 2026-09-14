package group.senger.anymcp.universal;

import java.io.ByteArrayOutputStream;
import java.io.DataOutputStream;
import java.io.IOException;
import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Builds clientbound chat/disconnect/transfer/cookie packet bodies for any protocol. A text
 * component is sent as JSON before 1.20.3 and as network NBT after; the legacy chat packet's fields
 * differ again by version. IDs come from {@link Packets}.
 */
final class Chat {
    private Chat() {
    }

    static String json(String text, String color) {
        Map<String, Object> m = new LinkedHashMap<String, Object>();
        m.put("text", text);
        if (color != null) m.put("color", color);
        return Json.write(m);
    }

    /** A text component as anonymous network NBT (compound with no root name), for 1.20.3+. */
    static byte[] nbt(String text, String color) {
        ByteArrayOutputStream b = new ByteArrayOutputStream();
        DataOutputStream d = new DataOutputStream(b);
        try {
            d.writeByte(0x0A); // TAG_Compound, no name (network form)
            d.writeByte(0x08); // TAG_String
            d.writeUTF("text");
            d.writeUTF(text);
            if (color != null) {
                d.writeByte(0x08);
                d.writeUTF("color");
                d.writeUTF(color);
            }
            d.writeByte(0x00); // TAG_End
        } catch (IOException e) {
            throw new IllegalStateException(e); // ByteArrayOutputStream never throws
        }
        return b.toByteArray();
    }

    private static void component(ByteArrayOutputStream out, int protocol, String text, String color) {
        if (protocol >= Packets.P_1_20_3) {
            byte[] n = nbt(text, color);
            out.write(n, 0, n.length);
        } else {
            Mc.writeString(out, json(text, color));
        }
    }

    /** A system/overlay-off chat message, or null if this protocol can't show one. */
    static byte[] systemChat(Packets.Ids ids, int protocol, String text, String color) {
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        if (ids.systemChat >= 0) {
            Mc.writeVarInt(out, ids.systemChat);
            component(out, protocol, text, color);
            if (protocol == Packets.P_1_19) {
                Mc.writeVarInt(out, 1); // 1.19.0: chat type (1 = system)
            } else {
                out.write(0); // boolean overlay = false
            }
            return out.toByteArray();
        }
        if (ids.chat >= 0) { // pre-1.19 clientbound chat
            Mc.writeVarInt(out, ids.chat);
            Mc.writeString(out, json(text, color));
            out.write(1); // position: 1 = system message
            if (protocol >= Packets.P_1_16) {
                for (int i = 0; i < 16; i++) out.write(0); // sender UUID (zeros)
            }
            return out.toByteArray();
        }
        return null;
    }

    static byte[] disconnect(Packets.Ids ids, int protocol, String text, String color) {
        if (ids.disconnect < 0) return null;
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        Mc.writeVarInt(out, ids.disconnect);
        component(out, protocol, text, color);
        return out.toByteArray();
    }

    static byte[] storeCookie(Packets.Ids ids, String key, byte[] value) {
        if (ids.storeCookie < 0) return null;
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        Mc.writeVarInt(out, ids.storeCookie);
        Mc.writeString(out, key);
        Mc.writeVarInt(out, value.length);
        out.write(value, 0, value.length);
        return out.toByteArray();
    }

    static byte[] transfer(Packets.Ids ids, String host, int port) {
        if (ids.transfer < 0) return null;
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        Mc.writeVarInt(out, ids.transfer);
        Mc.writeString(out, host);
        Mc.writeVarInt(out, port);
        return out.toByteArray();
    }
}
