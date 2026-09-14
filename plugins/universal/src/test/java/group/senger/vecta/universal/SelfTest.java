package group.senger.vecta.universal;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.util.Arrays;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.atomic.AtomicReference;
import java.util.zip.ZipEntry;
import java.util.zip.ZipOutputStream;

/** Dependency-free tests, run by build.sh. */
public final class SelfTest {
    private static int failures;

    interface Case {
        void run() throws Exception;
    }

    public static void main(String[] args) {
        run("guard codec matches the Go test vector", SelfTest::guardVector);
        run("guard codec rejects bad headers", SelfTest::guardRejects);
        run("lenient json", SelfTest::json);
        run("mods.toml parsing", SelfTest::toml);
        run("mod scan and client mod selection", SelfTest::scan);
        run("runtime data overrides the file scan", SelfTest::runtimeMerge);
        run("config parsing", SelfTest::config);
        run("status ping", SelfTest::statusPing);
        run("guard proxy end to end", SelfTest::guardProxy);
        if (failures > 0) {
            System.out.println(failures + " test(s) FAILED");
            System.exit(1);
        }
        System.out.println("all tests passed");
    }

    private static void run(String name, Case c) {
        try {
            c.run();
            System.out.println("ok   " + name);
        } catch (Throwable t) {
            failures++;
            System.out.println("FAIL " + name + ": " + t);
            t.printStackTrace(System.out);
        }
    }

    private static void check(boolean cond, String msg) {
        if (!cond) throw new AssertionError(msg);
    }

    private static String hex(byte[] b) {
        StringBuilder s = new StringBuilder();
        for (byte x : b) s.append(String.format("%02x", x & 0xff));
        return s.toString();
    }

    private static final byte[] KEY = GuardCodec.key("tok-plugin");
    private static final byte[] NONCE = filled(12, 7);
    private static final long TS = 1_800_000_000L;

    private static byte[] filled(int n, int v) {
        byte[] b = new byte[n];
        Arrays.fill(b, (byte) v);
        return b;
    }

    private static InetSocketAddress addr(String ip, int port) throws IOException {
        return new InetSocketAddress(InetAddress.getByName(ip), port);
    }

    // Same vector as internal/guard/guard_test.go.
    private static void guardVector() throws Exception {
        byte[] h = GuardCodec.header(KEY, addr("203.0.113.9", 51234), addr("10.0.0.5", 25565), TS, NONCE);
        String want = "0d0a0d0a000d0a515549540a21110044cb0071090a000005c82263dde0003501000000006b49d200"
                + "07070707070707070707070719fc445ee8d3afb4cf0a2b8dd852950f9acb0ec6736b617f72b73d0c19dcf58b";
        check(hex(h).equals(want), "header\n got " + hex(h) + "\nwant " + want);
        GuardCodec.Result r = GuardCodec.verify(KEY, h, TS + 30, null);
        check(!r.local && r.src.getPort() == 51234 && r.src.getAddress().getHostAddress().equals("203.0.113.9"), "src " + r.src);
        check(r.dst.getPort() == 25565, "dst " + r.dst);
        byte[] read = GuardCodec.readHeader(new ByteArrayInputStream(concat(h, "rest".getBytes(StandardCharsets.US_ASCII))));
        check(Arrays.equals(read, h), "readHeader");

        GuardCodec.Result local = GuardCodec.verify(KEY, GuardCodec.localHeader(KEY, TS, NONCE), TS, null);
        check(local.local && local.src == null, "local header");
        byte[] v6 = GuardCodec.header(KEY, addr("2001:db8::1", 4000), addr("2001:db8::2", 25565), TS, NONCE);
        check(GuardCodec.verify(KEY, v6, TS, null).src.getPort() == 4000, "ipv6");
    }

