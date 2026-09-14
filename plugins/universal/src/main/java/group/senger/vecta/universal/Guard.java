package group.senger.vecta.universal;

import java.io.BufferedInputStream;
import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.Closeable;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.security.GeneralSecurityException;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Semaphore;
import java.util.concurrent.ThreadFactory;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Listens on the public port and forwards only connections that carry the gateway's signed header
 * to the real server. Direct joins get a disconnect message pointing to the gateway.
 */
final class Guard implements Closeable {
    private static final int MAX_CONNECTIONS = 1024;
    private static final long NONCE_TTL_MS = 2 * GuardCodec.MAX_SKEW_SECONDS * 1000;

    private final Config cfg;
    private final byte[] key;
    private final ConcurrentHashMap<String, Long> nonces = new ConcurrentHashMap<String, Long>();
    private final Semaphore slots = new Semaphore(MAX_CONNECTIONS);
    private final ExecutorService pool = Executors.newCachedThreadPool(daemon("vecta-guard-conn"));
    private final AtomicLong lastWarn = new AtomicLong();
    private volatile long lastPrune;
    private ServerSocket server;

    Guard(Config cfg) {
        this.cfg = cfg;
        this.key = cfg.token.isEmpty() ? null : GuardCodec.key(cfg.token);
    }

    /** Binds the listener and returns the bound port. */
    int start() throws IOException {
        HostPort listen = HostPort.parse(cfg.guardListen.isEmpty() ? "0.0.0.0:25565" : cfg.guardListen, 25565);
        HostPort backend = HostPort.parse(cfg.guardBackendAddress(), 25565);
        if (cfg.guardBackend.isEmpty() && listen.port != 0 && listen.port == cfg.serverPort) {
            throw new IOException("guardListen uses the server's own port " + cfg.serverPort
                    + ": set server-port in server.properties to another port (e.g. 25566) and server-ip=127.0.0.1");
        }
        if (key == null) Log.warn("guard: token is not set, so every connection is refused");
        server = new ServerSocket();
        server.setReuseAddress(true);
        server.bind(new InetSocketAddress(listen.host.isEmpty() ? "0.0.0.0" : listen.host, listen.port), 256);
        Thread t = new Thread(new Runnable() {
            @Override
            public void run() {
                acceptLoop();
            }
        }, "vecta-guard");
        t.setDaemon(true);
        t.start();
        Log.info("guard listening on " + listen.host + ":" + server.getLocalPort()
                + ", forwarding gateway connections to " + backend);
        return server.getLocalPort();
    }

