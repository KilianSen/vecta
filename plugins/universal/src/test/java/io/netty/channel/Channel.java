package io.netty.channel;

/**
 * Stand-in for Netty's own Channel interface. Channel discovery recognizes a channel by this name,
 * so the tests declare it here instead of depending on Netty.
 */
public interface Channel {
    Channel parent();
}
