package group.senger.vecta.universal;

import java.lang.instrument.Instrumentation;
import java.lang.reflect.Field;
import java.lang.reflect.Method;
import java.lang.reflect.Modifier;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collection;
import java.util.Collections;
import java.util.Deque;
import java.util.HashMap;
import java.util.HashSet;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.TreeSet;
import java.util.regex.Pattern;

/**
 * Asks the running server what actually loaded, by reflection on stable entry points of each
 * platform. Nothing is compiled against a platform and every step may fail; the report then falls
 * back to the file scan.
 */
final class RuntimeProbe {

    static final class Result {
        String loader = "";
        String software = "";
        /** Loaded mods, lowercase id to version. Empty unless modsKnown. */
        final Map<String, String> mods = new TreeMap<String, String>();
        boolean modsKnown;
        /** Mods bundled inside other mods. */
        final Set<String> nested = new HashSet<String>();
        /** Mods the loader says are server-only. */
        final Set<String> serverOnly = new HashSet<String>();
        /** Channel name to required. */
        final Map<String, Boolean> channels = new TreeMap<String, Boolean>();
        final Map<String, String> channelVersions = new HashMap<String, String>();
        final TreeSet<Integer> protocols = new TreeSet<Integer>();
        final Map<String, String> plugins = new TreeMap<String, String>();
        final List<String> notes = new ArrayList<String>();

        String summary() {
            return (loader.isEmpty() ? "unknown loader" : loader)
                    + ", " + (modsKnown ? mods.size() + " mods" : "mods unknown")
                    + ", " + channels.size() + " channels"
                    + (plugins.isEmpty() ? "" : ", " + plugins.size() + " plugins")
                    + (protocols.isEmpty() ? "" : ", protocols " + protocols.first() + "-" + protocols.last());
        }
    }

    private static final String FABRIC_LOADER = "net.fabricmc.loader.api.FabricLoader";
    private static final String FABRIC_PLAY = "net.fabricmc.fabric.api.networking.v1.ServerPlayNetworking";
    private static final String FABRIC_CONFIG = "net.fabricmc.fabric.api.networking.v1.ServerConfigurationNetworking";
    private static final String FABRIC_PAYLOADS = "net.fabricmc.fabric.api.networking.v1.PayloadTypeRegistry";
    private static final String NEO_MODLIST = "net.neoforged.fml.ModList";
    private static final String NEO_NETWORK = "net.neoforged.neoforge.network.registration.NetworkRegistry";
    private static final String FORGE_MODLIST = "net.minecraftforge.fml.ModList";
    private static final String FORGE_LOADER_1_8 = "net.minecraftforge.fml.common.Loader";
    private static final String FORGE_LOADER_1_7 = "cpw.mods.fml.common.Loader";
    private static final String BUKKIT = "org.bukkit.Bukkit";
    private static final String VIA = "com.viaversion.viaversion.api.Via";
    private static final String VIA_LEGACY = "us.myles.ViaVersion.api.Via";
    private static final List<String> CLASSES = Arrays.asList(FABRIC_LOADER, FABRIC_PLAY, FABRIC_CONFIG, FABRIC_PAYLOADS,
            NEO_MODLIST, NEO_NETWORK, FORGE_MODLIST, FORGE_LOADER_1_8, FORGE_LOADER_1_7, BUKKIT, VIA, VIA_LEGACY);
    private static final Pattern CHANNEL = Pattern.compile("[a-z0-9_.-]+:[a-z0-9_./-]+");

    private RuntimeProbe() {
    }

