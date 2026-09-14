package group.senger.anymcp.universal;

/** Console logging. Server software captures stdout into its own log. */
final class Log {
    static volatile boolean debug;

    private Log() {
    }

    static void info(String msg) {
        System.out.println("[anymcp] " + msg);
    }

    static void warn(String msg) {
        System.out.println("[anymcp] WARN " + msg);
    }

    static void warn(String msg, Throwable t) {
        warn(msg + ": " + t);
        if (debug) t.printStackTrace(System.out);
    }

    static void debug(String msg) {
        if (debug) System.out.println("[anymcp] DEBUG " + msg);
    }
}
