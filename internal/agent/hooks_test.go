package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vecta/internal/registry"
)

// TestHookHelper is the fake callback: the test binary re-runs itself and
// appends the side port variables to $HOOK_LOG.
func TestHookHelper(t *testing.T) {
	logFile := os.Getenv("HOOK_LOG")
	if logFile == "" {
		t.Skip("helper process only")
	}
	var vars []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "VECTA_") {
			vars = append(vars, kv)
		}
	}
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		os.Exit(3)
	}
	json.NewEncoder(f).Encode(vars)
	f.Close()
	if os.Getenv("HOOK_FAIL") != "" {
		os.Exit(1)
	}
}

func hookArgv() []string { return []string{os.Args[0], "-test.run=^TestHookHelper$"} }

func readHookLog(t *testing.T, path string) []map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	dec := json.NewDecoder(strings.NewReader(string(b)))
	for dec.More() {
		var vars []string
		if err := dec.Decode(&vars); err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, kv := range vars {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		out = append(out, m)
	}
	return out
}

func assigned(name, proto string, backend int, host string, port int) registry.SidePort {
	return registry.SidePort{Name: name, Protocol: proto, Port: backend, Public: &registry.PublicAddr{Host: host, Port: port}}
}

func TestHooksApply(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "hook.log")
	t.Setenv("HOOK_LOG", logFile)
	state := filepath.Join(dir, "state", "sideports.json")
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()
	voice := registry.SidePort{Name: "voice", Protocol: "udp", Port: 24454}
	declared := []registry.SidePort{voice}

	h := newHooks(hookArgv(), state, "survival", log)
	h.apply(ctx, declared, []registry.SidePort{assigned("voice", "udp", 24454, "play.test", 24500)})
	runs := readHookLog(t, logFile)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	want := map[string]string{
		"VECTA_SERVER_ID": "survival", "VECTA_SIDEPORT_NAME": "voice", "VECTA_SIDEPORT_PROTOCOL": "udp",
		"VECTA_SIDEPORT_BACKEND_PORT": "24454", "VECTA_SIDEPORT_STATE": "assigned",
		"VECTA_SIDEPORT_HOST": "play.test", "VECTA_SIDEPORT_PORT": "24500", "VECTA_SIDEPORT_ERROR": "",
	}
	for k, v := range want {
		if runs[0][k] != v {
			t.Errorf("%s = %q, want %q", k, runs[0][k], v)
		}
	}

	// Same answer: no run. A restarted agent loads the state and also skips.
	h.apply(ctx, declared, []registry.SidePort{assigned("voice", "udp", 24454, "play.test", 24500)})
	h = newHooks(hookArgv(), state, "survival", log)
	h.apply(ctx, declared, []registry.SidePort{assigned("voice", "udp", 24454, "play.test", 24500)})
	if n := len(readHookLog(t, logFile)); n != 1 {
		t.Fatalf("unchanged state re-ran the hook: %d runs", n)
	}
	ports := []registry.SidePort{voice}
	h.prefer(ports)
	if ports[0].PreferredPort != 24500 {
		t.Fatalf("preferredPort = %d", ports[0].PreferredPort)
	}

	// A new port re-runs it; a failing hook is retried on the next answer.
	t.Setenv("HOOK_FAIL", "1")
	h.apply(ctx, declared, []registry.SidePort{assigned("voice", "udp", 24454, "play.test", 24501)})
	t.Setenv("HOOK_FAIL", "")
	h.apply(ctx, declared, []registry.SidePort{assigned("voice", "udp", 24454, "play.test", 24501)})
	h.apply(ctx, declared, []registry.SidePort{assigned("voice", "udp", 24454, "play.test", 24501)})
	runs = readHookLog(t, logFile)
	if len(runs) != 3 || runs[2]["VECTA_SIDEPORT_PORT"] != "24501" {
		t.Fatalf("runs after change = %v", runs)
	}

	// Errors are passed on.
	h.apply(ctx, declared, []registry.SidePort{{Name: "voice", Protocol: "udp", Port: 24454, Error: "no free public port"}})
	runs = readHookLog(t, logFile)
	if last := runs[len(runs)-1]; last["VECTA_SIDEPORT_STATE"] != "error" || last["VECTA_SIDEPORT_ERROR"] != "no free public port" || last["VECTA_SIDEPORT_PORT"] != "" {
		t.Fatalf("error run = %v", last)
	}

	// Removing the side port from the config releases it once.
	h.apply(ctx, nil, nil)
	h.apply(ctx, nil, nil)
	runs = readHookLog(t, logFile)
	if len(runs) != 5 || runs[4]["VECTA_SIDEPORT_STATE"] != "released" || runs[4]["VECTA_SIDEPORT_NAME"] != "voice" {
		t.Fatalf("release runs = %v", runs)
	}
	if b, _ := os.ReadFile(state); strings.TrimSpace(string(b)) != "{}" {
		t.Fatalf("state after release = %s", b)
	}
}

func TestRunReportsSidePorts(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "hook.log")
	t.Setenv("HOOK_LOG", logFile)
	state := filepath.Join(dir, "sideports.json")
	os.WriteFile(state, []byte(`{"voice":{"state":"assigned","protocol":"udp","backendPort":24454,"host":"play.test","port":24510}}`), 0o644)

	var mu sync.Mutex
	var got []registry.Server
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var s registry.Server
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &s)
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
		for i := range s.SidePorts {
			s.SidePorts[i].Public = &registry.PublicAddr{Host: "play.test", Port: 24511}
		}
		json.NewEncoder(w).Encode(s)
	}))
	defer gw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error)
	go func() {
		done <- Run(ctx, Config{
			Gateway: gw.URL, Token: "t", Interval: time.Hour, StateFile: state, SidePortHook: hookArgv(),
			Server: registry.Server{ID: "survival", Address: "10.0.0.5:25565", SidePorts: []registry.SidePort{{Name: "voice", Protocol: "udp", Port: 24454}}},
		}, slog.New(slog.DiscardHandler))
	}()
	for {
		if runs := readHookLog(t, logFile); len(runs) == 1 {
			if runs[0]["VECTA_SIDEPORT_PORT"] != "24511" {
				t.Fatalf("hook got %v", runs[0])
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("hook never ran")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if got[0].SidePorts[0].PreferredPort != 24510 {
		t.Fatalf("preferredPort sent = %d", got[0].SidePorts[0].PreferredPort)
	}
}