    static Result probe() {
        Result r = new Result();
        Map<String, Class<?>> c = findClasses(CLASSES);
        try {
            if (c.containsKey(FABRIC_LOADER)) {
                fabric(c.get(FABRIC_LOADER), r);
                fabricChannels(c, r);
            } else if (c.containsKey(NEO_MODLIST)) {
                modList(c.get(NEO_MODLIST), "neoforge", r);
                if (c.containsKey(NEO_NETWORK)) neoChannels(c.get(NEO_NETWORK), r);
            } else if (c.containsKey(FORGE_MODLIST)) {
                modList(c.get(FORGE_MODLIST), "forge", r);
            } else if (c.containsKey(FORGE_LOADER_1_8) || c.containsKey(FORGE_LOADER_1_7)) {
                legacyForge(c.containsKey(FORGE_LOADER_1_8) ? c.get(FORGE_LOADER_1_8) : c.get(FORGE_LOADER_1_7), r);
            }
        } catch (Throwable t) {
            r.notes.add("mod loader: " + t);
        }
        if (c.containsKey(BUKKIT)) {
            try {
                bukkit(c.get(BUKKIT), r);
            } catch (Throwable t) {
                r.notes.add("bukkit: " + t);
            }
        }
        try {
            via(c, r);
        } catch (Throwable t) {
            r.notes.add("viaversion: " + t);
        }
        for (String n : r.notes) Log.debug("runtime check: " + n);
        return r;
    }

    // --- platforms ---

    private static void fabric(Class<?> loaderClass, Result r) throws Exception {
        Object loader = call(loaderClass, "getInstance");
        for (Object container : (Collection<?>) call(loader, "getAllMods")) {
            Object meta = call(container, "getMetadata");
            String id = lower(call(meta, "getId"));
            r.mods.put(id, String.valueOf(call(call(meta, "getVersion"), "getFriendlyString")));
            try {
                Object parent = call(container, "getContainingMod");
                if (parent != null && Boolean.TRUE.equals(call(parent, "isPresent"))) r.nested.add(id);
            } catch (NoSuchMethodException ignored) {
                // older loader: the file scan identifies bundled mods
            }
            try {
                if ("SERVER".equals(String.valueOf(call(meta, "getEnvironment")))) r.serverOnly.add(id);
            } catch (NoSuchMethodException ignored) {
                // no environment info
            }
        }
        r.modsKnown = true;
        boolean quilt = r.mods.containsKey("quilt_loader");
        r.loader = quilt ? "quilt" : "fabric";
        r.software = (quilt ? "Quilt Loader " + r.mods.get("quilt_loader") : "Fabric Loader " + r.mods.get("fabricloader"))
                + minecraft(r);
    }

    private static void fabricChannels(Map<String, Class<?>> c, Result r) {
        for (String name : new String[] {FABRIC_PLAY, FABRIC_CONFIG}) {
            if (!c.containsKey(name)) continue;
            try {
                for (Object id : (Collection<?>) call(c.get(name), "getGlobalReceivers")) addChannel(r, String.valueOf(id), "", false);
            } catch (Throwable t) {
                r.notes.add(name + ".getGlobalReceivers: " + t);
            }
        }
        if (!c.containsKey(FABRIC_PAYLOADS)) return;
        for (String method : new String[] {"playC2S", "configurationC2S"}) {
            try {
                Object registry = call(c.get(FABRIC_PAYLOADS), method);
                for (Class<?> k = registry.getClass(); k != null && k != Object.class; k = k.getSuperclass()) {
                    for (Field f : k.getDeclaredFields()) {
                        if (Modifier.isStatic(f.getModifiers()) || !Map.class.isAssignableFrom(f.getType())) continue;
                        if (!makeAccessible(f)) continue;
                        Map<?, ?> map = (Map<?, ?>) f.get(registry);
                        if (map == null) continue;
                        for (Object key : new ArrayList<Object>(map.keySet())) {
                            String id = String.valueOf(key);
                            if (CHANNEL.matcher(id).matches()) addChannel(r, id, "", false);
                        }
                    }
                }
            } catch (Throwable t) {
                r.notes.add("PayloadTypeRegistry." + method + ": " + t);
            }
        }
    }

