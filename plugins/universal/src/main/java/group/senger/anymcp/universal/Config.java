package group.senger.anymcp.universal;

import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Properties;
import java.util.TreeSet;

/**
 * Settings from anymcp.properties, overridable with ANYMCP_* environment variables, -Danymcp.*
 * system properties and agent arguments (key=value;key=value), in that order of precedence.
 */
final class Config {
    static final String FILE_NAME = "anymcp.properties";
    static final List<String> KEYS = Arrays.asList(
            "gateway", "token", "serverId", "name", "description", "address", "hidden", "heartbeatSeconds",
            "loader", "protocols", "requiredClientMods", "proxyProtocol",
            "guard", "guardListen", "guardBackend", "guardJoinHint",
            "commands", "runtimeChecks", "serverJar", "debug");

    File serverDir = new File(System.getProperty("user.dir"));
    File file = new File(serverDir, FILE_NAME);

    String gateway = "";
    String token = "";
    String serverId = "";
    String name = "";
    String description = "";
    String address = "";
    boolean hidden;
    int heartbeatSeconds = 15;
    String loader = "";
    String protocols = "";
    List<String> requiredClientMods = new ArrayList<String>();
    boolean proxyProtocol;
    boolean guard;
    String guardListen = "";
    String guardBackend = "";
    String guardJoinHint = "";
    boolean commands = true;
    boolean runtimeChecks = true;
    String serverJar = "";
    boolean debug;

    /** From server.properties. */
    int serverPort = 25565;
    String serverIp = "";

    static Config load(String agentArgs) {
        Config c = new Config();
        Map<String, String> args = parseArgs(agentArgs);
        String path = first(args.get("config"), System.getProperty("anymcp.config"), System.getenv("ANYMCP_CONFIG"), FILE_NAME);
        c.file = new File(path).isAbsolute() ? new File(path) : new File(c.serverDir, path);

        Properties p = new Properties();
        if (c.file.isFile()) {
            try {
                InputStream in = new FileInputStream(c.file);
                try {
                    p.load(new InputStreamReader(in, StandardCharsets.UTF_8));
                } finally {
                    in.close();
                }
            } catch (IOException e) {
                Log.warn("could not read " + c.file + ": " + e);
            }
        } else {
            writeTemplate(c.file);
        }
        for (String key : KEYS) {
            String v = p.getProperty(key);
            String env = System.getenv("ANYMCP_" + snake(key));
            if (env != null) v = env;
            String sys = System.getProperty("anymcp." + key);
            if (sys != null) v = sys;
            if (args.containsKey(key)) v = args.get(key);
            if (v != null) {
                try {
                    c.set(key, v.trim());
                } catch (RuntimeException e) {
                    Log.warn("ignoring invalid " + key + "=" + v + ": " + e.getMessage());
                }
            }
        }
        c.readServerProperties();
        return c;
    }

    void set(String key, String v) {
        if (key.equals("gateway")) gateway = v.replaceAll("/+$", "");
        else if (key.equals("token")) token = v;
        else if (key.equals("serverId")) serverId = v.toLowerCase(Locale.ROOT);
        else if (key.equals("name")) name = v;
        else if (key.equals("description")) description = v;
        else if (key.equals("address")) address = v;
        else if (key.equals("hidden")) hidden = Boolean.parseBoolean(v);
        else if (key.equals("heartbeatSeconds")) heartbeatSeconds = Math.max(5, Integer.parseInt(v));
        else if (key.equals("loader")) loader = v.toLowerCase(Locale.ROOT);
        else if (key.equals("protocols")) {
            if (!v.isEmpty()) parseProtocols(v); // validate early
            protocols = v;
        } else if (key.equals("requiredClientMods")) requiredClientMods = splitList(v);
        else if (key.equals("proxyProtocol")) proxyProtocol = Boolean.parseBoolean(v);
        else if (key.equals("guard")) guard = Boolean.parseBoolean(v);
        else if (key.equals("guardListen")) guardListen = v;
        else if (key.equals("guardBackend")) guardBackend = v;
        else if (key.equals("guardJoinHint")) guardJoinHint = v;
        else if (key.equals("commands")) commands = Boolean.parseBoolean(v);
        else if (key.equals("runtimeChecks")) runtimeChecks = Boolean.parseBoolean(v);
        else if (key.equals("serverJar")) serverJar = v;
        else if (key.equals("debug")) debug = Boolean.parseBoolean(v);
    }

    private void readServerProperties() {
        File f = new File(serverDir, "server.properties");
        if (!f.isFile()) return;
        Properties p = new Properties();
        try {
            InputStream in = new FileInputStream(f);
            try {
                p.load(in);
            } finally {
                in.close();
            }
            serverPort = Integer.parseInt(p.getProperty("server-port", "25565").trim());
            serverIp = p.getProperty("server-ip", "").trim();
        } catch (IOException e) {
            Log.debug("could not read server.properties: " + e);
        } catch (NumberFormatException e) {
            Log.debug("bad server-port in server.properties");
        }
    }

    /** Why registration can't run, or null when it can. */
    String registrationProblem() {
        if (gateway.isEmpty()) return "gateway is not set";
        if (token.isEmpty()) return "token is not set";
        if (serverId.isEmpty()) return "serverId is not set";
        if (address.isEmpty()) return "address is not set";
        return null;
    }

