package group.senger.anymcp.universal;

import java.lang.reflect.Field;
import java.util.ArrayList;
import java.util.IdentityHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Set;

/**
 * Finds the server's live Netty player connections without any Minecraft mappings. Minecraft names
 * its pipeline handlers with string literals ("decoder", "decrypt", "packet_handler", ...) that
 * survive obfuscation, so once we hold a channel we can work with it by those names. Getting the
 * first reference is the hard part: we walk the Netty event-loop threads to their event loops and
 * read the channels registered on them (NIO selector keys, or the epoll channel map on Linux).
 *
 * <p>Everything here is best-effort and version-sensitive on Netty internals; every step is guarded
 * and a failure just yields fewer channels.
 */
final class Netty {
    private Netty() {
    }

    /** The server's listen (parentless) channels, where the child-connection handler lives. */
    static List<Object> serverChannels() {
        Set<Object> raw = java.util.Collections.newSetFromMap(new IdentityHashMap<Object, Boolean>());
        for (Object loop : eventLoops()) {
            try {
                channelsOf(loop, raw);
            } catch (Throwable ignored) {
                // skip this loop
            }
        }
        List<Object> out = new ArrayList<Object>();
        for (Object ch : raw) {
            try {
                if (RuntimeProbe.call(ch, "parent") == null && ch.getClass().getName().contains("Server")) out.add(ch);
            } catch (Throwable ignored) {
                // not a server channel
            }
        }
        return out;
    }

    /** For diagnostics: how many event loops and server channels discovery currently finds. */
    static String counts() {
        int loops = eventLoops().size();
        return "loops=" + loops + " serverChannels=" + serverChannels().size();
    }

    /** For diagnostics: each event loop's channel-holding fields and how many channels are extracted. */
    static void dumpLoops() {
        for (Object loop : eventLoops()) {
            StringBuilder sb = new StringBuilder(loop.getClass().getName()).append(" fields:");
            for (Field f : declaredFields(loop)) {
                String tn = f.getType().getName();
                if (tn.contains("Map") || tn.contains("Selector") || f.getType().isArray() || tn.contains("Channel")) {
                    Object v = RuntimeProbe.read(f, loop);
                    sb.append(' ').append(f.getName()).append('(').append(f.getType().getSimpleName()).append(')');
                    if (v != null) {
                        try {
                            Object vals = RuntimeProbe.call(v, "values");
                            sb.append("=values:").append(vals == null ? "null" : vals.getClass().getSimpleName());
                        } catch (Throwable t) {
                            sb.append("=novalues");
                        }
                    }
                }
            }
            Set<Object> chs = java.util.Collections.newSetFromMap(new IdentityHashMap<Object, Boolean>());
            try {
                channelsOf(loop, chs);
            } catch (Throwable ignored) {
                // nothing
            }
            Log.debug(sb + " -> channels=" + chs.size());
        }
    }

    /** Finds the Netty event-loop objects by walking their threads. */
    private static List<Object> eventLoops() {
        List<Object> out = new ArrayList<Object>();
        Set<Object> seen = java.util.Collections.newSetFromMap(new IdentityHashMap<Object, Boolean>());
        for (Thread t : Thread.getAllStackTraces().keySet()) {
            String cn = t.getClass().getName().toLowerCase(Locale.ROOT);
            String name = String.valueOf(t.getName()).toLowerCase(Locale.ROOT);
            boolean maybe = cn.contains("fastthreadlocalthread") || name.contains("netty")
                    || name.contains("epoll") || name.contains("nio") || name.contains("io-net")
                    || name.contains("server io");
            if (!maybe) continue;
            Object loop = eventLoopOf(t);
            if (loop != null && seen.add(loop)) out.add(loop);
        }
        return out;
    }

    /**
     * The event loop a Netty thread runs, reached through its Runnable. Netty wraps the loop's task
     * in layers (SingleThreadEventExecutor's task -> ThreadExecutorMap$2 -> ...); the executor is
     * held in a field somewhere in that graph, so walk the referenced objects breadth-first.
     */
    private static Object eventLoopOf(Thread t) {
        Set<Object> seen = java.util.Collections.newSetFromMap(new IdentityHashMap<Object, Boolean>());
        java.util.Deque<Object> queue = new java.util.ArrayDeque<Object>();
        Object task = threadTask(t);
        if (task != null) queue.add(task);
        for (int steps = 0; !queue.isEmpty() && steps < 64; steps++) {
            Object o = queue.poll();
            if (o == null || !seen.add(o)) continue;
            if (isEventLoop(o)) return o;
            for (Field f : declaredFields(o)) {
                if (f.getType().isPrimitive()) continue;
                Object v = RuntimeProbe.read(f, o);
                if (v != null && !(v instanceof String) && !seen.contains(v)) queue.add(v);
            }
        }
        return null;
    }