    /** Forge 1.13+ and NeoForge: ModList.get().getMods() gives IModInfo entries. */
    private static void modList(Class<?> modListClass, String loader, Result r) throws Exception {
        Object list = call(modListClass, "get");
        if (list == null) throw new IllegalStateException("ModList not initialized");
        for (Object info : (Collection<?>) call(list, "getMods")) {
            r.mods.put(lower(call(info, "getModId")), String.valueOf(call(info, "getVersion")));
        }
        r.modsKnown = true;
        r.loader = loader;
        String platform = loader.equals("neoforge") ? "neoforge" : "forge";
        r.software = (loader.equals("neoforge") ? "NeoForge " : "Forge ") + r.mods.get(platform) + minecraft(r);
    }

    /** NeoForge 1.20.5+: payload registrations with their version and optional flag. */
    private static void neoChannels(Class<?> registry, Result r) {
        try {
            Field f = registry.getDeclaredField("PAYLOAD_REGISTRATIONS");
            if (!makeAccessible(f)) throw new IllegalAccessException("PAYLOAD_REGISTRATIONS not accessible");
            for (Object inner : ((Map<?, ?>) f.get(null)).values()) {
                for (Map.Entry<?, ?> e : ((Map<?, ?>) inner).entrySet()) {
                    Object reg = e.getValue();
                    if (reg == null) continue;
                    String version = "";
                    boolean required = true;
                    try {
                        version = String.valueOf(call(reg, "version"));
                    } catch (Exception ignored) {
                        // keep empty
                    }
                    try {
                        required = !Boolean.TRUE.equals(call(reg, "optional"));
                    } catch (Exception ignored) {
                        // assume required, NeoForge's default
                    }
                    addChannel(r, String.valueOf(e.getKey()), version, required);
                }
            }
        } catch (NoSuchFieldException e) {
            r.notes.add("NeoForge NetworkRegistry has no PAYLOAD_REGISTRATIONS (pre-1.20.5)");
        } catch (Throwable t) {
            r.notes.add("NeoForge channels: " + t);
        }
    }

    /** Forge 1.7.10 - 1.12.2: Loader.instance().getActiveModList(). */
    private static void legacyForge(Class<?> loaderClass, Result r) throws Exception {
        Object loader = call(loaderClass, "instance");
        for (Object mc : (Collection<?>) call(loader, "getActiveModList")) {
            r.mods.put(lower(call(mc, "getModId")), String.valueOf(call(mc, "getVersion")));
        }
        r.modsKnown = true;
        r.loader = "forge";
        String mcVersion = "";
        try {
            Field f = loaderClass.getField("MC_VERSION");
            mcVersion = String.valueOf(f.get(null));
        } catch (Exception ignored) {
            // unknown
        }
        r.software = "Forge " + r.mods.get("forge") + (mcVersion.isEmpty() ? "" : " (Minecraft " + mcVersion + ")");
    }

    private static void bukkit(Class<?> bukkit, Result r) throws Exception {
        String name = String.valueOf(call(bukkit, "getName"));
        String version = String.valueOf(call(bukkit, "getVersion"));
        if (r.loader.isEmpty()) {
            // Old Paper/Spigot builds report "CraftBukkit"; their version string names the fork.
            String n = name.toLowerCase(Locale.ROOT);
            String v = version.toLowerCase(Locale.ROOT);
            if (n.equals("craftbukkit")) n = v.contains("paper") ? "paper" : v.contains("spigot") ? "spigot" : "craftbukkit";
            r.loader = n;
            r.software = name + " " + version;
            r.modsKnown = true; // plugins are server-side: no client mods
        }
        Object pm = call(bukkit, "getPluginManager");
        for (Object plugin : (Object[]) call(pm, "getPlugins")) {
            Object desc = call(plugin, "getDescription");
            r.plugins.put(String.valueOf(call(plugin, "getName")), String.valueOf(call(desc, "getVersion")));
        }
    }