    private static void guardRejects() throws Exception {
        byte[] good = GuardCodec.header(KEY, addr("203.0.113.9", 51234), addr("10.0.0.5", 25565), TS, NONCE);
        byte[] tampered = good.clone();
        tampered[16] ^= 1;
        byte[] plain = GuardCodec.plainHeader(addr("1.2.3.4", 1), addr("5.6.7.8", 2));
        Object[][] cases = {
            {"wrong key", GuardCodec.key("other"), good, TS},
            {"tampered", KEY, tampered, TS},
            {"unsigned", KEY, plain, TS},
            {"too old", KEY, good, TS + 121},
            {"future", KEY, good, TS - 121},
        };
        for (Object[] c : cases) {
            try {
                GuardCodec.verify((byte[]) c[1], (byte[]) c[2], (Long) c[3], null);
                throw new AssertionError(c[0] + ": accepted");
            } catch (GeneralSecurityException | IllegalArgumentException expected) {
                // rejected
            }
        }
        final Set<String> seen = new HashSet<String>();
        GuardCodec.NonceCheck check = new GuardCodec.NonceCheck() {
            @Override
            public boolean seen(byte[] nonce) {
                return !seen.add(hex(nonce));
            }
        };
        GuardCodec.verify(KEY, good, TS, check);
        try {
            GuardCodec.verify(KEY, good, TS, check);
            throw new AssertionError("replay accepted");
        } catch (GeneralSecurityException expected) {
            // rejected
        }
    }

    private static void json() {
        Object v = Json.parse("{\"a\": [1, 2,], // comment\n \"b\": \"x\ty\\u0041\", /* c */ \"c\": {\"d\": true,},}");
        check(Json.list(Json.obj(v).get("a")).size() == 2, "array");
        check(Json.str(Json.obj(v).get("b")).equals("x\tyA"), "string " + Json.obj(v).get("b"));
        check(Boolean.TRUE.equals(Json.path(v, "c", "d")), "nested");
        String written = Json.write(v);
        check(Json.str(Json.obj(Json.parse(written)).get("b")).equals("x\tyA"), "round trip " + written);
    }

    private static final String MODS_TOML = ""
            + "modLoader=\"javafml\" #mandatory\n"
            + "loaderVersion=\"[47,)\"\n"
            + "license='MIT'\n"
            + "[[mods]] #mandatory\n"
            + "modId=\"waystones\"\n"
            + "version=\"${file.jarVersion}\"\n"
            + "displayName=\"Waystones\"\n"
            + "description='''\n"
            + "Multi\n"
            + "modId = \"not-a-mod\"\n"
            + "'''\n"
            + "authors=[\"a\", \"b]\"]\n"
            + "[[mods]]\n"
            + "  modId = \"serverutil\"\n"
            + "  version = \"1.0\"\n"
            + "  displayTest = \"IGNORE_ALL_VERSION\"\n"
            + "[[dependencies.waystones]]\n"
            + "    modId=\"minecraft\"\n"
            + "    mandatory=true\n"
            + "    side=\"BOTH\"\n";

    private static void toml() {
        Toml t = Toml.parse(MODS_TOML);
        List<Map<String, Object>> mods = t.tableArrays.get("mods");
        check(mods != null && mods.size() == 2, "mods tables " + t.tableArrays);
        check("waystones".equals(mods.get(0).get("modId")), "first mod " + mods.get(0));
        check(String.valueOf(mods.get(0).get("description")).startsWith("Multi"), "multiline");
        check("IGNORE_ALL_VERSION".equals(mods.get(1).get("displayTest")), "displayTest");
        check(t.tableArrays.get("dependencies.waystones").size() == 1, "dependencies");
        check("MIT".equals(t.root.get("license")), "literal string");
    }

    private static void writeJar(File f, String[]... entries) throws IOException {
        f.getParentFile().mkdirs();
        OutputStream out = new FileOutputStream(f);
        try {
            out.write(jarBytes((Object[][]) entries));
        } finally {
            out.close();
        }
    }

    private static byte[] jarBytes(Object[]... entries) throws IOException {
        ByteArrayOutputStream buf = new ByteArrayOutputStream();
        ZipOutputStream zip = new ZipOutputStream(buf);
        for (Object[] e : entries) {
            zip.putNextEntry(new ZipEntry((String) e[0]));
            zip.write(e[1] instanceof byte[] ? (byte[]) e[1] : ((String) e[1]).getBytes(StandardCharsets.UTF_8));
            zip.closeEntry();
        }
        zip.close();
        return buf.toByteArray();
    }

