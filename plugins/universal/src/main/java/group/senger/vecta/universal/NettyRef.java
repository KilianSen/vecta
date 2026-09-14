package group.senger.vecta.universal;

import java.lang.reflect.InvocationHandler;
import java.lang.reflect.Method;
import java.lang.reflect.Proxy;
import java.util.List;

/**
 * Reflective access to the server's Netty, resolved against the classloader that actually loaded it.
 * The jar can't link against Netty (it lives in the loader's child classloader), so every Netty
 * type and method is looked up by name here and cached. Only public interface methods are used, so
 * no setAccessible is needed.
 */
final class NettyRef {
    final ClassLoader loader;
    final Class<?> channelHandler;
    final Class<?> inboundHandler;

    private final Method bufReadable, bufReaderIndex, bufGetBytes, bufWriteBytes, bufRelease, allocBuffer;
    private final Method chanAlloc, chanPipeline, chanWriteFlush, chanIsActive, chanRemote, chanClose, chanEventLoop;
    private final Method pipeContext, pipeGet, pipeAddBefore, pipeAddFirst, pipeAddLast, pipeRemove, pipeNames;
    private final Method ctxChannel, ctxPipeline, ctxFireRead, ctxWriteFlush;
    private final Method fireActive, fireInactive, fireRegistered, fireUnregistered, fireReadComplete,
            fireUserEvent, fireWritability, fireException;

    NettyRef(ClassLoader loader) throws Exception {
        this.loader = loader;
        Class<?> buf = cls("io.netty.buffer.ByteBuf");
        Class<?> alloc = cls("io.netty.buffer.ByteBufAllocator");
        Class<?> channel = cls("io.netty.channel.Channel");
        Class<?> pipeline = cls("io.netty.channel.ChannelPipeline");
        Class<?> ctx = cls("io.netty.channel.ChannelHandlerContext");
        channelHandler = cls("io.netty.channel.ChannelHandler");
        inboundHandler = cls("io.netty.channel.ChannelInboundHandler");

        bufReadable = buf.getMethod("readableBytes");
        bufReaderIndex = buf.getMethod("readerIndex");
        bufGetBytes = buf.getMethod("getBytes", int.class, byte[].class);
        bufWriteBytes = buf.getMethod("writeBytes", byte[].class);
        bufRelease = buf.getMethod("release");
        allocBuffer = alloc.getMethod("buffer");

        chanAlloc = channel.getMethod("alloc");
        chanPipeline = channel.getMethod("pipeline");
        chanWriteFlush = channel.getMethod("writeAndFlush", Object.class);
        chanIsActive = channel.getMethod("isActive");
        chanRemote = channel.getMethod("remoteAddress");
        chanClose = channel.getMethod("close");
        chanEventLoop = channel.getMethod("eventLoop");

        pipeContext = pipeline.getMethod("context", String.class);
        pipeGet = pipeline.getMethod("get", String.class);
        pipeAddBefore = pipeline.getMethod("addBefore", String.class, String.class, channelHandler);
        pipeAddFirst = pipeline.getMethod("addFirst", String.class, channelHandler);
        pipeAddLast = pipeline.getMethod("addLast", String.class, channelHandler);
        pipeRemove = pipeline.getMethod("remove", channelHandler);
        pipeNames = pipeline.getMethod("names");

        ctxChannel = ctx.getMethod("channel");
        ctxPipeline = ctx.getMethod("pipeline");
        ctxFireRead = ctx.getMethod("fireChannelRead", Object.class);
        ctxWriteFlush = ctx.getMethod("writeAndFlush", Object.class);
        fireActive = ctx.getMethod("fireChannelActive");
        fireInactive = ctx.getMethod("fireChannelInactive");
        fireRegistered = ctx.getMethod("fireChannelRegistered");
        fireUnregistered = ctx.getMethod("fireChannelUnregistered");
        fireReadComplete = ctx.getMethod("fireChannelReadComplete");
        fireUserEvent = ctx.getMethod("fireUserEventTriggered", Object.class);
        fireWritability = ctx.getMethod("fireChannelWritabilityChanged");
        fireException = ctx.getMethod("fireExceptionCaught", Throwable.class);
    }

    private Class<?> cls(String name) throws ClassNotFoundException {
        return Class.forName(name, false, loader);
    }