    private static void via(Map<String, Class<?>> c, Result r) throws Exception {
        Class<?> via = c.containsKey(VIA) ? c.get(VIA) : c.get(VIA_LEGACY);
        if (via == null && c.containsKey(BUKKIT)) {
            // Without instrumentation, reach Via through its plugin class loader.
            Object plugin = call(call(c.get(BUKKIT), "getPluginManager"), "getPlugin", "ViaVersion");
            if (plugin != null) {
                ClassLoader cl = plugin.getClass().getClassLoader();
                for (String name : new String[] {VIA, VIA_LEGACY}) {
                    try {
                        via = Class.forName(name, true, cl);
                        break;
                    } catch (ClassNotFoundException ignored) {
                        // try the next package
                    }
                }
            }
        }
        if (via == null) return;
        Object api = call(via, "getAPI");
        if (api == null) return;
        for (String m : new String[] {"getFullSupportedProtocolVersions", "getFullSupportedVersions", "getSupportedProtocolVersions", "getSupportedVersions"}) {
            try {
                addProtocols(r, call(api, m));
                if (!r.protocols.isEmpty()) break;
            } catch (NoSuchMethodException ignored) {
                // try the next name
            }
        }
        try {
            Object server = call(api, "getServerVersion");
            for (String m : new String[] {"supportedProtocolVersions", "supportedVersions"}) {
                try {
                    addProtocols(r, call(server, m));
                    break;
                } catch (NoSuchMethodException ignored) {
                    // try the next name
                }
            }
        } catch (NoSuchMethodException ignored) {
            // old API
        }
    }

    private static void addProtocols(Result r, Object collection) throws Exception {
        if (!(collection instanceof Collection)) return;
        for (Object o : (Collection<?>) collection) {
            Object n = o instanceof Integer ? o : call(o, "getVersion");
            if (n instanceof Integer && (Integer) n > 0 && (Integer) n < 0x40000000) r.protocols.add((Integer) n);
        }
    }

    // --- helpers ---

    private static String minecraft(Result r) {
        String mc = r.mods.get("minecraft");
        return mc == null ? "" : " (Minecraft " + mc + ")";
    }

    private static void addChannel(Result r, String name, String version, boolean required) {
        name = name.toLowerCase(Locale.ROOT);
        Boolean prev = r.channels.get(name);
        r.channels.put(name, (prev != null && prev) || required);
        if (!version.isEmpty() && !"null".equals(version)) r.channelVersions.put(name, version);
    }

    private static String lower(Object o) {
        return String.valueOf(o).toLowerCase(Locale.ROOT);
    }

    /** Finds classes wherever they were loaded: all loaded classes if we have instrumentation, else the known class loaders. */
    static Map<String, Class<?>> findClasses(Collection<String> names) {
        Map<String, Class<?>> out = new HashMap<String, Class<?>>();
        Set<String> wanted = new HashSet<String>(names);
        Instrumentation inst = Vecta.instrumentation();
        if (inst != null) {
            for (Class<?> k : inst.getAllLoadedClasses()) {
                if (wanted.contains(k.getName()) && !out.containsKey(k.getName())) out.put(k.getName(), k);
            }
        }
        Set<ClassLoader> loaders = new LinkedHashSet<ClassLoader>();
        for (Thread t : Thread.getAllStackTraces().keySet()) {
            ClassLoader cl = t.getContextClassLoader();
            if (cl != null) loaders.add(cl);
        }
        loaders.add(ClassLoader.getSystemClassLoader());
        for (String name : wanted) {
            if (out.containsKey(name)) continue;
            for (ClassLoader cl : loaders) {
                try {
                    out.put(name, Class.forName(name, false, cl));
                    break;
                } catch (Throwable ignored) {
                    // not visible from this loader
                }
            }
        }
        return out;
    }

    /** Calls a method by name. A Class target means a static call. */
    static Object call(Object target, String name, Object... args) throws Exception {
        boolean isStatic = target instanceof Class;
        Class<?> type = isStatic ? (Class<?>) target : target.getClass();
        Method m = findMethod(type, name, args.length, isStatic);
        if (m == null) throw new NoSuchMethodException(type.getName() + "." + name);
        return m.invoke(isStatic ? null : target, args);
    }

