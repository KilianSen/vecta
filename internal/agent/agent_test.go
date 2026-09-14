package agent

import (
	"archive/zip"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeJar(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestScanMods(t *testing.T) {
	dir := t.TempDir()
	// Layout copied from JEI 19.51 for NeoForge 1.21.1: header with trailing
	// comment, and a dependency table that must not be picked up.
	writeJar(t, filepath.Join(dir, "jei.jar"), map[string]string{"META-INF/neoforge.mods.toml": `
modLoader="javafml" #mandatory
[[mods]] #mandatory
# The modid of the mod
modId="jei" #mandatory
version="19.51.0.418" #mandatory
[[dependencies.jei]] #optional
    modId="neoforge" #mandatory
`})
	writeJar(t, filepath.Join(dir, "appleskin.jar"), map[string]string{"fabric.mod.json": `{"id":"appleskin","environment":"*"}`})
	writeJar(t, filepath.Join(dir, "q.jar"), map[string]string{"quilt.mod.json": `{"quilt_loader":{"id":"qsl"}}`})
	writeJar(t, filepath.Join(dir, "old.jar"), map[string]string{"mcmod.info": `[{"modid":"ic2"}]`})
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644)

	got, err := ScanMods(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"appleskin", "ic2", "jei", "qsl"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
