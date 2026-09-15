package group.senger.vecta.universal;

import java.io.File;
import java.lang.instrument.Instrumentation;
import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Method;
import java.net.URL;
import java.net.URLClassLoader;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Locale;
import java.util.jar.JarFile;
import java.util.jar.Manifest;

/**
 * Wrapper mode: {@code java -jar vecta.jar [server.jar] [server args]} starts vecta and then the
 * real server jar in the same JVM.
 */
public final class Launcher {
    /** Server jar names in order of preference, matched by prefix. */
    private static final List<String> SERVER_JARS = Arrays.asList(
            "fabric-server", "quilt-server", "neoforge-", "forge-", "minecraftforge-",
            "paper", "purpur", "folia", "pufferfish", "spigot", "craftbukkit", "minecraft_server", "server");

    private Launcher() {
    }

    public static void main(String[] args) throws Throwable {
        Config cfg = Config.load(null);
        List<String> rest = new ArrayList<String>(Arrays.asList(args));
        File jar;
        if (!rest.isEmpty() && rest.get(0).toLowerCase(Locale.ROOT).endsWith(".jar")) {
            jar = new File(rest.remove(0));
        } else if (!cfg.serverJar.isEmpty()) {
            jar = new File(cfg.serverJar);
        } else {
            jar = detectServerJar(cfg.serverDir);
        }
        if (jar == null) {
            fail("no server jar found. Run java -jar vecta.jar <server.jar> [args], set serverJar in " + cfg.file
                    + ", or — if the server is started by a script or launcher — add -javaagent:vecta.jar to that "
                    + "server's JVM instead of wrapping it.");
        }
        if (!jar.isAbsolute()) jar = new File(cfg.serverDir, jar.getPath());
        if (!jar.isFile()) fail("server jar " + jar + " does not exist");

        String mainClass = null;
        JarFile jf = new JarFile(jar);
        try {
            Manifest mf = jf.getManifest();
            if (mf != null) mainClass = mf.getMainAttributes().getValue("Main-Class");
        } finally {
            jf.close();
        }
        if (mainClass == null) fail(jar.getName() + " has no Main-Class; start it with -javaagent:vecta.jar instead");

        addToClassPath(jar);
        String cp = System.getProperty("java.class.path", "");
        System.setProperty("java.class.path", jar.getPath() + (cp.isEmpty() ? "" : File.pathSeparator + cp));

        Vecta.start(cfg, "wrapper");
        Log.info("starting " + jar.getName() + " (" + mainClass + ")");
        ClassLoader system = ClassLoader.getSystemClassLoader();
        Thread.currentThread().setContextClassLoader(system);
        Method main = Class.forName(mainClass, true, system).getMethod("main", String[].class);
        try {
            main.invoke(null, (Object) rest.toArray(new String[0]));
        } catch (InvocationTargetException e) {
            throw e.getCause();
        }
    }

    static File detectServerJar(File dir) {
        File self = self();
        File[] files = dir.listFiles();
        if (files == null) return null;
        List<File> jars = new ArrayList<File>();
        for (File f : files) {
            String n = f.getName().toLowerCase(Locale.ROOT);
            if (!f.isFile() || !n.endsWith(".jar") || n.contains("installer") || n.startsWith("vecta")) continue;
            if (self != null && f.getAbsoluteFile().equals(self.getAbsoluteFile())) continue;
            jars.add(f);
        }
        for (String prefix : SERVER_JARS) {
            for (File f : jars) {
                if (f.getName().toLowerCase(Locale.ROOT).startsWith(prefix)) return f;
            }
        }
        return jars.size() == 1 ? jars.get(0) : null;
    }

    private static File self() {
        try {
            return new File(Launcher.class.getProtectionDomain().getCodeSource().getLocation().toURI());
        } catch (Exception e) {
            return null;
        }
    }

    private static void addToClassPath(File jar) throws Exception {
        Instrumentation inst = Vecta.instrumentation();
        if (inst != null) {
            inst.appendToSystemClassLoaderSearch(new JarFile(jar));
            return;
        }
        ClassLoader system = ClassLoader.getSystemClassLoader();
        if (system instanceof URLClassLoader) { // Java 8
            Method add = URLClassLoader.class.getDeclaredMethod("addURL", URL.class);
            add.setAccessible(true);
            add.invoke(system, jar.toURI().toURL());
            return;
        }
        fail("cannot add " + jar.getName() + " to the class path; start with java -javaagent:vecta.jar -jar " + jar.getName());
    }

    private static void fail(String msg) {
        System.err.println("[vecta] " + msg);
        System.exit(2);
    }
}
