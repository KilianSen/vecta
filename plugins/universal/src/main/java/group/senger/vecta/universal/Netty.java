package group.senger.vecta.universal;

import java.lang.reflect.Field;
import java.util.ArrayList;
import java.util.Arrays;
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
 * Netty 4.2 (Minecraft 1.21.11 and newer) moved those registrations off the event loop into an
 * IoHandler it holds, and made a selector key point at an IoRegistration instead of the channel, so
 * both layouts are handled here.
 *
 * <p>Everything here is best-effort and version-sensitive on Netty internals; every step is guarded
 * and a failure just yields fewer channels.
 */
final class Netty {
    private Netty() {
    }

    /** The server's listen (parentless) channels, where the child-connection handler lives. */
    static List<Object> serverChannels() {
        return serverChannelsOn(eventLoops());
    }

    /** The listen channels registered on the given event loops. */
    static List<Object> serverChannelsOn(List<Object> loops) {
        Set<Object> raw = java.util.Collections.newSetFromMap(new IdentityHashMap<Object, Boolean>());
        for (Object loop : loops) {
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
        List<Object> loops = eventLoops();
        Set<String> kinds = new java.util.TreeSet<String>();
        for (Object loop : loops) kinds.add(loop.getClass().getName());
        return "loops=" + loops.size() + kinds + " serverChannels=" + serverChannelsOn(loops).size();
    }

    /** Whether this JVM runs any Netty event loop at all (so a server could be in it). */
    static boolean hasEventLoops() {
        return !eventLoops().isEmpty();
    }

    /**
     * For diagnostics: each event loop's (and, on Netty 4.2, its IoHandler's) channel-holding fields
     * and how many channels are extracted.
     */
    static void dumpLoops() {
        for (Object loop : eventLoops()) {
            for (Object holder : holders(loop)) dumpFields(holder);
        }
    }

    private static void dumpFields(Object holder) {
        StringBuilder sb = new StringBuilder(holder.getClass().getName()).append(" fields:");
        for (Field f : declaredFields(holder)) {
            String tn = f.getType().getName();
            if (tn.contains("Map") || tn.contains("Selector") || f.getType().isArray() || tn.contains("Channel")) {
                Object v = RuntimeProbe.read(f, holder);
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
            registeredOn(holder, chs);
        } catch (Throwable ignored) {
            // nothing
        }
        Log.debug(sb + " -> channels=" + chs.size());
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
        // NioEventLoop, EpollEventLoop, KQueueEventLoop, SingleThreadIoEventLoop (Netty 4.2), ...
        // but not the *EventLoopGroup wrappers.
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

    /**
     * Adds every channel registered on one event loop to out. Netty 4.1 keeps the selector and the
     * channel map on the event loop itself; Netty 4.2 moved them into an IoHandler the loop holds,
     * so read both.
     */
    private static void channelsOf(Object loop, Set<Object> out) throws Exception {
        int before = out.size();
        List<Object> holders = holders(loop);
        for (Object holder : holders) {
            try {
                registeredOn(holder, out);
            } catch (Throwable ignored) {
                // skip this holder
            }
        }
        // Field names drift across Netty versions; if nothing turned up, scan any map field.
        if (out.size() == before) {
            for (Object holder : holders) {
                for (Field f : declaredFields(holder)) {
                    if (f.getType().getName().contains("Map")) addFromMap(RuntimeProbe.read(f, holder), out);
                }
            }
        }
    }

    /** Where an event loop's registrations live: the loop itself and, on Netty 4.2, its IoHandler. */
    private static List<Object> holders(Object loop) {
        List<Object> out = new ArrayList<Object>();
        out.add(loop);
        Object handler = field(loop, "ioHandler"); // Netty 4.2 SingleThreadIoEventLoop
        if (handler != null) out.add(handler);
        return out;
    }

    /** Adds the channels one event loop or IoHandler has registered to out. */
    private static void registeredOn(Object holder, Set<Object> out) throws Exception {
        // NIO: the (possibly wrapped) Selector's registered keys carry the channel as attachment
        // (Netty 4.1) or its IoRegistration (4.2).
        Object selector = field(holder, "selector");
        if (selector == null) selector = field(holder, "unwrappedSelector");
        if (selector instanceof java.nio.channels.Selector) {
            for (java.nio.channels.SelectionKey key : ((java.nio.channels.Selector) selector).keys()) {
                addChannel(key.attachment(), out);
            }
        }
        // Epoll/KQueue keep a channel map: "channels" (Netty 4.1) or "ids" (Netty 4.0); their 4.2
        // IoHandlers keep "registrations".
        addFromMap(field(holder, "channels"), out);
        addFromMap(field(holder, "ids"), out);
        addFromMap(field(holder, "registrations"), out);
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

    private static void addChannel(Object o, Set<Object> out) {
        Object ch = channelOf(o);
        if (ch != null) out.add(ch);
    }

    /** Fields a Netty wrapper keeps its channel in, in the order they are tried. */
    private static final String[] WRAPPED = {"handle", "channel", "ch", "this$0"};

    /**
     * The channel behind a selector attachment or registration-map value. On Netty 4.1 that is the
     * channel itself; on 4.2 it is an IoRegistration holding an IoHandle, which for a channel is the
     * channel's own inner Unsafe — so follow those fields (the Unsafe's synthetic outer reference
     * included) until a channel turns up.
     */
    private static Object channelOf(Object o) {
        Set<Object> seen = java.util.Collections.newSetFromMap(new IdentityHashMap<Object, Boolean>());
        java.util.Deque<Object> queue = new java.util.ArrayDeque<Object>();
        if (o != null) queue.add(o);
        for (int steps = 0; !queue.isEmpty() && steps < 16; steps++) {
            Object v = queue.poll();
            if (v == null || !seen.add(v)) continue;
            if (isChannel(v)) return v;
            for (String name : WRAPPED) {
                Object next = field(v, name);
                if (next != null && !seen.contains(next)) queue.add(next);
            }
        }
        return null;
    }

    /** Whether an object is a Netty channel rather than a wrapper around one. */
    private static boolean isChannel(Object o) {
        java.util.Deque<Class<?>> queue = new java.util.ArrayDeque<Class<?>>();
        Set<Class<?>> seen = new java.util.HashSet<Class<?>>();
        queue.add(o.getClass());
        while (!queue.isEmpty()) {
            Class<?> k = queue.poll();
            if (k == null || !seen.add(k)) continue;
            // endsWith, not equals: 1.7.x era servers relocate Netty into their own package.
            if (k.getName().endsWith("io.netty.channel.Channel")) return true;
            if (k.getSuperclass() != null) queue.add(k.getSuperclass());
            queue.addAll(Arrays.asList(k.getInterfaces()));
        }
        return false;
    }

    private static Object field(Object o, String name) {
        return RuntimeProbe.field(o, name);
    }
}