    private static File serverDir() throws IOException {
        File dir = File.createTempFile("vecta-test", "");
        dir.delete();
        dir.mkdirs();
        File mods = new File(dir, "mods");
        byte[] nested = jarBytes(new Object[] {"fabric.mod.json", "{\"id\":\"cloth-config\",\"version\":\"15\"}"});
        ByteArrayOutputStream outer = new ByteArrayOutputStream();
        ZipOutputStream zip = new ZipOutputStream(outer);
        zip.putNextEntry(new ZipEntry("fabric.mod.json"));
        zip.write("{\"id\":\"AppleSkin\",\"version\":\"3.0\",\"environment\":\"*\"}".getBytes(StandardCharsets.UTF_8));
        zip.closeEntry();
        zip.putNextEntry(new ZipEntry("META-INF/jars/cloth.jar"));
        zip.write(nested);
        zip.closeEntry();
        zip.close();
        mods.mkdirs();
        FileOutputStream fo = new FileOutputStream(new File(mods, "appleskin.jar"));
        fo.write(outer.toByteArray());
        fo.close();

        writeJar(new File(mods, "ledger.jar"), new String[] {"fabric.mod.json", "{\"id\":\"ledger\",\"environment\":\"server\"}"});
        writeJar(new File(mods, "sodium.jar"), new String[] {"fabric.mod.json", "{\"id\":\"sodium\",\"environment\":\"client\"}"});
        writeJar(new File(mods, "jei.jar"),
                new String[] {"META-INF/neoforge.mods.toml", "[[mods]]\nmodId=\"jei\"\nversion=\"${file.jarVersion}\"\n"},
                new String[] {"META-INF/MANIFEST.MF", "Manifest-Version: 1.0\r\nImplementation-Version: 19.5\r\n"});
        writeJar(new File(mods, "waystones.jar"), new String[] {"META-INF/mods.toml", MODS_TOML});
        writeJar(new File(mods, "broken.jar"), new String[] {"fabric.mod.json", "{not json"});
        writeJar(new File(dir, "plugins/via.jar"), new String[] {"plugin.yml", "name: ViaVersion\nversion: '5.0.0'\n"});
        new File(mods, "not-a-jar.txt").createNewFile();
        return dir;
    }

    private static ModScanner.ModInfo find(ModScanner.Result r, String id) {
        for (ModScanner.ModInfo m : r.mods) {
            if (m.id.equals(id)) return m;
        }
        return null;
    }

    private static void scan() throws Exception {
        ModScanner.Result r = ModScanner.scan(serverDir());
        check(r.jars == 7, "jars " + r.jars);
        check(find(r, "appleskin") != null && find(r, "appleskin").side == ModScanner.Side.UNKNOWN, "appleskin " + r.mods);
        check(r.nestedIds.contains("cloth-config") && find(r, "cloth-config") == null, "nested " + r.nestedIds);
        check(find(r, "ledger").side == ModScanner.Side.SERVER, "ledger");
        check(find(r, "sodium").side == ModScanner.Side.CLIENT, "sodium");
        check("19.5".equals(find(r, "jei").version) && "neoforge".equals(find(r, "jei").kind), "jei " + find(r, "jei"));
        check(find(r, "serverutil").side == ModScanner.Side.OPTIONAL, "serverutil");
        check(find(r, "waystones").side == ModScanner.Side.BOTH && find(r, "waystones").version.isEmpty(), "waystones");
        check(find(r, "viaversion") != null && "bukkit".equals(find(r, "viaversion").kind), "plugin");

        check(Report.clientMods("fabric", r, null).keySet().equals(set("appleskin")), "fabric " + Report.clientMods("fabric", r, null));
        check(Report.clientMods("neoforge", r, null).keySet().equals(set("jei", "waystones", "serverutil")), "neoforge " + Report.clientMods("neoforge", r, null));
        check(Report.clientMods("forge", r, null).keySet().equals(set("waystones", "serverutil")), "forge");
        check(Report.clientMods("paper", r, null).isEmpty(), "paper");
    }

