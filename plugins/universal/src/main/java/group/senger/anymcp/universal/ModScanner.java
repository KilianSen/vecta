package group.senger.anymcp.universal;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.IOException;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Enumeration;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.zip.ZipEntry;
import java.util.zip.ZipFile;
import java.util.zip.ZipInputStream;

/**
 * Reads mod and plugin metadata from the jars in mods/ and plugins/, including mods bundled inside
 * other mods (Fabric META-INF/jars, Forge/NeoForge META-INF/jarjar).
 */
final class ModScanner {

    enum Side {
        /** Needed on both sides (Forge/NeoForge default). */
        BOTH,
        /** Clients may lack it (Forge/NeoForge displayTest other than MATCH_VERSION). */
        OPTIONAL,
        /** Server-only: not loaded on clients (Fabric "server", Quilt "dedicated_server", Bukkit plugins). */
        SERVER,
        /** Client-only: not loaded on a dedicated server. */
        CLIENT,
        /** The metadata doesn't say (Fabric "*", mcmod.info). */
        UNKNOWN
    }

    static final class ModInfo {
        final String id;
        final String version;
        /** fabric, quilt, forge (mods.toml), neoforge (neoforge.mods.toml), legacyforge, bukkit */
        final String kind;
        final Side side;
        final String file;

        ModInfo(String id, String version, String kind, Side side, String file) {
            this.id = id.toLowerCase(Locale.ROOT);
            this.version = version;
            this.kind = kind;
            this.side = side;
            this.file = file;
        }

        @Override
        public String toString() {
            return id + "@" + version + " (" + kind + ", " + side + ", " + file + ")";
        }
    }

    static final class Result {
        /** Mods and plugins found in top-level jars. */
        final List<ModInfo> mods = new ArrayList<ModInfo>();
        /** IDs of mods that only appear bundled inside other jars. */
        final Set<String> nestedIds = new HashSet<String>();
        int jars;
    }

    private static final Set<String> METADATA = new HashSet<String>(Arrays.asList(
            "fabric.mod.json", "quilt.mod.json", "META-INF/mods.toml", "META-INF/neoforge.mods.toml",
            "mcmod.info", "plugin.yml", "paper-plugin.yml", "META-INF/MANIFEST.MF"));
    private static final int MAX_METADATA = 1 << 20;
    private static final int MAX_NESTED = 64 << 20;
    private static final int MAX_DEPTH = 2;
    // Forge's own wording: IGNORE_SERVER_VERSION is for mods clients don't need, IGNORE_ALL_VERSION
    // for mods without a server component. Mods like JEI use them to be optional, so they stay
    // relevant for matching; only the hard requirement is relaxed.
    private static final Set<String> OPTIONAL_DISPLAY_TESTS = new HashSet<String>(Arrays.asList(
            "IGNORE_SERVER_VERSION", "IGNORE_ALL_VERSION", "NONE"));

    private ModScanner() {
    }

    static Result scan(File serverDir) {
        Result r = new Result();
        for (String dirName : new String[] {"mods", "plugins"}) {
            File[] files = new File(serverDir, dirName).listFiles();
            if (files == null) continue;
            Arrays.sort(files);
            for (File f : files) {
                if (!f.isFile() || !f.getName().toLowerCase(Locale.ROOT).endsWith(".jar")) continue;
                r.jars++;
                try {
                    scanJar(f, r);
                } catch (Exception e) {
                    Log.debug("skipping " + f + ": " + e);
                }
            }
        }
        return r;
    }

