package group.senger.vecta.universal;

import java.io.BufferedInputStream;
import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.util.Map;

/** Server list ping against the local server: tells us it is up, its version and player counts. */
final class StatusPing {
    static final class Status {
        int protocol;
        String versionName = "";
        int players;
        int maxPlayers;
    }

    private StatusPing() {
    }

    static Status ping(String address, boolean proxyProtocol, int timeoutMs) throws IOException {
        HostPort hp = HostPort.parse(address, 25565);
        Socket socket = new Socket();
        try {
            socket.connect(new InetSocketAddress(hp.host, hp.port), timeoutMs);
            socket.setSoTimeout(timeoutMs);

            ByteArrayOutputStream hs = new ByteArrayOutputStream();
            Mc.writeVarInt(hs, 0x00);
            Mc.writeVarInt(hs, -1); // makes ViaVersion report the native version
            Mc.writeString(hs, hp.host);
            hs.write(hp.port >> 8);
            hs.write(hp.port);
            Mc.writeVarInt(hs, 1);

            ByteArrayOutputStream req = new ByteArrayOutputStream();
            if (proxyProtocol) {
                byte[] local = GuardCodec.plainHeader(null, null);
                req.write(local, 0, local.length);
            }
            byte[] f = Mc.frame(hs.toByteArray());
            req.write(f, 0, f.length);
            f = Mc.frame(new byte[] {0x00});
            req.write(f, 0, f.length);
            socket.getOutputStream().write(req.toByteArray());

            InputStream in = new BufferedInputStream(socket.getInputStream());
            ByteArrayInputStream p = new ByteArrayInputStream(Mc.readFrame(in, 1 << 21));
            if (Mc.readVarInt(p) != 0x00) throw new IOException("unexpected status packet");
            Object json = Json.parse(Mc.readString(p, 1 << 21));

            Status st = new Status();
            Map<String, Object> version = Json.obj(Json.obj(json).get("version"));
            st.protocol = Json.integer(version.get("protocol"), 0);
            st.versionName = Json.str(version.get("name"));
            Map<String, Object> players = Json.obj(Json.obj(json).get("players"));
            st.players = Json.integer(players.get("online"), 0);
            st.maxPlayers = Json.integer(players.get("max"), 0);
            return st;
        } catch (IllegalArgumentException e) {
            throw new IOException("bad status response: " + e.getMessage());
        } finally {
            socket.close();
        }
    }
}