    private static Set<String> set(String... s) {
        return new HashSet<String>(Arrays.asList(s));
    }

    private static void runtimeMerge() throws Exception {
        File dir = serverDir();
        ModScanner.Result scan = ModScanner.scan(dir);
        RuntimeProbe.Result rt = new RuntimeProbe.Result();
        rt.loader = "fabric";
        rt.modsKnown = true;
        rt.software = "Fabric Loader 0.16 (Minecraft 1.21.1)";
        for (String id : new String[] {"minecraft", "java", "fabricloader", "fabric-api", "fabric-networking-api-v1", "appleskin", "cloth-config", "ledger", "lithium"}) {
            rt.mods.put(id, "1");
        }
        rt.serverOnly.add("lithium");
        rt.channels.put("appleskin:sync", false);
        rt.channels.put("create:main", true);
        rt.protocols.add(767);

        Config cfg = new Config();
        cfg.serverDir = dir;
        cfg.serverId = "pack";
        cfg.address = "10.0.0.5:25565";
        cfg.requiredClientMods = Config.splitList("AppleSkin");
        StatusPing.Status ping = new StatusPing.Status();
        ping.protocol = 767;
        ping.players = 2;
        ping.maxPlayers = 20;

        Map<String, Object> body = Report.build(cfg, scan, rt, ping);
        check("fabric".equals(body.get("loader")), "loader " + body);
        check(set("fabric-api", "appleskin").equals(new HashSet<Object>((List<?>) body.get("mods"))), "mods " + body.get("mods"));
        check(Integer.valueOf(767).equals(body.get("minProtocol")), "protocol");
        check(((List<?>) body.get("channels")).size() == 2, "channels");
        check(Json.write(body).contains("\"required\":true"), "required channel " + Json.write(body));
        check(Boolean.FALSE.equals(body.get("guard")), "guard flag");
    }

    private static void config() {
        Config.Protocols range = Config.parseProtocols("5-774");
        check(range.min == 5 && range.max == 774 && range.list == null, "range");
        Config.Protocols list = Config.parseProtocols("767, 763,765");
        check(list.min == 763 && list.max == 767 && list.list.size() == 3, "list");
        check(Config.snake("guardJoinHint").equals("GUARD_JOIN_HINT"), "snake");
        Map<String, String> a = Config.parseArgs("serverId=pack;guard=true");
        check("pack".equals(a.get("serverId")) && "true".equals(a.get("guard")), "args");
        check("/x/vecta.properties".equals(Config.parseArgs("/x/vecta.properties").get("config")), "config path arg");
        check(HostPort.parse("[::1]:25566", 1).port == 25566 && HostPort.parse("host", 25565).port == 25565, "hostport");
    }