    /** Prefers a public declaration on a public type, so non-public implementation classes can be called. */
    private static Method findMethod(Class<?> type, String name, int arity, boolean isStatic) {
        Deque<Class<?>> queue = new ArrayDeque<Class<?>>();
        Set<Class<?>> seen = new HashSet<Class<?>>();
        queue.add(type);
        Method fallback = null;
        while (!queue.isEmpty()) {
            Class<?> k = queue.poll();
            if (!seen.add(k)) continue;
            for (Method m : k.getDeclaredMethods()) {
                if (!m.getName().equals(name) || m.getParameterTypes().length != arity) continue;
                if (Modifier.isStatic(m.getModifiers()) != isStatic) continue;
                if (Modifier.isPublic(m.getModifiers()) && Modifier.isPublic(k.getModifiers())) return m;
                if (fallback == null) fallback = m;
            }
            if (k.getSuperclass() != null) queue.add(k.getSuperclass());
            queue.addAll(Arrays.asList(k.getInterfaces()));
        }
        if (fallback != null && makeAccessible(fallback)) return fallback;
        return null;
    }

    /** Reads a field by name, searching superclasses and opening modules if needed. Null on any failure. */
    static Object field(Object o, String name) {
        if (o == null) return null;
        for (Class<?> k = o.getClass(); k != null && k != Object.class; k = k.getSuperclass()) {
            try {
                return read(k.getDeclaredField(name), o);
            } catch (NoSuchFieldException e) {
                // try the superclass
            }
        }
        return null;
    }

    /** Sets a field by name, searching superclasses and opening modules if needed. Returns success. */
    static boolean setField(Object o, String name, Object value) {
        if (o == null) return false;
        for (Class<?> k = o.getClass(); k != null && k != Object.class; k = k.getSuperclass()) {
            try {
                Field f = k.getDeclaredField(name);
                if (!makeAccessible(f)) return false;
                f.set(o, value);
                return true;
            } catch (NoSuchFieldException e) {
                // try the superclass
            } catch (Throwable t) {
                return false;
            }
        }
        return false;
    }

    /** Reads one field, opening its module if needed. Null on failure. */
    static Object read(Field f, Object o) {
        if (!makeAccessible(f)) return null;
        try {
            return f.get(o);
        } catch (Throwable t) {
            return null;
        }
    }

    static boolean makeAccessible(java.lang.reflect.AccessibleObject o) {
        try {
            o.setAccessible(true);
            return true;
        } catch (RuntimeException e) {
            // Java 9+ module boundary: open the package to us with instrumentation, then retry.
            Class<?> owner = o instanceof Field ? ((Field) o).getDeclaringClass() : ((Method) o).getDeclaringClass();
            if (openPackage(owner)) {
                try {
                    o.setAccessible(true);
                    return true;
                } catch (RuntimeException ignored) {
                    // still closed
                }
            }
            return false;
        }
    }

    static boolean openPackage(Class<?> target) {
        Instrumentation inst = Vecta.instrumentation();
        if (inst == null) return false;
        try {
            Method getModule = Class.class.getMethod("getModule");
            Object targetModule = getModule.invoke(target);
            Object ourModule = getModule.invoke(RuntimeProbe.class);
            Class<?> moduleClass = Class.forName("java.lang.Module");
            String cn = target.getName();
            String pkg = cn.substring(0, cn.lastIndexOf('.'));
            Map<String, Set<Object>> opens = Collections.singletonMap(pkg, Collections.singleton(ourModule));
            Method redefine = Instrumentation.class.getMethod("redefineModule", moduleClass, Set.class, Map.class, Map.class, Set.class, Map.class);
            redefine.invoke(inst, targetModule, Collections.emptySet(), Collections.emptyMap(), opens,
                    Collections.emptySet(), Collections.emptyMap());
            return true;
        } catch (Throwable t) {
            Log.debug("could not open " + target.getName() + ": " + t);
            return false;
        }
    }
}
