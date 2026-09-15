package group.senger.vecta.universal;

import java.io.ByteArrayInputStream;
import java.util.Arrays;
import java.util.Base64;
import java.util.HashSet;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.ThreadFactory;

/**
 * In-server /hub, /server and /global, implemented at the Netty layer with no platform APIs. A
 * per-connection handler catches the chat/command packet; /hub and /server request a transfer
 * ticket from the gateway and send the client the resulting transfer or reconnect packet; /global
 * (operators only) posts to the gateway, which relays it to every server's players.
 */
final class Commands {
    private static final Set<String> KEYWORDS = new HashSet<String>(Arrays.asList("hub", "lobby", "server", "global"));
    private static final long PLAY_READY_MS = 500; // interceptor attaches post-login; small settle buffer
    private static final String COLOR = "yellow";

    private final Config cfg;
    private final GatewayClient gateway;
    private final Ops ops;
    private final NettyRef ref;
    private final ExecutorService pool = Executors.newCachedThreadPool(daemon("vecta-cmd"));
    private final ConcurrentHashMap<Object, Conn> live = new ConcurrentHashMap<Object, Conn>();
    private final java.util.concurrent.atomic.AtomicInteger childCounter = new java.util.concurrent.atomic.AtomicInteger();
    private volatile long globalSeq;
    private volatile int serverProtocol = -1;

    private Commands(Config cfg, NettyRef ref) {
        this.cfg = cfg;
        this.ref = ref;
        this.gateway = new GatewayClient(cfg.gateway, cfg.token);
        this.ops = new Ops(cfg.serverDir);
    }

    static final class Conn {
        final Object channel;
        final int protocol;
        final String player;
        final long readyAt;

        Conn(Object channel, int protocol, String player) {
            this.channel = channel;
            this.protocol = protocol;
            this.player = player;
            this.readyAt = System.currentTimeMillis() + PLAY_READY_MS;
        }
    }

    /** Installs command handling if the server's Netty can be reached. No-op on failure. */
    static Commands start(Config cfg) {
        if (cfg.registrationProblem() != null) return null; // needs gateway+token to do anything
        Packets.load();
        List<Object> servers = Netty.serverChannels();
        if (servers.isEmpty()) {
            Log.warn("commands: no server channel found yet; /hub, /server and /global are off");
            return null;
        }
        try {
            NettyRef ref = new NettyRef(servers.get(0).getClass().getClassLoader());
            Commands c = new Commands(cfg, ref);
            c.detectServerProtocol();
            int wired = 0;
            for (Object server : servers) {
                if (c.install(server)) wired++;
            }
            if (wired == 0) {
                Log.warn("commands: could not attach to any server channel");
                return null;
            }
            c.startGlobalPoll();
            Log.info("commands: /hub, /server and /global active");
            return c;
        } catch (Throwable t) {
            Log.warn("commands: setup failed", t);
            return null;
        }
    }

    /**
     * The server's own protocol — what our handler sees on the wire, since it sits after any
     * ViaVersion decoder. Packets are parsed and built with this; the client's handshake protocol
     * (which differs for Via clients) is used only for the transfer-ticket decision.
     */
    private void detectServerProtocol() {
        for (int i = 0; i < 5 && serverProtocol <= 0; i++) {
            try {
                int p = StatusPing.ping(cfg.localAddress(), cfg.proxyProtocol, 3000).protocol;
                if (p > 0) {
                    serverProtocol = p;
                    Log.debug("commands: server wire protocol " + p);
                    return;
                }
            } catch (Exception e) {
                Log.debug("commands: server protocol ping failed: " + e);
            }
            sleep(1000);
        }
    }

    private int wireFor(int clientProtocol) {
        return serverProtocol > 0 ? serverProtocol : clientProtocol;
    }