    private static void statusPing() throws Exception {
        final ServerSocket ss = new ServerSocket(0, 5, InetAddress.getLoopbackAddress());
        Thread t = new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    Socket s = ss.accept();
                    InputStream in = s.getInputStream();
                    byte[] header = GuardCodec.readHeader(in); // proxyProtocol=true sends LOCAL
                    check(header[12] == 0x20, "LOCAL header");
                    Mc.readFrame(in, 1024);
                    Mc.readFrame(in, 16);
                    ByteArrayOutputStream p = new ByteArrayOutputStream();
                    Mc.writeVarInt(p, 0);
                    Mc.writeString(p, "{\"version\":{\"name\":\"1.21.1\",\"protocol\":767},\"players\":{\"max\":20,\"online\":3}}");
                    s.getOutputStream().write(Mc.frame(p.toByteArray()));
                    s.close();
                } catch (Exception e) {
                    e.printStackTrace(System.out);
                }
            }
        });
        t.start();
        StatusPing.Status st = StatusPing.ping("127.0.0.1:" + ss.getLocalPort(), true, 3000);
        ss.close();
        check(st.protocol == 767 && st.players == 3 && st.maxPlayers == 20 && st.versionName.equals("1.21.1"), "status");
    }

    private static void guardProxy() throws Exception {
        final ServerSocket backend = new ServerSocket(0, 5, InetAddress.getLoopbackAddress());
        final AtomicReference<byte[]> backendHeader = new AtomicReference<byte[]>();
        Thread bt = new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    while (true) {
                        Socket s = backend.accept();
                        InputStream in = s.getInputStream();
                        backendHeader.set(GuardCodec.readHeader(in));
                        byte[] hello = new byte[5];
                        Mc.readFully(in, hello);
                        s.getOutputStream().write("world".getBytes(StandardCharsets.US_ASCII));
                        s.close();
                    }
                } catch (IOException ignored) {
                    // closed
                }
            }
        });
        bt.setDaemon(true);
        bt.start();

        Config cfg = new Config();
        cfg.token = "tok-guard";
        cfg.guard = true;
        cfg.guardListen = "127.0.0.1:0";
        cfg.guardBackend = "127.0.0.1:" + backend.getLocalPort();
        cfg.guardJoinHint = "play.test";
        cfg.proxyProtocol = true;
        Guard guard = new Guard(cfg);
        int port = guard.start();
        byte[] key = GuardCodec.key("tok-guard");
        long now = System.currentTimeMillis() / 1000L;
        byte[] signed = GuardCodec.header(key, addr("203.0.113.9", 5000), addr("10.0.0.5", 25565), now, NONCE);
        try {
            // Signed: forwarded, and the backend sees the player's address in a plain PROXY header.
            Socket c = connect(port);
            c.getOutputStream().write(concat(signed, "hello".getBytes(StandardCharsets.US_ASCII)));
            byte[] world = new byte[5];
            Mc.readFully(c.getInputStream(), world);
            check(new String(world, StandardCharsets.US_ASCII).equals("world"), "forwarded");
            c.close();
            byte[] h = backendHeader.get();
            check(h[12] == 0x21 && (h[16] & 0xff) == 203 && (h[19] & 0xff) == 9, "backend PROXY header " + hex(h));

            // Direct login: disconnect message with the join hint.
            c = connect(port);
            ByteArrayOutputStream hs = new ByteArrayOutputStream();
            Mc.writeVarInt(hs, 0);
            Mc.writeVarInt(hs, 767);
            Mc.writeString(hs, "backend.example");
            hs.write(0x63);
            hs.write(0xdd);
            Mc.writeVarInt(hs, 2);
            c.getOutputStream().write(Mc.frame(hs.toByteArray()));
            ByteArrayInputStream p = new ByteArrayInputStream(Mc.readFrame(c.getInputStream(), 1 << 16));
            check(Mc.readVarInt(p) == 0, "disconnect id");
            String msg = Mc.readString(p, 1 << 16);
            check(msg.contains("play.test"), "hint " + msg);
            c.close();

            // Replayed and wrongly signed headers: closed without data.
            check(closedWithoutData(port, signed), "replay was forwarded");
            byte[] wrong = GuardCodec.header(GuardCodec.key("other"), addr("203.0.113.9", 5000), addr("10.0.0.5", 25565), now, filled(12, 9));
            check(closedWithoutData(port, wrong), "wrong key was forwarded");
        } finally {
            guard.close();
            backend.close();
        }
    }

    private static Socket connect(int port) throws IOException {
        Socket c = new Socket();
        c.connect(new InetSocketAddress(InetAddress.getLoopbackAddress(), port), 2000);
        c.setSoTimeout(3000);
        return c;
    }

    private static boolean closedWithoutData(int port, byte[] header) throws IOException {
        Socket c = connect(port);
        try {
            c.getOutputStream().write(concat(header, "hello".getBytes(StandardCharsets.US_ASCII)));
            return c.getInputStream().read() == -1;
        } catch (IOException e) {
            return true; // reset
        } finally {
            c.close();
        }
    }

    private static byte[] concat(byte[] a, byte[] b) {
        byte[] out = Arrays.copyOf(a, a.length + b.length);
        System.arraycopy(b, 0, out, a.length, b.length);
        return out;
    }
}
