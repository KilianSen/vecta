package group.senger.vecta.universal;

import java.io.File;
import java.util.Locale;

/** Detects the server platform from files in the server directory, for when runtime checks can't tell. */
final class Platform {
    private Platform() {
    }

    static String detect(File dir) {
        if (exists(dir, "libraries/net/neoforged/neoforge") || exists(dir, "libraries/net/neoforged/forge")) return "neoforge";
        if (exists(dir, "libraries/net/minecraftforge/forge")) return "forge";
        if (exists(dir, ".quilt") || exists(dir, "quilt-server-launcher.properties")) return "quilt";
        if (exists(dir, ".fabric") || exists(dir, "fabric-server-launcher.properties")) return "fabric";
        if (exists(dir, "purpur.yml")) return "purpur";
        if (exists(dir, "config/paper-global.yml") || exists(dir, "paper.yml")) return "paper";
        if (exists(dir, "spigot.yml") || exists(dir, "bukkit.yml")) return "spigot";
        File[] jars = dir.listFiles();
        if (jars != null) {
            for (File f : jars) {
                String n = f.getName().toLowerCase(Locale.ROOT);
                if (!n.endsWith(".jar") || n.contains("installer")) continue;
                if (n.startsWith("neoforge-")) return "neoforge";
                if (n.startsWith("forge-") || n.startsWith("minecraftforge-")) return "forge";
                if (n.startsWith("fabric-server")) return "fabric";
                if (n.startsWith("quilt-server")) return "quilt";
            }
        }
        return "";
    }

    private static boolean exists(File dir, String path) {
        return new File(dir, path).exists();
    }
}
