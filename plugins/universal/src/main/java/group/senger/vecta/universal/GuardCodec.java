package group.senger.vecta.universal;

import java.io.DataInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.net.Inet4Address;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.security.MessageDigest;
import java.util.Arrays;
import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;

/** The gateway's signed PROXY v2 header (docs/guard-protocol.md), mirroring internal/guard in Go. */
final class GuardCodec {
    static final byte[] SIGNATURE = {0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A};
    static final int TLV_TYPE = 0xE0;
    static final int VERSION = 1;
    static final int NONCE_LEN = 12;
    static final int MAC_LEN = 32;
    static final long MAX_SKEW_SECONDS = 120;
    static final int MAX_HEADER = 16 + 1024;

    private static final int VALUE_LEN = 1 + 8 + NONCE_LEN + MAC_LEN;
    private static final int CMD_LOCAL = 0x20;
    private static final int CMD_PROXY = 0x21;
    private static final int FAM_UNSPEC = 0x00;
    private static final int FAM_TCP4 = 0x11;
    private static final int FAM_TCP6 = 0x21;

    interface NonceCheck {
        /** Records the nonce and reports whether it was seen before. */
        boolean seen(byte[] nonce);
    }

    static final class Result {
        boolean local;
        InetSocketAddress src;
        InetSocketAddress dst;
    }

    private GuardCodec() {
    }

    static byte[] key(String secret) {
        try {
            return MessageDigest.getInstance("SHA-256").digest(("vecta-guard-key-v1:" + secret).getBytes(StandardCharsets.UTF_8));
        } catch (GeneralSecurityException e) {
            throw new IllegalStateException(e);
        }
    }

    static boolean hasSignature(byte[] b) {
        return b.length >= 12 && Arrays.equals(Arrays.copyOf(b, 12), SIGNATURE);
    }

    /** Reads one complete PROXY v2 header. */
    static byte[] readHeader(InputStream in) throws IOException {
        DataInputStream d = new DataInputStream(in);
        byte[] head = new byte[16];
        d.readFully(head);
        if (!hasSignature(head)) throw new IOException("not a PROXY v2 header");
        int len = ((head[14] & 0xff) << 8) | (head[15] & 0xff);
        if (16 + len > MAX_HEADER) throw new IOException("PROXY header too long");
        byte[] out = Arrays.copyOf(head, 16 + len);
        d.readFully(out, 16, len);
        return out;
    }

    static byte[] header(byte[] key, InetSocketAddress src, InetSocketAddress dst, long unixSeconds, byte[] nonce)
            throws GeneralSecurityException {
        int[] fam = new int[1];
        byte[] addr = addressBlock(src, dst, fam);
        return build(key, CMD_PROXY, fam[0], addr, unixSeconds, nonce);
    }

    static byte[] localHeader(byte[] key, long unixSeconds, byte[] nonce) throws GeneralSecurityException {
        return build(key, CMD_LOCAL, FAM_UNSPEC, new byte[0], unixSeconds, nonce);
    }

    /** An unsigned PROXY v2 header for the local server; LOCAL when src is null. */
    static byte[] plainHeader(InetSocketAddress src, InetSocketAddress dst) {
        if (src == null || dst == null) {
            byte[] out = Arrays.copyOf(SIGNATURE, 16);
            out[12] = CMD_LOCAL;
            return out;
        }
        int[] fam = new int[1];
        byte[] addr = addressBlock(src, dst, fam);
        ByteBuffer b = ByteBuffer.allocate(16 + addr.length);
        b.put(SIGNATURE).put((byte) CMD_PROXY).put((byte) fam[0]).putShort((short) addr.length).put(addr);
        return b.array();
    }

    private static byte[] addressBlock(InetSocketAddress src, InetSocketAddress dst, int[] fam) {
        InetAddress s = src.getAddress();
        InetAddress d = dst.getAddress();
        ByteBuffer b;
        if (s instanceof Inet4Address && d instanceof Inet4Address) {
            fam[0] = FAM_TCP4;
            b = ByteBuffer.allocate(12);
            b.put(s.getAddress()).put(d.getAddress());
        } else {
            fam[0] = FAM_TCP6;
            b = ByteBuffer.allocate(36);
            b.put(to16(s)).put(to16(d));
        }
        b.putShort((short) src.getPort()).putShort((short) dst.getPort());
        return b.array();
    }

    private static byte[] to16(InetAddress a) {
        byte[] raw = a.getAddress();
        if (raw.length == 16) return raw;
        byte[] out = new byte[16];
        out[10] = (byte) 0xff;
        out[11] = (byte) 0xff;
        System.arraycopy(raw, 0, out, 12, 4);
        return out;
    }