    /** Where the real server listens. */
    String localAddress() {
        if (guard) return guardBackendAddress();
        String host = serverIp.isEmpty() || serverIp.equals("0.0.0.0") || serverIp.equals("::") ? "127.0.0.1" : serverIp;
        return new HostPort(host, serverPort).toString();
    }

    String guardBackendAddress() {
        return guardBackend.isEmpty() ? "127.0.0.1:" + serverPort : guardBackend;
    }

    static final class Protocols {
        int min;
        int max;
        /** Exact list, or null for a plain range. */
        List<Integer> list;
    }

    /** The declared protocol override, or null. */
    Protocols protocols() {
        return protocols.isEmpty() ? null : parseProtocols(protocols);
    }

    static Protocols parseProtocols(String v) {
        Protocols p = new Protocols();
        String s = v.replace(" ", "");
        int dash = s.indexOf('-');
        if (dash > 0 && s.indexOf(',') < 0) {
            p.min = Integer.parseInt(s.substring(0, dash));
            p.max = Integer.parseInt(s.substring(dash + 1));
        } else {
            TreeSet<Integer> set = new TreeSet<Integer>();
            for (String part : s.split(",")) {
                if (!part.isEmpty()) set.add(Integer.parseInt(part));
            }
            if (set.isEmpty()) throw new IllegalArgumentException("no protocols");
            p.list = new ArrayList<Integer>(set);
            p.min = set.first();
            p.max = set.last();
        }
        if (p.min <= 0 || p.max < p.min) throw new IllegalArgumentException("bad protocol range");
        return p;
    }

    static List<String> splitList(String v) {
        List<String> out = new ArrayList<String>();
        for (String part : v.split(",")) {
            part = part.trim().toLowerCase(Locale.ROOT);
            if (!part.isEmpty()) out.add(part);
        }
        return out;
    }

    static Map<String, String> parseArgs(String args) {
        Map<String, String> out = new HashMap<String, String>();
        if (args == null || args.trim().isEmpty()) return out;
        if (args.indexOf('=') < 0) {
            out.put("config", args.trim());
            return out;
        }
        for (String part : args.split(";")) {
            int eq = part.indexOf('=');
            if (eq > 0) out.put(part.substring(0, eq).trim(), part.substring(eq + 1));
        }
        return out;
    }

    static String snake(String camel) {
        return camel.replaceAll("([a-z])([A-Z])", "$1_$2").toUpperCase(Locale.ROOT);
    }

    private static String first(String... values) {
        for (String v : values) {
            if (v != null && !v.isEmpty()) return v;
        }
        return "";
    }

    private static void writeTemplate(File f) {
        String template = ""
                + "# anymcp: registers this server at an anymcp gateway and can guard it.\n"
                + "# Every key can also be set as an environment variable, e.g. ANYMCP_TOKEN or ANYMCP_SERVER_ID.\n"
                + "\n"
                + "# Gateway API base URL and the owner token from the gateway admin.\n"
                + "gateway=\n"
                + "token=\n"
                + "# Lowercase ID, also the subdomain: <serverId>.<gateway domain>\n"
                + "serverId=\n"
                + "name=\n"
                + "description=\n"
                + "# host:port the gateway connects to. With guard=true, the guard's public address.\n"
                + "address=\n"
                + "hidden=false\n"
                + "heartbeatSeconds=15\n"
                + "\n"
                + "# Detected automatically. Set only to override, e.g. loader=neoforge,\n"
                + "# protocols=763-767 (range) or protocols=763,765 (exact list).\n"
                + "loader=\n"
                + "protocols=\n"
                + "# Mods a client must have to be matched to this server (comma separated).\n"
                + "requiredClientMods=\n"
                + "\n"
                + "# The server expects a PROXY protocol header (e.g. Paper proxies.proxy-protocol=true).\n"
                + "proxyProtocol=false\n"
                + "\n"
                + "# Guard: only the gateway can connect. Move the server to another port in\n"
                + "# server.properties (server-port=25566, server-ip=127.0.0.1) and listen on the public port here.\n"
                + "guard=false\n"
                + "guardListen=0.0.0.0:25565\n"
                + "# Defaults to 127.0.0.1:<server-port>.\n"
                + "guardBackend=\n"
                + "# Shown to players who connect directly, e.g. play.example.com\n"
                + "guardJoinHint=\n"
                + "\n"
                + "# In-game /hub and /server <id> (move via the gateway) and /global <msg> (operators;\n"
                + "# broadcast to every server). Handled at the network layer, so no plugin is needed.\n"
                + "commands=true\n"
                + "# Ask the running server which mods, channels and protocols are actually loaded.\n"
                + "runtimeChecks=true\n"
                + "# Wrapper mode (java -jar anymcp.jar): the real server jar. Detected when empty.\n"
                + "serverJar=\n"
                + "debug=false\n";
        try {
            OutputStream out = new FileOutputStream(f);
            try {
                out.write(template.getBytes(StandardCharsets.UTF_8));
            } finally {
                out.close();
            }
            Log.info("created " + f);
        } catch (IOException e) {
            Log.warn("could not create " + f + ": " + e);
        }
    }
}
