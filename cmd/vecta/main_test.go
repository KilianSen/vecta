package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func repoFile(t *testing.T, parts ...string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(here), "..", ".."}, parts...)...)
}

// The documented example configs must keep loading with unknown-field checks.
func TestExampleConfigsParse(t *testing.T) {
	var gw gatewayConfig
	if err := loadJSON(repoFile(t, "examples", "gateway.json"), &gw); err != nil {
		t.Fatalf("examples/gateway.json: %v", err)
	}
	if gw.Owners["alice"].Token == "" || gw.Owners["bob"].Token == "" || len(gw.Owners["bob"].AllowedNetworks) != 1 {
		t.Fatalf("owners parsed wrong: %+v", gw.Owners)
	}
	if time.Duration(gw.StickyTTL) != 720*time.Hour || gw.RateLimit.PerSecond != 2 || gw.MaxLobbySessions == nil {
		t.Fatalf("hardening options parsed wrong: %+v", gw)
	}

	if lo, hi, err := gw.SidePorts.portRange(); err != nil || lo != 24500 || hi != 24599 {
		t.Fatalf("sidePorts range: %d-%d %v", lo, hi, err)
	}

	var ag agentConfig
	if err := loadJSON(repoFile(t, "examples", "agent.json"), &ag); err != nil {
		t.Fatalf("examples/agent.json: %v", err)
	}
	if len(ag.Server.SidePorts) != 2 || len(ag.SidePortHook) != 1 {
		t.Fatalf("agent side ports parsed wrong: %+v", ag)
	}

	var e2e gatewayConfig
	if err := loadJSON(repoFile(t, "test", "e2e", "gateway.json"), &e2e); err != nil {
		t.Fatalf("test/e2e/gateway.json: %v", err)
	}
	if time.Duration(e2e.StickyTTL) >= 0 {
		t.Fatal("e2e config must disable sticky routing (bot usernames are reused)")
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("VECTA_T_DOMAIN", "play.example.com")
	t.Setenv("VECTA_T_QUOTE", `se"cr\et`)
	t.Setenv("VECTA_T_EMPTY", "")
	raw := []byte(`{"domain":"${VECTA_T_DOMAIN}","cookieSecret":"${VECTA_T_QUOTE}",` +
		`"motd":"${VECTA_T_UNSET:-Welcome}","transferHost":"${VECTA_T_EMPTY:-fallback}","listen":"$${literal}"}`)
	out, err := expandEnv(raw)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Domain, CookieSecret, MOTD, TransferHost, Listen string
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("expanded config is not valid JSON: %v\n%s", err, out)
	}
	if cfg.Domain != "play.example.com" || cfg.CookieSecret != `se"cr\et` || cfg.MOTD != "Welcome" ||
		cfg.TransferHost != "fallback" || cfg.Listen != "${literal}" {
		t.Fatalf("expanded %+v", cfg)
	}
	if _, err := expandEnv([]byte(`{"token":"${VECTA_T_MISSING}"}`)); err == nil || !strings.Contains(err.Error(), "VECTA_T_MISSING") {
		t.Fatalf("missing variable not reported: %v", err)
	}
}

func TestDeployTemplatesParse(t *testing.T) {
	for k, v := range map[string]string{
		"VECTA_DOMAIN": "play.example.com", "VECTA_COOKIE_SECRET": "s", "VECTA_METRICS_TOKEN": "m",
		"VECTA_OWNER_TOKEN": "o", "VECTA_GATEWAY_URL": "https://mc-api.example.com", "VECTA_TOKEN": "t",
		"VECTA_SERVER_ADDRESS": "10.0.0.5:25565",
	} {
		t.Setenv(k, v)
	}
	var gw gatewayConfig
	if err := loadJSON(repoFile(t, "deploy", "gateway.json"), &gw); err != nil {
		t.Fatalf("deploy/gateway.json: %v", err)
	}
	if gw.Domain != "play.example.com" || gw.Owners["admin"].Token != "o" || gw.DataFile != "/data/state.json" {
		t.Fatalf("deploy gateway config: %+v", gw)
	}
	if lo, hi, err := gw.SidePorts.portRange(); err != nil || hi != 0 {
		t.Fatalf("side ports must default to disabled: %d-%d %v", lo, hi, err)
	}
	var ag agentConfig
	if err := loadJSON(repoFile(t, "deploy", "agent.json"), &ag); err != nil {
		t.Fatalf("deploy/agent.json: %v", err)
	}
	if ag.Server.Address != "10.0.0.5:25565" || ag.Token != "t" {
		t.Fatalf("deploy agent config: %+v", ag)
	}
}

func TestSidePortRange(t *testing.T) {
	for in, want := range map[string][2]int{"": {0, 0}, " ": {0, 0}, "24500-24599": {24500, 24599}, "30000": {30000, 30000}, " 1 - 2 ": {1, 2}} {
		lo, hi, err := sidePortsConfig{Range: in}.portRange()
		if err != nil || lo != want[0] || hi != want[1] {
			t.Errorf("%q: %d-%d %v", in, lo, hi, err)
		}
	}
	for _, in := range []string{"0-10", "10-5", "1-70000", "a-b", "-"} {
		if _, _, err := (sidePortsConfig{Range: in}).portRange(); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	if code := healthcheck([]string{"-url", ok.URL}); code != 0 {
		t.Fatalf("healthy endpoint: exit %d", code)
	}
	if code := healthcheck([]string{"-url", bad.URL}); code != 1 {
		t.Fatalf("503 endpoint: exit %d", code)
	}
	if code := healthcheck([]string{"-url", "http://127.0.0.1:1/healthz", "-timeout", "500ms"}); code != 1 {
		t.Fatalf("closed port: exit %d", code)
	}
}

func TestOwnerConfigForms(t *testing.T) {
	var owners map[string]ownerConfig
	err := json.Unmarshal([]byte(`{"a":"tok","b":{"token":"t2","allowedNetworks":["10.0.0.0/8"]}}`), &owners)
	if err != nil || owners["a"].Token != "tok" || owners["b"].Token != "t2" || owners["b"].AllowedNetworks[0] != "10.0.0.0/8" {
		t.Fatalf("owners %+v err %v", owners, err)
	}
	if err := json.Unmarshal([]byte(`{"c":{"token":"x","typo":1}}`), &owners); err == nil {
		t.Fatal("unknown owner field accepted")
	}
}
