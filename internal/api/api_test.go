package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"anymcp/internal/metrics"
	"anymcp/internal/netguard"
	"anymcp/internal/registry"
	"anymcp/internal/router"
)

type fakeIssuer struct {
	got router.Ticket
	err error
	req []any
}

func (f *fakeIssuer) IssueTicket(player string, protocol int32, target string) (router.Ticket, error) {
	f.req = []any{player, protocol, target}
	return f.got, f.err
}

func setup(t *testing.T) (*httptest.Server, *registry.Registry, *fakeIssuer, *metrics.Registry) {
	t.Helper()
	reg := registry.New()
	if _, err := reg.Upsert("alice", registry.Server{ID: "mine", Address: "10.1.2.3:25565", MinProtocol: 767, MaxProtocol: 767}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Upsert("bob", registry.Server{ID: "bobs", Address: "10.1.2.4:25565"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	lan := netip.MustParsePrefix("192.168.0.0/16")
	issuer := &fakeIssuer{got: router.Ticket{Mode: "transfer", Host: "play.test", Port: 25565, CookieKey: "anymcp:route", Cookie: "Y29va2ll"}}
	m := metrics.New()
	m.Counter("anymcp_test_total", "test").Inc()
	srv := httptest.NewServer(New(Options{
		Registry: reg,
		Owners: map[string]Owner{
			"alice": {Token: "tok-a"},
			"bob":   {Token: "tok-b"},
			"lan":   {Token: "tok-lan", Policy: netguard.Policy{Allowed: []netip.Prefix{lan}}},
		},
		TTL:          time.Minute,
		Domain:       "play.test",
		Tickets:      issuer,
		Metrics:      m,
		MetricsToken: "scrape",
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	t.Cleanup(srv.Close)
	return srv, reg, issuer, m
}

func do(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRegisterAddressPolicy(t *testing.T) {
	srv, _, _, _ := setup(t)
	cases := []struct {
		token, id, address string
		want               int
	}{
		{"tok-a", "loop", "127.0.0.1:25565", http.StatusForbidden},
		{"tok-a", "meta", "169.254.169.254:80", http.StatusForbidden},
		{"tok-a", "lan1", "10.9.9.9:25565", http.StatusOK},
		{"tok-lan", "lan2", "10.9.9.9:25565", http.StatusForbidden},
		{"tok-lan", "lan3", "192.168.7.7:25565", http.StatusOK},
		{"nope", "x", "10.0.0.1:1", http.StatusUnauthorized},
	}
	for _, c := range cases {
		code, body := do(t, "PUT", srv.URL+"/api/v1/servers/"+c.id, c.token,
			`{"address":"`+c.address+`","loader":"paper","reporter":"plugin","channels":[{"name":"Create:Main","version":"1","required":true}]}`)
		if code != c.want {
			t.Errorf("%s %s: got %d %s, want %d", c.token, c.address, code, body, c.want)
		}
	}
}

func TestTransferTicket(t *testing.T) {
	srv, _, issuer, _ := setup(t)
	url := srv.URL + "/api/v1/servers/mine/transfer-ticket"
	good := `{"player":"Steve","protocol":767,"target":"survival"}`

	if code, _ := do(t, "POST", url, "", good); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := do(t, "POST", url, "tok-b", good); code != http.StatusForbidden {
		t.Fatalf("other owner: %d", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/api/v1/servers/ghost/transfer-ticket", "tok-a", good); code != http.StatusNotFound {
		t.Fatalf("unknown source: %d", code)
	}
	if code, _ := do(t, "POST", url, "tok-a", `{"player":"bad name!","protocol":767,"target":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("bad player: %d", code)
	}

	code, body := do(t, "POST", url, "tok-a", good)
	if code != http.StatusOK {
		t.Fatalf("success: %d %s", code, body)
	}
	var tk router.Ticket
	if err := json.Unmarshal([]byte(body), &tk); err != nil || tk.Mode != "transfer" || tk.CookieKey != "anymcp:route" {
		t.Fatalf("ticket %s (%v)", body, err)
	}
	if issuer.req[0] != "Steve" || issuer.req[1] != int32(767) || issuer.req[2] != "survival" {
		t.Fatalf("issuer got %v", issuer.req)
	}

	for err, want := range map[error]int{
		router.ErrIncompatible:    http.StatusConflict,
		router.ErrUnknownTarget:   http.StatusNotFound,
		router.ErrNoPublicAddress: http.StatusServiceUnavailable,
	} {
		issuer.err = err
		if code, _ := do(t, "POST", url, "tok-a", good); code != want {
			t.Errorf("%v: got %d want %d", err, code, want)
		}
	}
}

func TestEmptyListIsArray(t *testing.T) {
	srv := httptest.NewServer(New(Options{Registry: registry.New(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	defer srv.Close()
	code, body := do(t, "GET", srv.URL+"/api/v1/servers", "", "")
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("empty list: %d %q", code, body)
	}
}

func TestMetricsToken(t *testing.T) {
	srv, _, _, _ := setup(t)
	if code, _ := do(t, "GET", srv.URL+"/metrics", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("metrics without token: %d", code)
	}
	code, body := do(t, "GET", srv.URL+"/metrics", "scrape", "")
	if code != http.StatusOK || !strings.Contains(body, "anymcp_test_total 1") {
		t.Fatalf("metrics: %d %s", code, body)
	}
}