    private static byte[] mac(byte[] key, int verCmd, int fam, byte[] addr, long ts, byte[] nonce)
            throws GeneralSecurityException {
        Mac m = Mac.getInstance("HmacSHA256");
        m.init(new SecretKeySpec(key, "HmacSHA256"));
        m.update("vecta-guard-v1".getBytes(StandardCharsets.US_ASCII));
        m.update((byte) verCmd);
        m.update((byte) fam);
        m.update(addr);
        m.update((byte) VERSION);
        m.update(ByteBuffer.allocate(8).putLong(ts).array());
        m.update(nonce);
        return m.doFinal();
    }

    private static byte[] build(byte[] key, int verCmd, int fam, byte[] addr, long ts, byte[] nonce)
            throws GeneralSecurityException {
        if (nonce.length != NONCE_LEN) throw new IllegalArgumentException("nonce must be 12 bytes");
        ByteBuffer b = ByteBuffer.allocate(16 + addr.length + 3 + VALUE_LEN);
        b.put(SIGNATURE).put((byte) verCmd).put((byte) fam).putShort((short) (addr.length + 3 + VALUE_LEN));
        b.put(addr);
        b.put((byte) TLV_TYPE).putShort((short) VALUE_LEN).put((byte) VERSION).putLong(ts).put(nonce);
        b.put(mac(key, verCmd, fam, addr, ts, nonce));
        return b.array();
    }

    /**
     * Verifies a complete header. Format errors throw IllegalArgumentException, a bad signature or
     * timestamp or a replay throws GeneralSecurityException.
     */
    static Result verify(byte[] key, byte[] h, long nowSeconds, NonceCheck nonces) throws GeneralSecurityException {
        if (h.length < 16 || !hasSignature(h)) throw new IllegalArgumentException("not a PROXY v2 header");
        int verCmd = h[12] & 0xff;
        int fam = h[13] & 0xff;
        int len = ((h[14] & 0xff) << 8) | (h[15] & 0xff);
        if (len != h.length - 16) throw new IllegalArgumentException("length mismatch");
        Result res = new Result();
        int addrLen;
        if (verCmd == CMD_LOCAL) {
            res.local = true;
            addrLen = 0;
        } else if (verCmd == CMD_PROXY && fam == FAM_TCP4) {
            addrLen = 12;
        } else if (verCmd == CMD_PROXY && fam == FAM_TCP6) {
            addrLen = 36;
        } else {
            throw new IllegalArgumentException("unsupported command/family");
        }
        if (len < addrLen) throw new IllegalArgumentException("short address block");
        byte[] addr = Arrays.copyOfRange(h, 16, 16 + addrLen);

        byte[] value = null;
        int p = 16 + addrLen;
        while (p < h.length) {
            if (h.length - p < 3) throw new IllegalArgumentException("truncated TLV");
            int type = h[p] & 0xff;
            int l = ((h[p + 1] & 0xff) << 8) | (h[p + 2] & 0xff);
            if (h.length - p - 3 < l) throw new IllegalArgumentException("truncated TLV");
            if (type == TLV_TYPE) {
                if (value != null) throw new IllegalArgumentException("duplicate signature TLV");
                value = Arrays.copyOfRange(h, p + 3, p + 3 + l);
            }
            p += 3 + l;
        }
        if (value == null || value.length != VALUE_LEN || value[0] != VERSION) {
            throw new GeneralSecurityException("missing or invalid signature TLV");
        }
        long ts = ByteBuffer.wrap(value, 1, 8).getLong();
        byte[] nonce = Arrays.copyOfRange(value, 9, 9 + NONCE_LEN);
        byte[] got = Arrays.copyOfRange(value, 9 + NONCE_LEN, VALUE_LEN);
        if (!MessageDigest.isEqual(got, mac(key, verCmd, fam, addr, ts, nonce))) {
            throw new GeneralSecurityException("bad signature");
        }
        if (Math.abs(nowSeconds - ts) > MAX_SKEW_SECONDS) {
            throw new GeneralSecurityException("timestamp outside allowed clock skew (" + (nowSeconds - ts) + "s)");
        }
        if (nonces != null && nonces.seen(nonce)) throw new GeneralSecurityException("replayed header");

        if (!res.local) {
            int ipLen = fam == FAM_TCP4 ? 4 : 16;
            try {
                InetAddress s = InetAddress.getByAddress(Arrays.copyOfRange(addr, 0, ipLen));
                InetAddress d = InetAddress.getByAddress(Arrays.copyOfRange(addr, ipLen, 2 * ipLen));
                ByteBuffer ports = ByteBuffer.wrap(addr, 2 * ipLen, 4);
                res.src = new InetSocketAddress(s, ports.getShort() & 0xffff);
                res.dst = new InetSocketAddress(d, ports.getShort() & 0xffff);
            } catch (java.net.UnknownHostException e) {
                throw new IllegalArgumentException("bad address block");
            }
        }
        return res;
    }
}
