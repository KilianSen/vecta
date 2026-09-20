package io.netty.channel.fake;

import io.netty.channel.Channel;
import java.nio.channels.Selector;
import java.util.Map;

/**
 * Stand-ins for the Netty internals channel discovery reads, in both layouts: Netty 4.1, where the
 * event loop holds the selector and the channel map and a selector key is attached to the channel
 * itself, and Netty 4.2 (Minecraft 1.21.11 and newer), where an IoHandler holds them and a key is
 * attached to an IoRegistration pointing at the channel's inner Unsafe.
 */
public final class Fakes {
    private Fakes() {
    }

    /** What Netty 4.2 registers is this inner Unsafe, which only its outer reference ties to the channel. */
    public abstract static class AbstractChannel implements Channel {
        public final Object unsafe = new Unsafe();

        private final class Unsafe {
        }
    }

    /** A listen channel: parentless, with "Server" in its name like NioServerSocketChannel. */
    public static final class ServerSocketChannel extends AbstractChannel {
        @Override
        public Channel parent() {
            return null;
        }
    }

    /** A player connection: it has a parent, so it is not a listen channel. */
    public static final class SocketChannel extends AbstractChannel {
        private final Channel parent;

        public SocketChannel(Channel parent) {
            this.parent = parent;
        }

        @Override
        public Channel parent() {
            return parent;
        }
    }

    /** Netty 4.1 NIO: the event loop holds the selector. */
    public static final class NioEventLoop {
        private final Selector selector;

        public NioEventLoop(Selector selector) {
            this.selector = selector;
        }
    }

    /** Netty 4.1 epoll: the event loop holds the channels by file descriptor. */
    public static final class EpollEventLoop {
        private final Map<Integer, Object> channels;

        public EpollEventLoop(Map<Integer, Object> channels) {
            this.channels = channels;
        }
    }

    /** Netty 4.2: the event loop only holds the IoHandler that does the registering. */
    public static final class SingleThreadIoEventLoop {
        private final Object ioHandler;

        public SingleThreadIoEventLoop(Object ioHandler) {
            this.ioHandler = ioHandler;
        }
    }

    /** Netty 4.2 NIO. */
    public static final class NioIoHandler {
        private final Selector unwrappedSelector;

        public NioIoHandler(Selector unwrappedSelector) {
            this.unwrappedSelector = unwrappedSelector;
        }
    }

    /** Netty 4.2 epoll. */
    public static final class EpollIoHandler {
        private final Map<Integer, Object> registrations;

        public EpollIoHandler(Map<Integer, Object> registrations) {
            this.registrations = registrations;
        }
    }

    /** Netty 4.2: what a selector key or the registration map points at. */
    public static final class IoRegistration {
        private final Object handle;

        public IoRegistration(Object handle) {
            this.handle = handle;
        }
    }
}
