package group.senger.vecta.universal;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Iterator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.TreeMap;
import java.util.concurrent.TimeUnit;
import java.util.regex.Pattern;

/**
 * Side ports (docs/side-ports.md): extra ports of this server that the gateway exposes on public
 * ports it assigns. Declares them in the registration and runs the owner's hook whenever an
 * assignment changes, so the hook can point the mod (e.g. Simple Voice Chat's voice_host) at the
 * public address. The last applied state is kept in {@value #STATE_FILE}.
 */
final class SidePorts {
    static final String STATE_FILE = ".vecta-sideports.json";
    static final long HOOK_TIMEOUT_MS = 30000;
    private static final Pattern NAME = Pattern.compile("[a-z0-9][a-z0-9-]{0,31}");

    static final class Decl {
        final String name;
        final String protocol;
        final int port;

        Decl(String name, String protocol, int port) {
            this.name = name;
            this.protocol = protocol;
            this.port = port;
        }
    }

    /** What was last applied for one side port. Mirrors the Go agent's state file. */
    static final class State {
        String state = "";
        String protocol = "";
        int backendPort;
        String host = "";
        int port;
        String error = "";

        boolean same(State o) {
            return state.equals(o.state) && protocol.equals(o.protocol) && backendPort == o.backendPort
                    && host.equals(o.host) && port == o.port && error.equals(o.error);
        }

        Map<String, Object> toJson() {
            Map<String, Object> m = new LinkedHashMap<String, Object>();
            m.put("state", state);
            m.put("protocol", protocol);
            m.put("backendPort", backendPort);
            if (!host.isEmpty()) m.put("host", host);
            if (port != 0) m.put("port", port);
            if (!error.isEmpty()) m.put("error", error);
            return m;
        }

        static State fromJson(Object o) {
            Map<String, Object> m = Json.obj(o);
            State s = new State();
            s.state = Json.str(m.get("state"));
            s.protocol = Json.str(m.get("protocol"));
            s.backendPort = Json.integer(m.get("backendPort"), 0);
            s.host = Json.str(m.get("host"));
            s.port = Json.integer(m.get("port"), 0);
            s.error = Json.str(m.get("error"));
            return s;
        }
    }

    final List<Decl> declared;
    private final List<String> hook;
    private final File stateFile;
    private final File workDir;
    private final String serverId;
    final Map<String, State> applied = new TreeMap<String, State>();

    SidePorts(Config cfg) {
        this(parse(cfg.sidePorts), splitHook(cfg.sidePortHook), new File(cfg.serverDir, STATE_FILE), cfg.serverDir, cfg.serverId);
    }

    SidePorts(List<Decl> declared, List<String> hook, File stateFile, File workDir, String serverId) {
        this.declared = declared;
        this.hook = hook;
        this.stateFile = stateFile;
        this.workDir = workDir;
        this.serverId = serverId;
        load();
    }

    /** Whether there is anything to declare or clean up. */
    boolean active() {
        return !declared.isEmpty() || !applied.isEmpty();
    }

    /** Parses {@code name:protocol:port,...}. */
    static List<Decl> parse(String v) {
        List<Decl> out = new ArrayList<Decl>();
        for (String part : v.split(",")) {
            part = part.trim().toLowerCase(Locale.ROOT);
            if (part.isEmpty()) continue;
            String[] f = part.split(":");
            if (f.length != 3) throw new IllegalArgumentException("side port \"" + part + "\" must be name:protocol:port");
            String name = f[0].trim();
            String protocol = f[1].trim();
            int port = Integer.parseInt(f[2].trim());
            if (!NAME.matcher(name).matches()) throw new IllegalArgumentException("bad side port name " + name);
            if (!protocol.equals("tcp") && !protocol.equals("udp")) throw new IllegalArgumentException("protocol must be tcp or udp");
            if (port < 1 || port > 65535) throw new IllegalArgumentException("port out of range");
            for (Decl d : out) {
                if (d.name.equals(name)) throw new IllegalArgumentException("duplicate side port " + name);
            }
            out.add(new Decl(name, protocol, port));
        }
        return out;
    }