    /** An event loop we can read channels from: a Netty NIO or epoll single-thread executor. */
    private static boolean isEventLoop(Object o) {
        String cn = o.getClass().getName();
        // NioEventLoop, EpollEventLoop, KQueueEventLoop, ... but not the *EventLoopGroup wrappers.
        return cn.startsWith("io.netty.") && cn.endsWith("EventLoop");
    }

    private static List<Field> declaredFields(Object o) {
        List<Field> out = new ArrayList<Field>();
        for (Class<?> k = o.getClass(); k != null && k != Object.class; k = k.getSuperclass()) {
            for (Field f : k.getDeclaredFields()) {
                if (!java.lang.reflect.Modifier.isStatic(f.getModifiers())) out.add(f);
            }
        }
        return out;
    }

    private static Object threadTask(Thread t) {
        Object task = field(t, "target"); // JDK 8-18
        if (task == null) {
            Object holder = field(t, "holder"); // JDK 19+
            if (holder != null) task = field(holder, "task");
        }
        return task;
    }

    /** Adds every channel registered on one event loop to out. */
    private static void channelsOf(Object loop, Set<Object> out) throws Exception {
        int before = out.size();
        // NIO: the (possibly wrapped) Selector's registered keys carry the channel as attachment.
        Object selector = field(loop, "selector");
        if (selector == null) selector = field(loop, "unwrappedSelector");
        if (selector instanceof java.nio.channels.Selector) {
            for (java.nio.channels.SelectionKey key : ((java.nio.channels.Selector) selector).keys()) {
                addChannel(key.attachment(), out);
            }
        }
        // Epoll/KQueue keep a channel map: "channels" (Netty 4.1) or "ids" (Netty 4.0).
        addFromMap(field(loop, "channels"), out);
        addFromMap(field(loop, "ids"), out);
        // Field names drift across Netty versions; if nothing turned up, scan any map field.
        if (out.size() == before) {
            for (Field f : declaredFields(loop)) {
                if (f.getType().getName().contains("Map")) addFromMap(RuntimeProbe.read(f, loop), out);
            }
        }
    }

    /** Adds the channel-like values of a Netty IntObjectMap/Map to out. Handles both a Collection
     * (Netty 4.1) and an array (Netty 4.0's IntObjectHashMap.values()). */
    private static void addFromMap(Object map, Set<Object> out) {
        if (map == null) return;
        try {
            addAll(RuntimeProbe.call(map, "values"), out); // Netty 4.1 IntObjectMap / Map
            return;
        } catch (Throwable ignored) {
            // Netty 4.0's IntObjectMap.values() takes a Class arg; use entries() instead.
        }
        try {
            Object entries = RuntimeProbe.call(map, "entries");
            for (Object e : iterate(entries)) {
                if (e != null) addChannel(RuntimeProbe.call(e, "value"), out);
            }
        } catch (Throwable ignored) {
            // not a map we understand
        }
    }

    private static void addAll(Object values, Set<Object> out) {
        for (Object ch : iterate(values)) addChannel(ch, out);
    }

    /** Iterates a Collection or an array; empty for anything else. */
    private static Iterable<Object> iterate(Object v) {
        List<Object> out = new ArrayList<Object>();
        if (v instanceof Iterable) {
            for (Object o : (Iterable<?>) v) out.add(o);
        } else if (v != null && v.getClass().isArray()) {
            int n = java.lang.reflect.Array.getLength(v);
            for (int i = 0; i < n; i++) out.add(java.lang.reflect.Array.get(v, i));
        }
        return out;
    }

    private static void addChannel(Object ch, Set<Object> out) {
        if (ch != null && ch.getClass().getName().contains("Channel")) out.add(ch);
    }

    private static Object field(Object o, String name) {
        return RuntimeProbe.field(o, name);
    }
}
