package group.senger.anymcp.universal;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;

/** Combines the file scan, runtime probe and local ping into the gateway registration body. */
final class Report {
    static final int MAX_MODS = 2000;
    static final int MAX_CHANNELS = 5000;
    static final int MAX_PROTOCOLS = 1000;

    /** Loader and platform IDs that say nothing about a modpack. */
    private static final Set<String> PLATFORM = new HashSet<String>(Arrays.asList(
            "minecraft", "java", "fabricloader", "fabric-loader", "quilt_loader", "forge", "neoforge", "fml", "mcp", "anymcp"));

    private Report() {
    }

    static String loader(Config cfg, RuntimeProbe.Result rt) {
        if (!cfg.loader.isEmpty()) return cfg.loader;
        if (rt != null && !rt.loader.isEmpty()) return rt.loader;
        return Platform.detect(cfg.serverDir);
    }

    static Map<String, Object> build(Config cfg, ModScanner.Result scan, RuntimeProbe.Result rt, StatusPing.Status ping) {
        String loader = loader(cfg, rt);
        Map<String, String> mods = clientMods(loader, scan, rt);

        Map<String, Object> o = new LinkedHashMap<String, Object>();
        o.put("name", cfg.name.isEmpty() ? cfg.serverId : cfg.name);
        if (!cfg.description.isEmpty()) o.put("description", cfg.description);
        o.put("address", cfg.address);
        o.put("loader", loader.isEmpty() ? "vanilla" : loader);
        o.put("reporter", "jar");
        String software = rt != null && !rt.software.isEmpty() ? rt.software : ping != null ? ping.versionName : "";
        if (!software.isEmpty()) o.put("software", software);

        List<Integer> protocols = null;
        int min = 0;
        int max = 0;
        Config.Protocols declared = cfg.protocols();
        if (declared != null) {
            protocols = declared.list;
            min = declared.min;
            max = declared.max;
        } else if (rt != null && !rt.protocols.isEmpty()) {
            protocols = new ArrayList<Integer>(rt.protocols);
            min = rt.protocols.first();
            max = rt.protocols.last();
        } else if (ping != null && ping.protocol > 0) {
            min = max = ping.protocol;
        }
        if (min > 0) {
            o.put("minProtocol", min);
            o.put("maxProtocol", max);
        }
        if (protocols != null && !protocols.isEmpty() && protocols.size() <= MAX_PROTOCOLS) o.put("protocols", protocols);

        o.put("mods", new ArrayList<String>(mods.keySet()));
        o.put("modVersions", mods);
        o.put("requiredClientMods", cfg.requiredClientMods);

        List<Map<String, Object>> channels = new ArrayList<Map<String, Object>>();
        if (rt != null) {
            for (Map.Entry<String, Boolean> e : rt.channels.entrySet()) {
                if (channels.size() >= MAX_CHANNELS) break;
                Map<String, Object> ch = new LinkedHashMap<String, Object>();
                ch.put("name", e.getKey());
                String version = rt.channelVersions.get(e.getKey());
                if (version != null) ch.put("version", version);
                if (e.getValue()) ch.put("required", true);
                channels.add(ch);
            }
        }
        o.put("channels", channels);
        if (ping != null) {
            o.put("players", ping.players);
            o.put("maxPlayers", ping.maxPlayers);
        }
        o.put("hidden", cfg.hidden);
        o.put("proxyProtocol", cfg.proxyProtocol && !cfg.guard);
        o.put("guard", cfg.guard);
        return o;
    }

    /**
     * Mods a client may care about. With runtime data: what actually loaded, minus platform IDs,
     * bundled libraries and server-only or client-only mods. Without: the top-level mod files that
     * this loader would load, with the same filters.
     */
    static Map<String, String> clientMods(String loader, ModScanner.Result scan, RuntimeProbe.Result rt) {
        Map<String, ModScanner.ModInfo> files = new HashMap<String, ModScanner.ModInfo>();
        for (ModScanner.ModInfo m : scan.mods) {
            if (compatible(loader, m.kind) && !files.containsKey(m.id)) files.put(m.id, m);
        }
        Map<String, String> out = new TreeMap<String, String>();
        if (rt != null && rt.modsKnown) {
            for (Map.Entry<String, String> e : rt.mods.entrySet()) {
                String id = e.getKey();
                if (ignored(id) || rt.nested.contains(id) || rt.serverOnly.contains(id)) continue;
                ModScanner.ModInfo f = files.get(id);
                if (f == null && scan.nestedIds.contains(id)) continue;
                if (f != null && (f.side == ModScanner.Side.SERVER || f.side == ModScanner.Side.CLIENT)) continue;
                out.put(id, e.getValue());
                if (out.size() >= MAX_MODS) break;
            }
        } else {
            for (ModScanner.ModInfo f : files.values()) {
                if (ignored(f.id) || f.side == ModScanner.Side.SERVER || f.side == ModScanner.Side.CLIENT) continue;
                out.put(f.id, f.version);
                if (out.size() >= MAX_MODS) break;
            }
        }
        return out;
    }

    private static boolean ignored(String id) {
        return PLATFORM.contains(id) || (id.startsWith("fabric-") && !id.equals("fabric-api"));
    }

    /** Whether a loader loads mods described by this metadata kind. */
    static boolean compatible(String loader, String kind) {
        if (loader.equals("fabric")) return kind.equals("fabric");
        if (loader.equals("quilt")) return kind.equals("fabric") || kind.equals("quilt");
        if (loader.equals("neoforge")) return kind.equals("neoforge") || kind.equals("forge");
        if (loader.equals("forge")) return kind.equals("forge") || kind.equals("legacyforge");
        return false;
    }
}
