package group.senger.anymcp.universal;

import java.lang.instrument.Instrumentation;

/**
 * Java agent entry points. premain runs for -javaagent:anymcp.jar (use this where the server is
 * started by a script, e.g. Forge/NeoForge run.sh). agentmain runs via Launcher-Agent-Class when the
 * jar is started with java -jar on Java 9+, and only hands over instrumentation to the wrapper.
 */
public final class Agent {
    private Agent() {
    }

    public static void premain(String args, Instrumentation inst) {
        Anymcp.setInstrumentation(inst);
        try {
            Anymcp.start(Config.load(args), "agent");
        } catch (Throwable t) {
            Log.warn("anymcp failed to start; the server continues without it", t);
        }
    }

    public static void agentmain(String args, Instrumentation inst) {
        Anymcp.setInstrumentation(inst);
    }
}
