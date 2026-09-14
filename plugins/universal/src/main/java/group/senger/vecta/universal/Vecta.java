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

    /** Waits for the server's Netty to bind, then installs the /hub, /server and /global handlers. */
    private static void startCommands(final Config cfg) {
        Thread t = new Thread(new Runnable() {
            @Override
            public void run() {
                for (int i = 0; i < 150; i++) { // up to ~5 minutes for the server to bind
                    if (!Netty.serverChannels().isEmpty()) {
                        if (Commands.start(cfg) != null) return;
                    } else if (i % 10 == 5) {
                        Log.debug("commands: waiting for server listener (" + Netty.counts() + ")");
                        if (Log.debug) Netty.dumpLoops();
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
}