    @Override
    public void close() throws IOException {
        if (server != null) server.close();
        pool.shutdownNow();
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

    private void acceptLoop() {
        while (!server.isClosed()) {
            final Socket s;
            try {
                s = server.accept();
            } catch (IOException e) {
                if (server.isClosed()) return;
                Log.warn("guard: accept failed: " + e);
                sleep(100);
                continue;
            }
            if (!slots.tryAcquire()) {
                closeQuietly(s);
                continue;
            }
            try {
                pool.execute(new Runnable() {
                    @Override
                    public void run() {
                        try {
                            handle(s);
                        } finally {
                            slots.release();
                            closeQuietly(s);
                        }
                    }
                });
            } catch (RuntimeException e) {
                slots.release();
                closeQuietly(s);
            }
        }
    }

    private void handle(Socket client) {
        Socket backend = null;
        try {
            client.setSoTimeout(5000);
            client.setTcpNoDelay(true);
            final BufferedInputStream in = new BufferedInputStream(client.getInputStream(), 16 * 1024);
            in.mark(16);
            byte[] head = new byte[12];
            int n = 0;
            while (n < 12) {
                int r = in.read(head, n, 12 - n);
                if (r < 0) break;
                n += r;
            }
            in.reset();
            if (n < 12 || !GuardCodec.hasSignature(head)) {
                refuse(client, in);
                return;
            }
            byte[] header = GuardCodec.readHeader(in);
            if (key == null) return;
            GuardCodec.Result res;
            try {
                res = GuardCodec.verify(key, header, System.currentTimeMillis() / 1000L, new GuardCodec.NonceCheck() {
                    @Override
                    public boolean seen(byte[] nonce) {
                        return nonceSeen(nonce);
                    }
                });
            } catch (GeneralSecurityException e) {
                warnLimited("guard: refused " + client.getRemoteSocketAddress() + ": " + e.getMessage());
                return;
            } catch (IllegalArgumentException e) {
                warnLimited("guard: refused " + client.getRemoteSocketAddress() + ": " + e.getMessage());
                return;
            }

            HostPort b = HostPort.parse(cfg.guardBackendAddress(), 25565);
            backend = new Socket();
            backend.connect(new InetSocketAddress(b.host, b.port), 5000);
            backend.setTcpNoDelay(true);
            final OutputStream toBackend = backend.getOutputStream();
            if (cfg.proxyProtocol) toBackend.write(GuardCodec.plainHeader(res.src, res.dst));
            client.setSoTimeout(0);

            final Socket backendRef = backend;
            final Socket clientRef = client;
            pool.execute(new Runnable() {
                @Override
                public void run() {
                    copy(in, toBackend);
                    closeQuietly(backendRef);
                    closeQuietly(clientRef);
                }
            });
            copy(backend.getInputStream(), client.getOutputStream());
        } catch (IOException e) {
            Log.debug("guard: connection from " + client.getRemoteSocketAddress() + " ended: " + e);
        } finally {
            closeQuietly(backend);
        }
    }

    private static void copy(InputStream in, OutputStream out) {
        byte[] buf = new byte[16 * 1024];
        try {
            for (int n; (n = in.read(buf)) > 0; ) {
                out.write(buf, 0, n);
                out.flush();
            }
        } catch (IOException ignored) {
            // the other side closed
        }
    }

    /** Answers a direct (unsigned) connection with a message that points players to the gateway. */
    private void refuse(Socket client, InputStream in) throws IOException {
        int len = Mc.readVarInt(in);
        if (len <= 0 || len > 1024) return; // not a modern handshake (e.g. legacy ping)
        byte[] frame = new byte[len];
        Mc.readFully(in, frame);
        ByteArrayInputStream p = new ByteArrayInputStream(frame);
        if (Mc.readVarInt(p) != 0x00) return;
        int protocol = Mc.readVarInt(p);
        Mc.readString(p, 1024);
        if (p.skip(2) != 2) return;
        int next = Mc.readVarInt(p);

        String message = cfg.guardJoinHint.isEmpty()
                ? "This server can only be joined through its gateway."
                : "Join this server through " + cfg.guardJoinHint;
        OutputStream out = client.getOutputStream();
        if (next == 1) {
            Mc.readFrame(in, 16); // status request
            Map<String, Object> version = new LinkedHashMap<String, Object>();
            version.put("name", "vecta guard");
            version.put("protocol", protocol);
            Map<String, Object> players = new LinkedHashMap<String, Object>();
            players.put("max", 0);
            players.put("online", 0);
            Map<String, Object> status = new LinkedHashMap<String, Object>();
            status.put("version", version);
            status.put("players", players);
            status.put("description", text(message));
            ByteArrayOutputStream resp = new ByteArrayOutputStream();
            Mc.writeVarInt(resp, 0x00);
            Mc.writeString(resp, Json.write(status));
            out.write(Mc.frame(resp.toByteArray()));
            out.flush();
            try {
                out.write(Mc.frame(Mc.readFrame(in, 64))); // echo ping as pong
            } catch (IOException ignored) {
                // client closed without pinging
            }
        } else if (next == 2 || next == 3) {
            ByteArrayOutputStream resp = new ByteArrayOutputStream();
            Mc.writeVarInt(resp, 0x00); // login disconnect
            Mc.writeString(resp, Json.write(text(message)));
            out.write(Mc.frame(resp.toByteArray()));
            Log.debug("guard: refused direct join from " + client.getRemoteSocketAddress());
        }
        out.flush();
    }

    private static Map<String, Object> text(String message) {
        Map<String, Object> t = new LinkedHashMap<String, Object>();
        t.put("text", message);
        t.put("color", "red");
        return t;
    }

    private boolean nonceSeen(byte[] nonce) {
        final long now = System.currentTimeMillis();
        if (now - lastPrune > 60_000 || nonces.size() > 100_000) {
            lastPrune = now;
            for (Map.Entry<String, Long> e : nonces.entrySet()) {
                if (now - e.getValue() > NONCE_TTL_MS) nonces.remove(e.getKey(), e.getValue());
            }
        }
        StringBuilder hex = new StringBuilder(nonce.length * 2);
        for (byte x : nonce) hex.append(String.format("%02x", x & 0xff));
        return nonces.putIfAbsent(hex.toString(), now) != null;
    }

    private void warnLimited(String msg) {
        long now = System.currentTimeMillis();
        long last = lastWarn.get();
        if (now - last > 10_000 && lastWarn.compareAndSet(last, now)) {
            Log.warn(msg);
        } else {
            Log.debug(msg);
        }
    }

    static boolean isLoopback(String host) {
        try {
            return InetAddress.getByName(host).isLoopbackAddress();
        } catch (IOException e) {
            return false;
        }
    }

    private static void sleep(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    static void closeQuietly(Closeable c) {
        if (c == null) return;
        try {
            c.close();
        } catch (IOException ignored) {
            // nothing to do
        }
    }
}