    /** The hook command, split on whitespace (no shell). */
    static List<String> splitHook(String v) {
        List<String> out = new ArrayList<String>();
        for (String part : v.trim().split("\\s+")) {
            if (!part.isEmpty()) out.add(part);
        }
        return out;
    }

    /** The registration's sidePorts value, with the last assigned port as preference. */
    List<Map<String, Object>> report() {
        List<Map<String, Object>> out = new ArrayList<Map<String, Object>>();
        for (Decl d : declared) {
            Map<String, Object> m = new LinkedHashMap<String, Object>();
            m.put("name", d.name);
            m.put("protocol", d.protocol);
            m.put("port", d.port);
            State st = applied.get(d.name);
            if (st != null && st.state.equals("assigned")) m.put("preferredPort", st.port);
            out.add(m);
        }
        return out;
    }

    /**
     * Compares the gateway's registration response with what was applied and runs the hook for each
     * change. started tells the hook whether the Minecraft server is already running.
     */
    void apply(String response, boolean started) {
        Map<String, Map<String, Object>> got = new TreeMap<String, Map<String, Object>>();
        boolean supported = false;
        try {
            Map<String, Object> saved = Json.obj(Json.parse(response));
            supported = saved.containsKey("sidePorts");
            for (Object o : Json.list(saved.get("sidePorts"))) {
                Map<String, Object> p = Json.obj(o);
                got.put(Json.str(p.get("name")), p);
            }
        } catch (RuntimeException e) {
            Log.warn("unreadable registration response: " + e.getMessage());
            return;
        }
        boolean changed = false;
        for (Decl d : declared) {
            State want = new State();
            want.protocol = d.protocol;
            want.backendPort = d.port;
            Map<String, Object> p = got.get(d.name);
            Map<String, Object> pub = p == null ? null : Json.obj(p.get("public"));
            if (pub != null && !pub.isEmpty()) {
                want.state = "assigned";
                want.host = Json.str(pub.get("host"));
                want.port = Json.integer(pub.get("port"), 0);
            } else {
                want.state = "error";
                want.error = p != null ? Json.str(p.get("error"))
                        : supported ? "missing from gateway response" : "gateway does not support side ports";
            }
            State have = applied.get(d.name);
            if (have != null && have.same(want)) continue;
            if (want.state.equals("assigned")) {
                Log.info("side port " + d.name + " (" + d.protocol + " " + d.port + ") is public at " + want.host + ":" + want.port);
            } else {
                Log.warn("side port " + d.name + " not assigned: " + want.error);
            }
            if (runHook(d.name, want, started)) {
                applied.put(d.name, want);
                changed = true;
            }
        }
        for (Iterator<Map.Entry<String, State>> it = applied.entrySet().iterator(); it.hasNext(); ) {
            Map.Entry<String, State> e = it.next();
            if (isDeclared(e.getKey())) continue;
            State rel = new State();
            rel.state = "released";
            rel.protocol = e.getValue().protocol;
            rel.backendPort = e.getValue().backendPort;
            if (runHook(e.getKey(), rel, started)) {
                it.remove();
                changed = true;
            }
        }
        if (changed) save();
    }

    private boolean isDeclared(String name) {
        for (Decl d : declared) {
            if (d.name.equals(name)) return true;
        }
        return false;
    }

    /** Environment for the hook; the same variables as the Go agent plus VECTA_SERVER_STARTED. */
    Map<String, String> env(String name, State st, boolean started) {
        Map<String, String> env = new LinkedHashMap<String, String>();
        env.put("VECTA_SERVER_ID", serverId);
        env.put("VECTA_SIDEPORT_NAME", name);
        env.put("VECTA_SIDEPORT_PROTOCOL", st.protocol);
        env.put("VECTA_SIDEPORT_BACKEND_PORT", String.valueOf(st.backendPort));
        env.put("VECTA_SIDEPORT_STATE", st.state);
        env.put("VECTA_SIDEPORT_HOST", st.host);
        env.put("VECTA_SIDEPORT_PORT", st.port == 0 ? "" : String.valueOf(st.port));
        env.put("VECTA_SIDEPORT_ERROR", st.error);
        env.put("VECTA_SERVER_STARTED", started ? "1" : "0");
        return env;
    }

