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

	var ag agentConfig
	if err := loadJSON(repoFile(t, "examples", "agent.json"), &ag); err != nil {
		t.Fatalf("examples/agent.json: %v", err)
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
	t.Setenv("ANYMCP_T_DOMAIN", "play.example.com")
	t.Setenv("ANYMCP_T_QUOTE", `se"cr\et`)
	t.Setenv("ANYMCP_T_EMPTY", "")
	raw := []byte(`{"domain":"${ANYMCP_T_DOMAIN}","cookieSecret":"${ANYMCP_T_QUOTE}",` +
		`"motd":"${ANYMCP_T_UNSET:-Welcome}","transferHost":"${ANYMCP_T_EMPTY:-fallback}","listen":"$${literal}"}`)
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
	if _, err := expandEnv([]byte(`{"token":"${ANYMCP_T_MISSING}"}`)); err == nil || !strings.Contains(err.Error(), "ANYMCP_T_MISSING") {
		t.Fatalf("missing variable not reported: %v", err)
	}
}

func TestDeployTemplatesParse(t *testing.T) {
	for k, v := range map[string]string{
		"ANYMCP_DOMAIN": "play.example.com", "ANYMCP_COOKIE_SECRET": "s", "ANYMCP_METRICS_TOKEN": "m",
		"ANYMCP_OWNER_TOKEN": "o", "ANYMCP_GATEWAY_URL": "https://mc-api.example.com", "ANYMCP_TOKEN": "t",
		"ANYMCP_SERVER_ADDRESS": "10.0.0.5:25565",
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
	var ag agentConfig
	if err := loadJSON(repoFile(t, "deploy", "agent.json"), &ag); err != nil {
		t.Fatalf("deploy/agent.json: %v", err)
	}
	if ag.Server.Address != "10.0.0.5:25565" || ag.Token != "t" {
		t.Fatalf("deploy agent config: %+v", ag)
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