    // --- ByteBuf ---

    /** Copies the readable bytes without consuming them. */
    byte[] peek(Object buf) throws Exception {
        int len = (Integer) bufReadable.invoke(buf);
        byte[] out = new byte[len];
        bufGetBytes.invoke(buf, bufReaderIndex.invoke(buf), out);
        return out;
    }

    void release(Object buf) {
        try {
            bufRelease.invoke(buf);
        } catch (Exception ignored) {
            // already released or not ref-counted
        }
    }

    /** Wraps bytes in a fresh ByteBuf allocated by the channel. */
    Object buffer(Object channel, byte[] bytes) throws Exception {
        Object b = allocBuffer.invoke(chanAlloc.invoke(channel));
        bufWriteBytes.invoke(b, (Object) bytes);
        return b;
    }

    // --- Channel / pipeline ---

    Object pipeline(Object channel) throws Exception {
        return chanPipeline.invoke(channel);
    }

    Object ctxChannel(Object ctx) throws Exception {
        return ctxChannel.invoke(ctx);
    }

    Object ctxPipeline(Object ctx) throws Exception {
        return ctxPipeline.invoke(ctx);
    }

    Object get(Object pipeline, String name) throws Exception {
        return pipeGet.invoke(pipeline, name);
    }

    void addLast(Object pipeline, String name, Object handler) throws Exception {
        pipeAddLast.invoke(pipeline, name, handler);
    }

    boolean isActive(Object channel) {
        try {
            return Boolean.TRUE.equals(chanIsActive.invoke(channel));
        } catch (Exception e) {
            return false;
        }
    }

    Object remoteAddress(Object channel) {
        try {
            return chanRemote.invoke(channel);
        } catch (Exception e) {
            return null;
        }
    }

    void close(Object channel) {
        try {
            chanClose.invoke(channel);
        } catch (Exception ignored) {
            // already closing
        }
    }

    /** Runs task on the channel's event loop after delayMs (Runnable/TimeUnit are JDK types). */
    void scheduleLater(Object channel, Runnable task, long delayMs) {
        try {
            Object loop = chanEventLoop.invoke(channel);
            Method schedule = loop.getClass().getMethod("schedule", Runnable.class, long.class, java.util.concurrent.TimeUnit.class);
            schedule.invoke(loop, task, delayMs, java.util.concurrent.TimeUnit.MILLISECONDS);
        } catch (Exception e) {
            Log.debug("scheduleLater failed: " + e);
        }
    }

    boolean hasHandler(Object pipeline, String name) throws Exception {
        return pipeGet.invoke(pipeline, name) != null;
    }

    @SuppressWarnings("unchecked")
    List<String> names(Object pipeline) throws Exception {
        return (List<String>) pipeNames.invoke(pipeline);
    }

    void addBefore(Object pipeline, String base, String name, Object handler) throws Exception {
        pipeAddBefore.invoke(pipeline, base, name, handler);
    }

    void addFirst(Object pipeline, String name, Object handler) throws Exception {
        pipeAddFirst.invoke(pipeline, name, handler);
    }

    void remove(Object pipeline, Object handler) {
        try {
            pipeRemove.invoke(pipeline, handler);
        } catch (Exception ignored) {
            // already removed
        }
    }

    /**
     * Sends a raw packet body (varint id + fields) to the client so that "compress" and "prepender"
     * still run but "encoder" is skipped: write from the encoder's own context, which forwards to the
     * next outbound handler toward the head. Falls back to the whole pipeline if "encoder" is absent.
     */
    void sendRaw(Object channel, byte[] packet) {
        try {
            Object pipeline = pipeline(channel);
            Object buf = buffer(channel, packet);
            Object ctx = pipeContext.invoke(pipeline, "encoder");
            if (ctx != null) {
                ctxWriteFlush.invoke(ctx, buf);
            } else {
                chanWriteFlush.invoke(channel, buf);
            }
        } catch (Exception e) {
            Log.debug("sendRaw failed: " + e);
        }
    }

    // --- inbound proxy ---

    /** A per-connection inbound callback; channelRead returns true to consume the message. */
    interface Inbound {
        boolean channelRead(Object ctx, Object msg) throws Exception;

        void channelInactive(Object ctx);
    }

