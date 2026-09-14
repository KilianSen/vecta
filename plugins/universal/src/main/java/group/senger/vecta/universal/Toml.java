package group.senger.vecta.universal;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Just enough TOML for mods.toml files: [tables], [[arrays of tables]] and key = value pairs with
 * strings, booleans and numbers. Arrays and inline tables are skipped and stored as null.
 */
final class Toml {
    final Map<String, Object> root = new LinkedHashMap<String, Object>();
    final Map<String, Map<String, Object>> tables = new LinkedHashMap<String, Map<String, Object>>();
    final Map<String, List<Map<String, Object>>> tableArrays = new LinkedHashMap<String, List<Map<String, Object>>>();

    private final String s;
    private int i;

    private Toml(String s) {
        this.s = s;
    }

    static Toml parse(String text) {
        Toml t = new Toml(text);
        t.run();
        return t;
    }

    private void run() {
        Map<String, Object> current = root;
        while (i < s.length()) {
            char c = s.charAt(i);
            if (c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '﻿') {
                i++;
            } else if (c == '#') {
                skipLine();
            } else if (c == '[') {
                boolean array = i + 1 < s.length() && s.charAt(i + 1) == '[';
                int start = i + (array ? 2 : 1);
                int end = s.indexOf(array ? "]]" : "]", start);
                int eol = lineEnd(start);
                if (end < 0 || end > eol) {
                    skipLine();
                    continue;
                }
                String name = unquoteKey(s.substring(start, end));
                i = end + (array ? 2 : 1);
                skipLine();
                Map<String, Object> table = new LinkedHashMap<String, Object>();
                if (array) {
                    List<Map<String, Object>> list = tableArrays.get(name);
                    if (list == null) {
                        list = new ArrayList<Map<String, Object>>();
                        tableArrays.put(name, list);
                    }
                    list.add(table);
                } else {
                    tables.put(name, table);
                }
                current = table;
            } else {
                int eq = s.indexOf('=', i);
                int eol = lineEnd(i);
                if (eq < 0 || eq > eol) {
                    skipLine();
                    continue;
                }
                String key = unquoteKey(s.substring(i, eq));
                i = eq + 1;
                Object value = value();
                if (!key.isEmpty()) current.put(key, value);
                skipLine();
            }
        }
    }

    private int lineEnd(int from) {
        int e = s.indexOf('\n', from);
        return e < 0 ? s.length() : e;
    }

    private void skipLine() {
        i = lineEnd(i);
        if (i < s.length()) i++;
    }

    private static String unquoteKey(String k) {
        k = k.trim();
        if (k.length() >= 2 && (k.charAt(0) == '"' || k.charAt(0) == '\'') && k.charAt(k.length() - 1) == k.charAt(0)) {
            return k.substring(1, k.length() - 1);
        }
        return k;
    }

    private Object value() {
        while (i < s.length() && (s.charAt(i) == ' ' || s.charAt(i) == '\t')) i++;
        if (i >= s.length()) return null;
        if (s.startsWith("\"\"\"", i)) return multiline("\"\"\"", true);
        if (s.startsWith("'''", i)) return multiline("'''", false);
        char c = s.charAt(i);
        if (c == '"') return basicString();
        if (c == '\'') {
            int end = s.indexOf('\'', i + 1);
            int eol = lineEnd(i);
            if (end < 0 || end > eol) return null;
            String v = s.substring(i + 1, end);
            i = end + 1;
            return v;
        }
        if (c == '[' || c == '{') {
            skipBalanced();
            return null;
        }
        int end = i;
        while (end < s.length() && " \t\r\n#".indexOf(s.charAt(end)) < 0) end++;
        String token = s.substring(i, end);
        i = end;
        if (token.equals("true")) return Boolean.TRUE;
        if (token.equals("false")) return Boolean.FALSE;
        try {
            return Long.parseLong(token.replace("_", ""));
        } catch (NumberFormatException e) {
            return token;
        }
    }

    private String multiline(String delim, boolean escapes) {
        int start = i + 3;
        if (start < s.length() && s.charAt(start) == '\n') start++;
        else if (start + 1 < s.length() && s.charAt(start) == '\r' && s.charAt(start + 1) == '\n') start += 2;
        int end = s.indexOf(delim, start);
        if (end < 0) end = s.length();
        String raw = s.substring(start, end);
        i = Math.min(s.length(), end + 3);
        return escapes ? unescape(raw) : raw;
    }

    private String basicString() {
        StringBuilder raw = new StringBuilder();
        i++;
        while (i < s.length()) {
            char c = s.charAt(i++);
            if (c == '"') break;
            if (c == '\n') break;
            if (c == '\\' && i < s.length()) {
                raw.append(c).append(s.charAt(i++));
            } else {
                raw.append(c);
            }
        }
        return unescape(raw.toString());
    }

    private static String unescape(String raw) {
        if (raw.indexOf('\\') < 0) return raw;
        StringBuilder b = new StringBuilder();
        for (int k = 0; k < raw.length(); k++) {
            char c = raw.charAt(k);
            if (c != '\\' || k + 1 >= raw.length()) {
                b.append(c);
                continue;
            }
            char e = raw.charAt(++k);
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
                case 'u':
                    if (k + 4 < raw.length()) {
                        try {
                            b.append((char) Integer.parseInt(raw.substring(k + 1, k + 5), 16));
                            k += 4;
                            break;
                        } catch (NumberFormatException ignored) {
                            // fall through to the literal character
                        }
                    }
                    b.append(e);
                    break;
                default:
                    b.append(e);
            }
        }
        return b.toString();
    }

    /** Skips an array or inline table, including nested ones and strings containing brackets. */
    private void skipBalanced() {
        int level = 0;
        while (i < s.length()) {
            char c = s.charAt(i);
            if (c == '"' || c == '\'') {
                if (s.startsWith("\"\"\"", i) || s.startsWith("'''", i)) {
                    String delim = s.substring(i, i + 3);
                    int end = s.indexOf(delim, i + 3);
                    i = end < 0 ? s.length() : end + 3;
                    continue;
                }
                int k = i + 1;
                while (k < s.length() && s.charAt(k) != c && s.charAt(k) != '\n') {
                    if (c == '"' && s.charAt(k) == '\\') k++;
                    k++;
                }
                i = k + 1;
                continue;
            }
            if (c == '#') {
                i = lineEnd(i);
                continue;
            }
            if (c == '[' || c == '{') level++;
            if (c == ']' || c == '}') {
                level--;
                if (level == 0) {
                    i++;
                    return;
                }
            }
            i++;
        }
    }
}
