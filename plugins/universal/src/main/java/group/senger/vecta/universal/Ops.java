package group.senger.vecta.universal;

import java.io.File;
import java.io.FileInputStream;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.util.Collections;
import java.util.HashSet;
import java.util.List;
import java.util.Locale;
import java.util.Set;

/** Reads the server's operator list from ops.json (vanilla, Paper, Fabric and Forge all use it). */
final class Ops {
    private final File file;
    private Set<String> names = Collections.emptySet();
    private long readAt;
    private long size = -1;
    private long modified = -1;

    Ops(File serverDir) {
        this.file = new File(serverDir, "ops.json");
    }

    /** Whether name is an operator. Re-reads ops.json at most every few seconds, or when it changes. */
    synchronized boolean isOp(String name) {
        long now = System.currentTimeMillis();
        if (now - readAt > 5000 || file.length() != size || file.lastModified() != modified) {
            reload();
            readAt = now;
        }
        return names.contains(name.toLowerCase(Locale.ROOT));
    }

    private void reload() {
        size = file.length();
        modified = file.lastModified();
        Set<String> out = new HashSet<String>();
        if (file.isFile()) {
            try {
                InputStream in = new FileInputStream(file);
                try {
                    byte[] raw = ModScanner.readAll(in, 1 << 20);
                    List<Object> list = Json.list(Json.parse(new String(raw, StandardCharsets.UTF_8)));
                    for (Object o : list) {
                        String n = Json.str(Json.obj(o).get("name"));
                        if (!n.isEmpty()) out.add(n.toLowerCase(Locale.ROOT));
                    }
                } finally {
                    in.close();
                }
            } catch (Exception e) {
                Log.debug("could not read ops.json: " + e);
            }
        }
        names = out;
    }
}