    /** A shared child-connection handler: its handlerAdded fires once per new connection. */
    interface Added {
        void handlerAdded(Object ctx) throws Exception;
    }

    /**
     * A single @Sharable ChannelHandler to install as a ServerBootstrap child handler. On each new
     * connection its handlerAdded runs (to splice in per-connection handlers); all events forward.
     */
    Object newChildHandler(final Added cb) {
        InvocationHandler h = new InvocationHandler() {
            @Override
            public Object invoke(Object proxy, Method method, Object[] args) throws Throwable {
                String n = method.getName();
                Object ctx = args != null && args.length > 0 ? args[0] : null;
                if (n.equals("handlerAdded")) {
                    try {
                        cb.handlerAdded(ctx);
                    } catch (Throwable t) {
                        Log.warn("vecta: installing command handlers failed", t);
                    }
                    return null;
                }
                if (n.equals("isSharable")) return Boolean.TRUE;
                if (n.equals("equals")) return proxy == args[0];
                if (n.equals("hashCode")) return System.identityHashCode(proxy);
                if (n.equals("toString")) return "vecta-child";
                try {
                    forward(n, ctx, args);
                } catch (Throwable ignored) {
                    // best effort
                }
                return null;
            }
        };
        return Proxy.newProxyInstance(loader, new Class<?>[] {inboundHandler}, h);
    }

    private void forward(String n, Object ctx, Object[] args) throws Exception {
        switch (n) {
            case "channelRead": ctxFireRead.invoke(ctx, args[1]); break;
            case "channelActive": fireActive.invoke(ctx); break;
            case "channelInactive": fireInactive.invoke(ctx); break;
            case "channelRegistered": fireRegistered.invoke(ctx); break;
            case "channelUnregistered": fireUnregistered.invoke(ctx); break;
            case "channelReadComplete": fireReadComplete.invoke(ctx); break;
            case "channelWritabilityChanged": fireWritability.invoke(ctx); break;
            case "userEventTriggered": fireUserEvent.invoke(ctx, args[1]); break;
            case "exceptionCaught": fireException.invoke(ctx, args[1]); break;
            default: break;
        }
    }

    /** Creates a Netty ChannelInboundHandler backed by cb that forwards every event it doesn't handle. */
    Object newHandler(final Inbound cb) {
        InvocationHandler h = new InvocationHandler() {
            @Override
            public Object invoke(Object proxy, Method method, Object[] args) throws Throwable {
                String n = method.getName();
                Object ctx = args != null && args.length > 0 ? args[0] : null;
                try {
                    if (n.equals("channelRead")) {
                        if (!cb.channelRead(ctx, args[1])) ctxFireRead.invoke(ctx, args[1]);
                        return null;
                    }
                    if (n.equals("channelInactive")) {
                        try {
                            cb.channelInactive(ctx);
                        } finally {
                            fireInactive.invoke(ctx);
                        }
                        return null;
                    }
                    switch (n) {
                        case "channelActive": fireActive.invoke(ctx); return null;
                        case "channelRegistered": fireRegistered.invoke(ctx); return null;
                        case "channelUnregistered": fireUnregistered.invoke(ctx); return null;
                        case "channelReadComplete": fireReadComplete.invoke(ctx); return null;
                        case "channelWritabilityChanged": fireWritability.invoke(ctx); return null;
                        case "userEventTriggered": fireUserEvent.invoke(ctx, args[1]); return null;
                        case "exceptionCaught": fireException.invoke(ctx, args[1]); return null;
                        case "handlerAdded":
                        case "handlerRemoved": return null;
                        case "isSharable": return Boolean.FALSE;
                        case "equals": return proxy == args[0];
                        case "hashCode": return System.identityHashCode(proxy);
                        case "toString": return "vecta-handler";
                        default: return null;
                    }
                } catch (Throwable t) {
                    Log.debug("handler " + n + " failed: " + t);
                    // Best effort: try to keep the pipeline flowing.
                    try {
                        if (n.equals("channelRead")) ctxFireRead.invoke(ctx, args[1]);
                    } catch (Throwable ignored) {
                        // give up on this event
                    }
                    return null;
                }
            }
        };
        return Proxy.newProxyInstance(loader, new Class<?>[] {inboundHandler}, h);
    }
}
