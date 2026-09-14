package group.senger.anymcp.universal;

import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.Map;

/**
 * Per-protocol packet IDs and formats the command handler needs, loaded from the generated
 * packets.json resource (tools/limbogen/gen.py). Formats that changed at known versions (chat
 * component encoding, the legacy chat packet's fields) are decided here by protocol thresholds.
 */
final class Packets {
    // Protocol thresholds for format changes.
    static final int P_1_16 = 735;   // clientbound chat gained a sender UUID
    static final int P_1_19 = 759;   // system_chat introduced; 1.19.0 uses a varint type
    static final int P_1_19_1 = 760; // system_chat uses a boolean overlay
    static final int P_1_20_3 = 765; // text components sent as NBT, not JSON
    static final int P_1_20_5 = 766; // transfer and cookies

    static final class Ids {
        int inChat = -1;
        int inChatCommand = -1;
        int inChatCommandSigned = -1;
        int systemChat = -1;
        int chat = -1;
        int disconnect = -1;
        int transfer = -1;
        int storeCookie = -1;
    }

    private static final Map<Integer, Ids> TABLE = new HashMap<Integer, Ids>();
    private static volatile boolean loaded;

    private Packets() {
    }

    static synchronized void load() {
        if (loaded) return;
        loaded = true;
        InputStream in = Packets.class.getResourceAsStream("/packets.json");
        if (in == null) {
            Log.warn("packets.json missing from the jar; commands are disabled");
            return;
        }
        try {
            byte[] raw = ModScanner.readAll(in, 1 << 20);
            Map<String, Object> table = Json.obj(Json.parse(new String(raw, StandardCharsets.UTF_8)));
            for (Map.Entry<String, Object> e : table.entrySet()) {
                Map<String, Object> o = Json.obj(e.getValue());
                Ids ids = new Ids();
                ids.inChat = Json.integer(o.get("inChat"), -1);
                ids.inChatCommand = Json.integer(o.get("inChatCommand"), -1);
                ids.inChatCommandSigned = Json.integer(o.get("inChatCommandSigned"), -1);
                ids.systemChat = Json.integer(o.get("systemChat"), -1);
                ids.chat = Json.integer(o.get("chat"), -1);
                ids.disconnect = Json.integer(o.get("disconnect"), -1);
                ids.transfer = Json.integer(o.get("transfer"), -1);
                ids.storeCookie = Json.integer(o.get("storeCookie"), -1);
                try {
                    TABLE.put(Integer.parseInt(e.getKey()), ids);
                } catch (NumberFormatException ignored) {
                    // skip non-numeric keys
                }
            }
            Log.debug("loaded packet ids for " + TABLE.size() + " protocols");
        } catch (Exception ex) {
            Log.warn("could not read packets.json; commands are disabled", ex);
        } finally {
            try {
                in.close();
            } catch (Exception ignored) {
                // nothing to do
            }
        }
    }

    static Ids forProtocol(int protocol) {
        load();
        return TABLE.get(protocol);
    }

    static boolean known(int protocol) {
        return forProtocol(protocol) != null;
    }
}