    static void scanJar(File f, Result r) throws IOException {
        ZipFile zf = new ZipFile(f);
        try {
            Map<String, byte[]> files = new HashMap<String, byte[]>();
            List<ZipEntry> nested = new ArrayList<ZipEntry>();
            Enumeration<? extends ZipEntry> entries = zf.entries();
            while (entries.hasMoreElements()) {
                ZipEntry e = entries.nextElement();
                if (METADATA.contains(e.getName())) {
                    InputStream in = zf.getInputStream(e);
                    try {
                        files.put(e.getName(), readAll(in, MAX_METADATA));
                    } finally {
                        in.close();
                    }
                } else if (isNestedJar(e.getName())) {
                    nested.add(e);
                }
            }
            r.mods.addAll(parse(files, f.getName()));
            for (ZipEntry e : nested) {
                InputStream in = zf.getInputStream(e);
                try {
                    scanNested(readAll(in, MAX_NESTED), f.getName() + "!/" + e.getName(), 1, r);
                } catch (IOException ex) {
                    Log.debug("skipping nested " + e.getName() + " in " + f + ": " + ex);
                } finally {
                    in.close();
                }
            }
        } finally {
            zf.close();
        }
    }

    private static void scanNested(byte[] jar, String label, int depth, Result r) throws IOException {
        Map<String, byte[]> files = new HashMap<String, byte[]>();
        List<byte[]> nested = new ArrayList<byte[]>();
        List<String> nestedNames = new ArrayList<String>();
        ZipInputStream zin = new ZipInputStream(new ByteArrayInputStream(jar));
        try {
            for (ZipEntry e; (e = zin.getNextEntry()) != null; ) {
                if (METADATA.contains(e.getName())) {
                    files.put(e.getName(), readAll(zin, MAX_METADATA));
                } else if (depth < MAX_DEPTH && isNestedJar(e.getName())) {
                    nested.add(readAll(zin, MAX_NESTED));
                    nestedNames.add(e.getName());
                }
            }
        } finally {
            zin.close();
        }
        for (ModInfo m : parse(files, label)) r.nestedIds.add(m.id);
        for (int k = 0; k < nested.size(); k++) {
            scanNested(nested.get(k), label + "!/" + nestedNames.get(k), depth + 1, r);
        }
    }

    private static boolean isNestedJar(String name) {
        return (name.startsWith("META-INF/jars/") || name.startsWith("META-INF/jarjar/")) && name.endsWith(".jar");
    }

    static byte[] readAll(InputStream in, int max) throws IOException {
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        byte[] buf = new byte[8192];
        for (int n; (n = in.read(buf)) > 0; ) {
            if (out.size() + n > max) throw new IOException("entry larger than " + max + " bytes");
            out.write(buf, 0, n);
        }
        return out.toByteArray();
    }

    private static String text(Map<String, byte[]> files, String name) {
        byte[] b = files.get(name);
        if (b == null) return null;
        String s = new String(b, StandardCharsets.UTF_8);
        return s.startsWith("﻿") ? s.substring(1) : s;
    }

