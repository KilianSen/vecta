package group.senger.anymcp.universal;

import java.util.ArrayList;
import java.util.Collection;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Small JSON reader and writer. The reader is lenient because mod metadata in the wild is: it
 * accepts comments, trailing commas and raw control characters inside strings.
 */
final class Json {
    private static final int MAX_DEPTH = 128;
    private final String s;
    private int i;
    private int depth;

    private Json(String s) {
        this.s = s;
    }

    static Object parse(String text) {
        Json p = new Json(text);
        p.ws();
        Object v = p.value();
        p.ws();
        if (p.i < p.s.length()) throw p.error("trailing data");
        return v;
    }

    private IllegalArgumentException error(String msg) {
        return new IllegalArgumentException("json: " + msg + " at offset " + i);
    }

    private void ws() {
        while (i < s.length()) {
            char c = s.charAt(i);
            if (c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '﻿') {
                i++;
            } else if (c == '/' && i + 1 < s.length() && s.charAt(i + 1) == '/') {
                while (i < s.length() && s.charAt(i) != '\n') i++;
            } else if (c == '/' && i + 1 < s.length() && s.charAt(i + 1) == '*') {
                int end = s.indexOf("*/", i + 2);
                i = end < 0 ? s.length() : end + 2;
            } else {
                return;
            }
        }
    }

    private Object value() {
        if (i >= s.length()) throw error("unexpected end");
        switch (s.charAt(i)) {
            case '{':
                return object();
            case '[':
                return array();
            case '"':
                return string();
            case 't':
                return literal("true", Boolean.TRUE);
            case 'f':
                return literal("false", Boolean.FALSE);
            case 'n':
                return literal("null", null);
            default:
                return number();
        }
    }

    private Object literal(String word, Object v) {
        if (!s.startsWith(word, i)) throw error("unexpected character");
        i += word.length();
        return v;
    }

    private Map<String, Object> object() {
        if (++depth > MAX_DEPTH) throw error("nesting too deep");
        Map<String, Object> m = new LinkedHashMap<String, Object>();
        i++;
        while (true) {
            ws();
            if (i >= s.length()) throw error("unterminated object");
            if (s.charAt(i) == '}') {
                i++;
                break;
            }
            if (s.charAt(i) != '"') throw error("expected a key");
            String key = string();
            ws();
            if (i >= s.length() || s.charAt(i) != ':') throw error("expected ':'");
            i++;
            ws();
            m.put(key, value());
            ws();
            if (i < s.length() && s.charAt(i) == ',') {
                i++;
                continue;
            }
            if (i < s.length() && s.charAt(i) == '}') {
                i++;
                break;
            }
            throw error("expected ',' or '}'");
        }
        depth--;
        return m;
    }

    private List<Object> array() {
        if (++depth > MAX_DEPTH) throw error("nesting too deep");
        List<Object> list = new ArrayList<Object>();
        i++;
        while (true) {
            ws();
            if (i >= s.length()) throw error("unterminated array");
            if (s.charAt(i) == ']') {
                i++;
                break;
            }
            list.add(value());
            ws();
            if (i < s.length() && s.charAt(i) == ',') {
                i++;
                continue;
            }
            if (i < s.length() && s.charAt(i) == ']') {
                i++;
                break;
            }
            throw error("expected ',' or ']'");
        }
        depth--;
        return list;
    }

    private String string() {
        i++;
        StringBuilder b = new StringBuilder();
        while (true) {
            if (i >= s.length()) throw error("unterminated string");
            char c = s.charAt(i++);
            if (c == '"') return b.toString();
            if (c != '\\') {
                b.append(c);
                continue;
            }
            if (i >= s.length()) throw error("unterminated escape");
            char e = s.charAt(i++);
            switch (e) {
                case 'n':
                    b.append('\n');
                    break;
                case 't':
                    b.append('\t');
                    break;
                case 'r':
                    b.append('\r');
                    break;
                case 'b':
                    b.append('\b');
                    break;
                case 'f':
                    b.append('\f');
                    break;
                case 'u':
                    if (i + 4 > s.length()) throw error("bad unicode escape");
                    try {
                        b.append((char) Integer.parseInt(s.substring(i, i + 4), 16));
                    } catch (NumberFormatException ex) {
                        throw error("bad unicode escape");
                    }
                    i += 4;
                    break;
                default:
                    b.append(e);
            }
        }
    }

    private Object number() {
        int start = i;
        while (i < s.length() && "+-0123456789.eE".indexOf(s.charAt(i)) >= 0) i++;
        if (start == i) throw error("unexpected character");
        String n = s.substring(start, i);
        try {
            return Long.parseLong(n);
        } catch (NumberFormatException e) {
            try {
                return Double.parseDouble(n);
            } catch (NumberFormatException e2) {
                throw error("bad number");
            }
        }
    }

    // --- navigation helpers (null-safe) ---

    @SuppressWarnings("unchecked")
    static Map<String, Object> obj(Object o) {
        return o instanceof Map ? (Map<String, Object>) o : Collections.<String, Object>emptyMap();
    }

    @SuppressWarnings("unchecked")
    static List<Object> list(Object o) {
        return o instanceof List ? (List<Object>) o : Collections.emptyList();
    }

    static String str(Object o) {
        return o instanceof String ? (String) o : o == null ? "" : String.valueOf(o);
    }

    static int integer(Object o, int def) {
        return o instanceof Number ? ((Number) o).intValue() : def;
    }

    static Object path(Object root, String... keys) {
        Object cur = root;
        for (String k : keys) cur = obj(cur).get(k);
        return cur;
    }

    // --- writer ---

    static String write(Object v) {
        StringBuilder b = new StringBuilder();
        write(b, v);
        return b.toString();
    }

    private static void write(StringBuilder b, Object v) {
        if (v == null) {
            b.append("null");
        } else if (v instanceof String) {
            quote(b, (String) v);
        } else if (v instanceof Number || v instanceof Boolean) {
            b.append(v);
        } else if (v instanceof Map) {
            b.append('{');
            boolean first = true;
            for (Map.Entry<?, ?> e : ((Map<?, ?>) v).entrySet()) {
                if (!first) b.append(',');
                first = false;
                quote(b, String.valueOf(e.getKey()));
                b.append(':');
                write(b, e.getValue());
            }
            b.append('}');
        } else if (v instanceof Collection) {
            b.append('[');
            boolean first = true;
            for (Object o : (Collection<?>) v) {
                if (!first) b.append(',');
                first = false;
                write(b, o);
            }
            b.append(']');
        } else {
            quote(b, v.toString());
        }
    }

    private static void quote(StringBuilder b, String s) {
        b.append('"');
        for (int k = 0; k < s.length(); k++) {
            char c = s.charAt(k);
            switch (c) {
                case '"':
                    b.append("\\\"");
                    break;
                case '\\':
                    b.append("\\\\");
                    break;
                case '\n':
                    b.append("\\n");
                    break;
                case '\r':
                    b.append("\\r");
                    break;
                case '\t':
                    b.append("\\t");
                    break;
                default:
                    if (c < 0x20) {
                        b.append(String.format("\\u%04x", (int) c));
                    } else {
                        b.append(c);
                    }
            }
        }
        b.append('"');
    }
}
