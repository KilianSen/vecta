package group.senger.vecta.universal;

import java.lang.instrument.Instrumentation;
import java.util.concurrent.atomic.AtomicBoolean;

/** Starts the guard and heartbeat once, whichever entry point (wrapper or agent) runs first. */
final class Vecta {
    private static final AtomicBoolean STARTED = new AtomicBoolean();
    private static volatile Instrumentation instrumentation;

    private Vecta() {
    }

    static void setInstrumentation(Instrumentation inst) {
        if (inst != null) instrumentation = inst;
    }

    static Instrumentation instrumentation() {
        return instrumentation;
    }

    static String version() {
        String v = Vecta.class.getPackage() == null ? null : Vecta.class.getPackage().getImplementationVersion();
        return v == null ? "dev" : v;
    }

    static void start(Config cfg, String mode) {
        if (!STARTED.compareAndSet(false, true)) return;
        Log.debug = cfg.debug;
        Log.info("vecta " + version() + " (" + mode + " mode, config " + cfg.file + ")");
        if (cfg.guard) {
            try {
                new Guard(cfg).start();
            } catch (Throwable t) {
                Log.warn("guard failed to start, so the gateway cannot reach this server", t);
            }
        }
        if (cfg.commands) startCommands(cfg);
        String problem = cfg.registrationProblem();
        if (problem != null) {
            Log.warn("not registering: " + problem + " (edit " + cfg.file + ")");
            return;
        }
        Thread t = new Thread(new Heartbeat(cfg), "vecta-heartbeat");
        t.setDaemon(true);
        t.start();
    }

    /**
     * Waits for the server's Netty to bind in this JVM, then installs the /hub, /server and /global
     * handlers. If the server comes up (answers a local ping) but never appears in this JVM's Netty,
     * it is running in a separate process — a launcher started it, not us — or on an unrecognized
     * framework; warn once and stop, since the in-JVM hook can't reach it.
     */
    private static void startCommands(final Config cfg) {
        Thread t = new Thread(new Runnable() {
            @Override
            public void run() {
                long serverUpAt = 0;
                for (int i = 0; i < 150; i++) { // up to ~5 minutes for the server to bind
                    if (!Netty.serverChannels().isEmpty()) {
                        if (Commands.start(cfg) != null) return;
                    } else {
                        if (serverUpAt == 0 && i % 5 == 0 && serverAnswers(cfg)) {
                            serverUpAt = System.currentTimeMillis();
                        }
                        // For an in-JVM server the ping only succeeds once Netty is listening, so a
                        // successful ping with no server channel here means it is not in this JVM.
                        if (serverUpAt != 0 && System.currentTimeMillis() - serverUpAt > 20_000) {
                            Log.warn("commands could not attach: the server is running but its network is not "
                                    + "reachable from this JVM (it was likely started in a separate process by a "
                                    + "launcher, or runs an unrecognized framework). /hub, /server and /global are off. "
                                    + "To enable them, start the server itself with -javaagent:vecta.jar.");
                            return;
                        }
                        if (i % 10 == 5) {
                            Log.debug("commands: waiting for server listener (" + Netty.counts() + ")");
                            if (Log.debug) Netty.dumpLoops();
                        }
                    }
                    try {
                        Thread.sleep(2000);
                    } catch (InterruptedException e) {
                        return;
                    }
                }
                Log.debug("commands: gave up waiting for the server's network listener");
            }
        }, "vecta-commands-init");
        t.setDaemon(true);
        t.start();
    }

    /** Whether the local server answers a status ping (it is up, in some process). */
    private static boolean serverAnswers(Config cfg) {
        try {
            StatusPing.ping(cfg.localAddress(), cfg.proxyProtocol, 2000);
            return true;
        } catch (Exception e) {
            return false;
        }
    }
}