    /** Replaces the server bootstrap's child handler so every new connection gets our sniffer. */
    private boolean install(Object server) {
        try {
            Object pipeline = ref.pipeline(server);
            Log.debug("commands: server channel " + server.getClass().getName() + " handlers=" + ref.names(pipeline));
            for (String name : ref.names(pipeline)) {
                Object h = ref.get(pipeline, name);
                if (h == null || !h.getClass().getName().contains("ServerBootstrapAcceptor")) continue;
                final Object original = RuntimeProbe.field(h, "childHandler");
                if (original == null) return false;
                if ("vecta-child".equals(String.valueOf(original))) return true; // already installed
                Object wrapper = ref.newChildHandler(new NettyRef.Added() {
                    @Override
                    public void handlerAdded(Object ctx) throws Exception {
                        onChildAdded(ctx, original);
                    }
                });
                boolean ok = RuntimeProbe.setField(h, "childHandler", wrapper);
                Log.debug("commands: replaced childHandler on " + name + " = " + ok);
                return ok;
            }
            Log.debug("commands: no ServerBootstrapAcceptor in server pipeline");
        } catch (Throwable t) {
            Log.debug("commands: install on server channel failed: " + t);
        }
        return false;
    }

    /**
     * A new player connection: add the platform's own child initializer (which builds the MC
     * pipeline), then our handshake sniffer at the head. Our shared wrapper stays in the pipeline as
     * a harmless pass-through.
     */
    private void onChildAdded(Object wrapperCtx, Object original) throws Exception {
        Object pipeline = ref.ctxPipeline(wrapperCtx);
        // A generated name: Netty 4.0's addLast throws on a null name (4.1 would auto-generate).
        ref.addLast(pipeline, "vecta-init-" + childCounter.incrementAndGet(), original);
        Sniffer sniffer = new Sniffer();
        sniffer.self = ref.newHandler(sniffer);
        ref.addFirst(pipeline, "vecta-sniff", sniffer.self);
    }

    /** Reads the handshake and login start (always plaintext) to learn the protocol and player. */
    private final class Sniffer implements NettyRef.Inbound {
        Object self;
        private final java.io.ByteArrayOutputStream acc = new java.io.ByteArrayOutputStream();
        private int state; // 0 need handshake, 1 need login start, 2 done
        private int protocol = -1;
        private boolean login;

        @Override
        public boolean channelRead(Object ctx, Object msg) throws Exception {
            if (state != 2) {
                try {
                    acc.write(ref.peek(msg));
                    parse(ctx);
                } catch (Exception e) {
                    Log.debug("commands: handshake sniff failed: " + e);
                    finish(ctx, false);
                }
                if (acc.size() > 8192 && state != 2) finish(ctx, false); // not a normal handshake
            }
            return false; // never consume; always forward
        }

        private void parse(Object ctx) throws Exception {
            byte[] b = acc.toByteArray();
            int off = 0;
            while (state != 2) {
                int[] lp = readVarInt(b, off);
                if (lp == null) return; // incomplete length
                int len = lp[0], idx = lp[1];
                if (len < 0 || len > 4096) {
                    finish(ctx, false);
                    return;
                }
                if (idx + len > b.length) return; // incomplete packet
                ByteArrayInputStream in = new ByteArrayInputStream(b, idx, len);
                if (state == 0) {
                    Mc.readVarInt(in); // packet id 0
                    protocol = Mc.readVarInt(in);
                    Mc.readString(in, 255); // host
                    in.skip(2); // port
                    int next = Mc.readVarInt(in);
                    login = next == 2 || next == 3;
                    state = 1;
                    if (!login) {
                        finish(ctx, false);
                        return;
                    }
                } else { // login start
                    Mc.readVarInt(in); // packet id 0
                    String player = Mc.readString(in, 64);
                    Log.debug("commands: sniffed login start player=" + player + " protocol=" + protocol);
                    finishLogin(ctx, player);
                    return;
                }
                off = idx + len;
            }
        }

        private void finishLogin(Object ctx, String player) throws Exception {
            final Object channel = ref.ctxChannel(ctx);
            final int proto = protocol;
            final String pl = player;
            if (!Packets.known(wireFor(proto))) {
                Log.debug("commands: unknown wire protocol for " + player + " (client " + proto + "); not attaching");
            } else {
                // Attach after login so compression negotiation is done: the decompressor is then
                // already before "decoder", and our handler (added before "decoder") sits after it.
                ref.scheduleLater(channel, new Runnable() {
                    @Override
                    public void run() {
                        attach(channel, proto, pl);
                    }
                }, 1000);
            }
            finish(ctx, true);
        }

