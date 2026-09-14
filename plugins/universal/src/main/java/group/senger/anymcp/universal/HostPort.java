package group.senger.anymcp.universal;

/** A parsed host:port, accepting [ipv6]:port and a bare host. */
final class HostPort {
    final String host;
    final int port;

    HostPort(String host, int port) {
        this.host = host;
        this.port = port;
    }

    static HostPort parse(String s, int defaultPort) {
        s = s.trim();
        if (s.startsWith("[")) {
            int end = s.indexOf(']');
            if (end < 0) throw new IllegalArgumentException("bad address: " + s);
            String host = s.substring(1, end);
            int port = end + 2 <= s.length() && end + 1 < s.length() && s.charAt(end + 1) == ':'
                    ? Integer.parseInt(s.substring(end + 2)) : defaultPort;
            return new HostPort(host, port);
        }
        int colon = s.lastIndexOf(':');
        if (colon >= 0 && s.indexOf(':') == colon) {
            return new HostPort(s.substring(0, colon), Integer.parseInt(s.substring(colon + 1)));
        }
        return new HostPort(s, defaultPort);
    }

    @Override
    public String toString() {
        return (host.indexOf(':') >= 0 ? "[" + host + "]" : host) + ":" + port;
    }
}