    /** Parses every metadata file of one jar. A jar built for several loaders yields one entry per loader. */
    static List<ModInfo> parse(Map<String, byte[]> files, String label) {
        List<ModInfo> out = new ArrayList<ModInfo>();
        String implVersion = manifestValue(text(files, "META-INF/MANIFEST.MF"), "Implementation-Version");

        String fabric = text(files, "fabric.mod.json");
        if (fabric != null) {
            try {
                Map<String, Object> m = Json.obj(Json.parse(fabric));
                String id = Json.str(m.get("id"));
                if (!id.isEmpty()) {
                    String env = Json.str(m.get("environment"));
                    Side side = "server".equals(env) ? Side.SERVER : "client".equals(env) ? Side.CLIENT : Side.UNKNOWN;
                    out.add(new ModInfo(id, cleanVersion(Json.str(m.get("version"))), "fabric", side, label));
                }
            } catch (RuntimeException e) {
                Log.debug(label + ": bad fabric.mod.json: " + e.getMessage());
            }
        }

        String quilt = text(files, "quilt.mod.json");
        if (quilt != null) {
            try {
                Object root = Json.parse(quilt);
                String id = Json.str(Json.path(root, "quilt_loader", "id"));
                if (!id.isEmpty()) {
                    String env = Json.str(Json.path(root, "minecraft", "environment"));
                    Side side = "dedicated_server".equals(env) ? Side.SERVER : "client".equals(env) ? Side.CLIENT : Side.UNKNOWN;
                    out.add(new ModInfo(id, cleanVersion(Json.str(Json.path(root, "quilt_loader", "version"))), "quilt", side, label));
                }
            } catch (RuntimeException e) {
                Log.debug(label + ": bad quilt.mod.json: " + e.getMessage());
            }
        }

        String neo = text(files, "META-INF/neoforge.mods.toml");
        if (neo != null) out.addAll(modsToml(neo, "neoforge", implVersion, label));
        String forge = text(files, "META-INF/mods.toml");
        if (forge != null) out.addAll(modsToml(forge, "forge", implVersion, label));

        String mcmod = text(files, "mcmod.info");
        if (mcmod != null) {
            try {
                Object root = Json.parse(mcmod);
                List<Object> list = root instanceof List ? Json.list(root) : Json.list(Json.obj(root).get("modList"));
                for (Object o : list) {
                    Map<String, Object> m = Json.obj(o);
                    String id = Json.str(m.get("modid"));
                    if (!id.isEmpty()) {
                        out.add(new ModInfo(id, cleanVersion(Json.str(m.get("version"))), "legacyforge", Side.UNKNOWN, label));
                    }
                }
            } catch (RuntimeException e) {
                Log.debug(label + ": bad mcmod.info: " + e.getMessage());
            }
        }

        String plugin = text(files, "paper-plugin.yml");
        if (plugin == null) plugin = text(files, "plugin.yml");
        if (plugin != null) {
            String name = yamlScalar(plugin, "name");
            if (!name.isEmpty()) {
                out.add(new ModInfo(name, yamlScalar(plugin, "version"), "bukkit", Side.SERVER, label));
            }
        }
        return out;
    }

    private static List<ModInfo> modsToml(String text, String kind, String implVersion, String label) {
        List<ModInfo> out = new ArrayList<ModInfo>();
        Toml t;
        try {
            t = Toml.parse(text);
        } catch (RuntimeException e) {
            Log.debug(label + ": bad mods.toml: " + e.getMessage());
            return out;
        }
        boolean clientOnly = Boolean.TRUE.equals(t.root.get("clientSideOnly"));
        List<Map<String, Object>> mods = t.tableArrays.get("mods");
        if (mods == null) return out;
        for (Map<String, Object> m : mods) {
            String id = Json.str(m.get("modId"));
            if (id.isEmpty()) continue;
            String version = Json.str(m.get("version"));
            if (version.contains("${file.jarVersion}")) {
                version = implVersion.isEmpty() ? "" : version.replace("${file.jarVersion}", implVersion);
            }
            String displayTest = Json.str(m.get("displayTest")).toUpperCase(Locale.ROOT);
            Side side = clientOnly ? Side.CLIENT : OPTIONAL_DISPLAY_TESTS.contains(displayTest) ? Side.OPTIONAL : Side.BOTH;
            out.add(new ModInfo(id, cleanVersion(version), kind, side, label));
        }
        return out;
    }

    /** Drops unreplaced build placeholders such as ${version} or @VERSION@. */
    private static String cleanVersion(String v) {
        v = v.trim();
        return v.contains("${") || (v.startsWith("@") && v.endsWith("@")) ? "" : v;
    }

    static String manifestValue(String manifest, String key) {
        if (manifest == null) return "";
        for (String line : manifest.split("\r?\n")) {
            if (line.startsWith(key + ":")) return line.substring(key.length() + 1).trim();
        }
        return "";
    }

    /** A top-level "key: value" scalar from a simple YAML file such as plugin.yml. */
    static String yamlScalar(String yaml, String key) {
        for (String line : yaml.split("\r?\n")) {
            if (!line.startsWith(key + ":")) continue;
            String v = line.substring(key.length() + 1).trim();
            if (v.length() >= 2 && (v.charAt(0) == '"' || v.charAt(0) == '\'') && v.charAt(v.length() - 1) == v.charAt(0)) {
                return v.substring(1, v.length() - 1);
            }
            int comment = v.indexOf(" #");
            return comment >= 0 ? v.substring(0, comment).trim() : v;
        }
        return "";
    }
}