        private void finish(Object ctx, boolean ok) {
            state = 2;
            try {
                ref.remove(ref.ctxPipeline(ctx), self);
            } catch (Exception ignored) {
                // already gone
            }
        }

        @Override
        public void channelInactive(Object ctx) {
            state = 2;
        }
    }

    /** Runs on the channel's event loop, once the player is in play, to splice in the interceptor. */
    private void attach(Object channel, int protocol, String player) {
        try {
            if (live.containsKey(channel) || !ref.isActive(channel)) return;
            Object pipeline = ref.pipeline(channel);
            if (!ref.hasHandler(pipeline, "decoder")) {
                Log.debug("commands: no decoder for " + player + "; not attaching");
                return;
            }
            int wire = wireFor(protocol);
            Interceptor h = new Interceptor(channel, protocol, wire, player);
            ref.addBefore(pipeline, "decoder", "vecta-cmd", ref.newHandler(h));
            live.put(channel, new Conn(channel, wire, player));
            Log.debug("commands: attached to " + player + " (client " + protocol + ", wire " + wire + ")");
        } catch (Throwable t) {
            Log.debug("commands: attach failed for " + player + ": " + t);
        }
    }

    /** In PLAY, catches our commands and consumes them; forwards everything else untouched. */
    private final class Interceptor implements NettyRef.Inbound {
        private final Object channel;
        private final int clientProtocol; // from the handshake; for the transfer ticket
        private final int wire;           // server-native; what's on the wire at our pipeline position
        private final String player;
        private final Packets.Ids ids;

        Interceptor(Object channel, int clientProtocol, int wire, String player) {
            this.channel = channel;
            this.clientProtocol = clientProtocol;
            this.wire = wire;
            this.player = player;
            this.ids = Packets.forProtocol(wire);
        }

        @Override
        public boolean channelRead(Object ctx, Object msg) throws Exception {
            String line = commandLine(msg);
            if (line == null) return false;
            String kw = line.split("\\s+", 2)[0].toLowerCase(Locale.ROOT);
            if (!KEYWORDS.contains(kw)) return false;
            ref.release(msg);
            final String cmd = line;
            pool.execute(new Runnable() {
                @Override
                public void run() {
                    handle(cmd);
                }
            });
            return true; // consumed
        }

        private String commandLine(Object msg) {
            try {
                byte[] pkt = ref.peek(msg);
                ByteArrayInputStream in = new ByteArrayInputStream(pkt);
                int id = Mc.readVarInt(in);
                if (ids.inChatCommand >= 0) {
                    if (id == ids.inChatCommand || id == ids.inChatCommandSigned) {
                        return Mc.readString(in, 512); // command, no leading slash
                    }
                } else if (id == ids.inChat) {
                    String s = Mc.readString(in, 512);
                    if (s.startsWith("/")) return s.substring(1);
                }
            } catch (Exception e) {
                Log.debug("commands: parse failed: " + e);
            }
            return null;
        }

        private void handle(String line) {
            String[] parts = line.trim().split("\\s+", 2);
            String cmd = parts[0].toLowerCase(Locale.ROOT);
            String arg = parts.length > 1 ? parts[1].trim() : "";
            try {
                if (cmd.equals("global")) {
                    global(arg);
                } else if (cmd.equals("hub") || cmd.equals("lobby")) {
                    transfer("lobby");
                } else if (cmd.equals("server")) {
                    if (arg.isEmpty()) {
                        say("Usage: /server <id>", "red");
                    } else {
                        transfer(arg.split("\\s+", 2)[0].toLowerCase(Locale.ROOT));
                    }
                }
            } catch (Exception e) {
                Log.debug("commands: " + cmd + " failed: " + e);
                say("Command failed: " + shortMessage(e), "red");
            }
        }

