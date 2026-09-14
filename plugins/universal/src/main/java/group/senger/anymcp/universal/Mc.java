package group.senger.anymcp.universal;

import java.io.ByteArrayOutputStream;
import java.io.EOFException;
import java.io.IOException;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;

/** Minecraft protocol primitives: VarInts, strings and length-prefixed frames (uncompressed). */
final class Mc {
    private Mc() {
    }

    static int readVarInt(InputStream in) throws IOException {
        int value = 0;
        for (int k = 0; k < 5; k++) {
            int b = in.read();
            if (b < 0) throw new EOFException();
            value |= (b & 0x7f) << (7 * k);
            if ((b & 0x80) == 0) return value;
        }
        throw new IOException("VarInt too long");
    }

    static void writeVarInt(ByteArrayOutputStream out, int v) {
        while ((v & ~0x7f) != 0) {
            out.write((v & 0x7f) | 0x80);
            v >>>= 7;
        }
        out.write(v);
    }

    static String readString(InputStream in, int maxBytes) throws IOException {
        int len = readVarInt(in);
        if (len < 0 || len > maxBytes) throw new IOException("string too long");
        byte[] b = new byte[len];
        readFully(in, b);
        return new String(b, StandardCharsets.UTF_8);
    }

    static void writeString(ByteArrayOutputStream out, String s) {
        byte[] b = s.getBytes(StandardCharsets.UTF_8);
        writeVarInt(out, b.length);
        out.write(b, 0, b.length);
    }

    static byte[] frame(byte[] payload) {
        ByteArrayOutputStream out = new ByteArrayOutputStream(payload.length + 5);
        writeVarInt(out, payload.length);
        out.write(payload, 0, payload.length);
        return out.toByteArray();
    }

    static byte[] readFrame(InputStream in, int max) throws IOException {
        int len = readVarInt(in);
        if (len < 0 || len > max) throw new IOException("frame too long");
        byte[] b = new byte[len];
        readFully(in, b);
        return b;
    }

    static void readFully(InputStream in, byte[] b) throws IOException {
        for (int off = 0; off < b.length; ) {
            int n = in.read(b, off, b.length - off);
            if (n < 0) throw new EOFException();
            off += n;
        }
    }
}