    /** Runs the hook; returns whether the state counts as applied. */
    private boolean runHook(String name, State st, boolean started) {
        if (hook.isEmpty()) return true;
        List<String> cmd = new ArrayList<String>(hook);
        String exe = cmd.get(0);
        // A relative path is relative to the server directory; a bare name is looked up in PATH.
        if (!new File(exe).isAbsolute() && (exe.indexOf('/') >= 0 || exe.indexOf(File.separatorChar) >= 0)) {
            cmd.set(0, new File(workDir, exe).getPath());
        }
        ProcessBuilder pb = new ProcessBuilder(cmd).directory(workDir).redirectErrorStream(true);
        pb.environment().putAll(env(name, st, started));
        try {
            final Process p = pb.start();
            p.getOutputStream().close();
            final ByteArrayOutputStream out = new ByteArrayOutputStream();
            Thread reader = new Thread(new Runnable() {
                @Override
                public void run() {
                    try {
                        InputStream in = p.getInputStream();
                        byte[] buf = new byte[4096];
                        int n;
                        while ((n = in.read(buf)) > 0) {
                            synchronized (out) {
                                if (out.size() < 16 * 1024) out.write(buf, 0, n);
                            }
                        }
                    } catch (IOException ignored) {
                    }
                }
            }, "vecta-sideport-hook");
            reader.setDaemon(true);
            reader.start();
            if (!p.waitFor(HOOK_TIMEOUT_MS, TimeUnit.MILLISECONDS)) {
                p.destroyForcibly();
                Log.warn("side port hook for " + name + " timed out; retrying next heartbeat");
                return false;
            }
            reader.join(1000);
            String text;
            synchronized (out) {
                text = new String(out.toByteArray(), StandardCharsets.UTF_8).trim();
            }
            if (!text.isEmpty()) Log.info("side port hook (" + name + "): " + text);
            if (p.exitValue() != 0) {
                Log.warn("side port hook for " + name + " exited with " + p.exitValue() + "; retrying next heartbeat");
                return false;
            }
            return true;
        } catch (IOException e) {
            Log.warn("side port hook for " + name + " failed: " + e.getMessage());
            return false;
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            return false;
        }
    }

    private void load() {
        if (!stateFile.isFile()) return;
        try {
            InputStream in = new FileInputStream(stateFile);
            try {
                String text = new String(ModScanner.readAll(in, 1 << 20), StandardCharsets.UTF_8);
                for (Map.Entry<String, Object> e : Json.obj(Json.parse(text)).entrySet()) {
                    applied.put(e.getKey(), State.fromJson(e.getValue()));
                }
            } finally {
                in.close();
            }
        } catch (IOException e) {
            Log.warn("could not read " + stateFile + ": " + e);
        } catch (RuntimeException e) {
            Log.warn("ignoring invalid " + stateFile + ": " + e.getMessage());
            applied.clear();
        }
    }

    private void save() {
        Map<String, Object> m = new LinkedHashMap<String, Object>();
        for (Map.Entry<String, State> e : applied.entrySet()) m.put(e.getKey(), e.getValue().toJson());
        File tmp = new File(stateFile.getPath() + ".tmp");
        try {
            OutputStream out = new FileOutputStream(tmp);
            try {
                out.write(Json.write(m).getBytes(StandardCharsets.UTF_8));
            } finally {
                out.close();
            }
            if (!tmp.renameTo(stateFile)) {
                // Windows cannot rename over an existing file.
                if (!stateFile.delete() || !tmp.renameTo(stateFile)) throw new IOException("rename failed");
            }
        } catch (IOException e) {
            Log.warn("could not write " + stateFile + ": " + e);
        }
    }
}
