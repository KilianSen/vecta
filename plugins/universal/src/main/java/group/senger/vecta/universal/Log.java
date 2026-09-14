package group.senger.vecta.universal;

/** Console logging. Server software captures stdout into its own log. */
final class Log {
    static volatile boolean debug;

    private Log() {
    }

    static void info(String msg) {
        System.out.println("[vecta] " + msg);
    }

    static void warn(String msg) {
        System.out.println("[vecta] WARN " + msg);
    }

    static void warn(String msg, Throwable t) {
        warn(msg + ": " + t);
        if (debug) t.printStackTrace(System.out);
    }

    static void debug(String msg) {
        if (debug) System.out.println("[vecta] DEBUG " + msg);
    }
}