        private void transfer(String target) throws Exception {
            Map<String, Object> tk;
            try {
                tk = gateway.ticket(cfg.serverId, player, clientProtocol, target);
            } catch (Exception e) {
                say("Could not move you: " + shortMessage(e), "red");
                return;
            }
            String mode = Json.str(tk.get("mode"));
            if (mode.equals("transfer")) {
                byte[] cookie = Base64.getDecoder().decode(Json.str(tk.get("cookie")));
                byte[] store = Chat.storeCookie(ids, Json.str(tk.get("cookieKey")), cookie);
                byte[] xfer = Chat.transfer(ids, Json.str(tk.get("host")), Json.integer(tk.get("port"), 25565));
                if (store != null && xfer != null) {
                    ref.sendRaw(channel, store);
                    ref.sendRaw(channel, xfer);
                } else {
                    say("Your client version can't be transferred.", "red");
                }
            } else { // reconnect
                byte[] dc = Chat.disconnect(ids, wire, Json.str(tk.get("message")), null);
                if (dc != null) {
                    ref.sendRaw(channel, dc);
                    ref.close(channel);
                }
            }
        }

        private void global(String message) throws Exception {
            if (message.isEmpty()) {
                say("Usage: /global <message>", "red");
                return;
            }
            if (!ops.isOp(player)) {
                say("You must be a server operator to use /global.", "red");
                return;
            }
            try {
                gateway.postGlobal(cfg.serverId, player, message);
            } catch (Exception e) {
                say("Could not send: " + shortMessage(e), "red");
            }
        }

        private void say(String text, String color) {
            byte[] p = Chat.systemChat(ids, wire, text, color);
            if (p != null) ref.sendRaw(channel, p);
        }

        @Override
        public void channelInactive(Object ctx) {
            live.remove(channel);
        }
    }

    // --- /global broadcast ---

    private void startGlobalPoll() {
        Thread t = new Thread(new Runnable() {
            @Override
            public void run() {
                pollLoop();
            }
        }, "vecta-global");
        t.setDaemon(true);
        t.start();
    }

    private void pollLoop() {
        boolean initialized = false;
        while (true) {
            try {
                if (!initialized) { // learn the current position without replaying history
                    globalSeq = seqOf(gateway.pollGlobalInit());
                    initialized = true;
                    continue;
                }
                Map<String, Object> resp = gateway.pollGlobal(globalSeq);
                for (Object o : Json.list(resp.get("messages"))) {
                    Map<String, Object> m = Json.obj(o);
                    broadcast(Json.str(m.get("server")), Json.str(m.get("player")), Json.str(m.get("message")));
                }
                globalSeq = Math.max(globalSeq, seqOf(resp));
            } catch (Exception e) {
                Log.debug("commands: global poll failed: " + e);
                sleep(5000); // back off on error, then resume long-polling
            }
        }
    }

    private long seqOf(Map<String, Object> resp) {
        Object s = resp.get("seq");
        return s instanceof Number ? ((Number) s).longValue() : globalSeq;
    }

    private void broadcast(String server, String player, String message) {
        String text = "[G] " + player + " » " + message;
        long now = System.currentTimeMillis();
        for (Conn c : live.values()) {
            if (now < c.readyAt || !ref.isActive(c.channel)) continue;
            Packets.Ids ids = Packets.forProtocol(c.protocol);
            if (ids == null) continue;
            byte[] p = Chat.systemChat(ids, c.protocol, text, COLOR);
            if (p != null) ref.sendRaw(c.channel, p);
        }
    }

    private static String shortMessage(Throwable t) {
        String m = t.getMessage();
        if (m == null) return t.getClass().getSimpleName();
        int nl = m.indexOf('\n');
        if (nl > 0) m = m.substring(0, nl);
        return m.length() > 120 ? m.substring(0, 120) : m;
    }

    private static void sleep(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    private static int[] readVarInt(byte[] b, int off) {
        int value = 0;
        for (int k = 0; k < 5; k++) {
            if (off + k >= b.length) return null; // incomplete
            int x = b[off + k] & 0xff;
            value |= (x & 0x7f) << (7 * k);
            if ((x & 0x80) == 0) return new int[] {value, off + k + 1};
        }
        return new int[] {-1, off}; // malformed
    }

    private static ThreadFactory daemon(final String name) {
        return new ThreadFactory() {
            @Override
            public Thread newThread(Runnable r) {
                Thread t = new Thread(r, name);
                t.setDaemon(true);
                return t;
            }
        };
    }
}
