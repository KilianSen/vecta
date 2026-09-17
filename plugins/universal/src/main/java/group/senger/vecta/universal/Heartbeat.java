package group.senger.vecta.universal;

import java.io.IOException;
import java.util.List;
import java.util.Map;

/** Waits for the local server, then keeps its registration at the gateway alive. */
final class Heartbeat implements Runnable {
    private static final long RESCAN_MS = 10 * 60 * 1000;
    private static final long REPROBE_MS = 5 * 60 * 1000;

    private final Config cfg;
    private final GatewayClient gateway;
    private final SidePorts sidePorts;
    private volatile boolean registered;
    private ModScanner.Result scan;
    private long scannedAt;
    private RuntimeProbe.Result runtime;
    private long probedAt;
    private String lastSummary = "";

    Heartbeat(Config cfg, SidePorts sidePorts) {
        this.cfg = cfg;
        this.gateway = new GatewayClient(cfg.gateway, cfg.token);
        this.sidePorts = sidePorts;
    }

    /**
     * Registers once before the server starts, so a side port hook can configure mods before they
     * read their config. The server shows as offline until its health check passes.
     */
    void registerEarly() {
        long start = System.currentTimeMillis();
        try {
            Map<String, Object> body = Report.build(cfg, ModScanner.scan(cfg.serverDir), null, null);
            body.put("sidePorts", sidePorts.report());
            sidePorts.apply(gateway.register(cfg.serverId, Json.write(body)), false);
            Log.debug("early registration took " + (System.currentTimeMillis() - start) + " ms");
        } catch (IOException e) {
            Log.warn("early registration for side ports failed (" + e.getMessage() + "); retrying once the server is up");
        } catch (RuntimeException e) {
            Log.warn("early registration for side ports failed", e);
        }
    }

    @Override
    public void run() {
        Runtime.getRuntime().addShutdownHook(new Thread(new Runnable() {
            @Override
            public void run() {
                unregister();
            }
        }, "vecta-unregister"));
        boolean waitLogged = false;
        while (true) {
            StatusPing.Status ping;
            try {
                ping = StatusPing.ping(cfg.localAddress(), cfg.proxyProtocol, 3000);
                // Forge answers pings while still starting, without a real version: not ready yet.
                if (ping.protocol <= 0) throw new IOException("server is still starting");
            } catch (IOException e) {
                ping = null;
                if (registered) {
                    Log.warn("server at " + cfg.localAddress() + " stopped answering (" + e.getMessage() + "); pausing heartbeats");
                    registered = false;
                } else if (!waitLogged) {
                    Log.info("waiting for the server at " + cfg.localAddress() + " before registering");
                    waitLogged = true;
                }
            }
            if (ping == null) {
                sleep(registered ? cfg.heartbeatSeconds * 1000L : 2000L);
                continue;
            }
            try {
                beat(ping);
            } catch (IOException e) {
                Log.warn("registration failed: " + e.getMessage());
                registered = false;
            } catch (Throwable t) {
                Log.warn("heartbeat failed", t);
                registered = false;
            }
            sleep(cfg.heartbeatSeconds * 1000L);
        }
    }

    private void beat(StatusPing.Status ping) throws IOException {
        long now = System.currentTimeMillis();
        if (scan == null || now - scannedAt > RESCAN_MS) {
            scan = ModScanner.scan(cfg.serverDir);
            scannedAt = now;
            Log.debug("file scan: " + scan.jars + " jars, " + scan.mods.size() + " mods/plugins, "
                    + scan.nestedIds.size() + " bundled");
        }
        if (cfg.runtimeChecks && (runtime == null || now - probedAt > REPROBE_MS)) {
            runtime = RuntimeProbe.probe();
            probedAt = now;
        }
        Map<String, Object> body = Report.build(cfg, scan, runtime, ping);
        if (sidePorts.active()) body.put("sidePorts", sidePorts.report());
        String saved = gateway.register(cfg.serverId, Json.write(body));
        if (sidePorts.active()) sidePorts.apply(saved, true);

        String summary = "loader " + body.get("loader") + ", " + ((List<?>) body.get("mods")).size() + " client mods, "
                + ((List<?>) body.get("channels")).size() + " channels, protocols "
                + body.get("minProtocol") + "-" + body.get("maxProtocol")
                + (runtime == null ? " (file scan only)" : " (runtime: " + runtime.summary() + ")");
        if (!registered) {
            Log.info("registered " + cfg.serverId + " at " + cfg.gateway + ": " + summary);
            registered = true;
        } else if (!summary.equals(lastSummary)) {
            Log.info("registration updated: " + summary);
        }
        lastSummary = summary;
    }

    void unregister() {
        if (!registered) return;
        registered = false;
        try {
            gateway.unregister(cfg.serverId);
            Log.info("unregistered " + cfg.serverId);
        } catch (IOException e) {
            Log.warn("unregister failed: " + e.getMessage());
        }
    }

    private static void sleep(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }
}
