// Command anymcp runs the Minecraft gateway or the backend registration agent.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"anymcp/internal/agent"
	"anymcp/internal/api"
	"anymcp/internal/guard"
	"anymcp/internal/metrics"
	"anymcp/internal/netguard"
	"anymcp/internal/registry"
	"anymcp/internal/router"
	"anymcp/internal/store"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

// Duration accepts "10s"-style strings or a number of seconds in JSON.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		v, err := time.ParseDuration(s)
		*d = Duration(v)
		return err
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\" or seconds")
	}
	*d = Duration(n * float64(time.Second))
	return nil
}

// ownerConfig is either a plain token string or {"token": ..., "allowedNetworks": [...]}.
type ownerConfig struct {
	Token           string   `json:"token"`
	AllowedNetworks []string `json:"allowedNetworks"`
}

func (o *ownerConfig) UnmarshalJSON(b []byte) error {
	var token string
	if json.Unmarshal(b, &token) == nil {
		o.Token = token
		return nil
	}
	type plain ownerConfig
	var p plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	*o = ownerConfig(p)
	return nil
}

type gatewayConfig struct {
	Listen              string   `json:"listen"`
	APIListen           string   `json:"apiListen"`
	Domain              string   `json:"domain"`
	TransferHost        string   `json:"transferHost"`
	TransferPort        int      `json:"transferPort"`
	CookieSecret        string   `json:"cookieSecret"`
	AcceptProxyProtocol bool     `json:"acceptProxyProtocol"`
	RegistrationTTL     Duration `json:"registrationTTL"`
	HealthInterval      Duration `json:"healthInterval"`
	AutoMinScore        int      `json:"autoMinScore"`
	AutoMargin          int      `json:"autoMargin"`
	ProbeWindow         Duration `json:"probeWindow"`
	PendingTTL          Duration `json:"pendingTTL"`
	ForgeModQuery       bool     `json:"forgeModQuery"`
	MOTD                string   `json:"motd"`

	// DataFile persists remembered routes and preferences ("-": memory only).
	DataFile string `json:"dataFile"`
	// StickyTTL remembers each player's last server (default 30 days,
	// negative disables).
	StickyTTL        Duration `json:"stickyTTL"`
	PersonalizedMOTD bool     `json:"personalizedMotd"`
	RateLimit        struct {
		PerSecond           float64 `json:"perSecond"`
		Burst               int     `json:"burst"`
		MaxConnectionsPerIP int     `json:"maxConnectionsPerIp"`
	} `json:"rateLimit"`
	MaxLobbySessions *int `json:"maxLobbySessions"`
	// AllowedNetworks limits owner-registered backend addresses (default:
	// anything except loopback, link-local, unspecified and multicast).
	AllowedNetworks       []string `json:"allowedNetworks"`
	AllowLoopbackBackends bool     `json:"allowLoopbackBackends"`
	MetricsToken          string   `json:"metricsToken"`

	Owners  map[string]ownerConfig `json:"owners"`
	Servers []registry.Server      `json:"servers"`
}

type agentConfig struct {
	Gateway  string          `json:"gateway"`
	Token    string          `json:"token"`
	Interval Duration        `json:"interval"`
	ModsDir  string          `json:"modsDir"`
	Server   registry.Server `json:"server"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("anymcp", version)
		return
	case "healthcheck":
		os.Exit(healthcheck(os.Args[2:]))
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	configPath := fs.String("config", os.Args[1]+".json", "path to JSON config")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(os.Args[2:])

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "gateway":
		var cfg gatewayConfig
		if err = loadJSON(*configPath, &cfg); err == nil {
			err = runGateway(ctx, cfg, log)
		}
	case "agent":
		var cfg agentConfig
		if err = loadJSON(*configPath, &cfg); err == nil {
			err = agent.Run(ctx, agent.Config{
				Gateway: cfg.Gateway, Token: cfg.Token, Interval: time.Duration(cfg.Interval),
				ModsDir: cfg.ModsDir, Server: cfg.Server,
			}, log)
		}
	default:
		usage()
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  anymcp gateway [-config gateway.json] [-v]
  anymcp agent [-config agent.json] [-v]
  anymcp healthcheck [-url http://127.0.0.1:8080/healthz]
  anymcp version`)
	os.Exit(2)
}

// healthcheck exits 0 when the gateway API answers its health endpoint. It
// serves as the container health check (the image has no shell or curl).
func healthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	def := os.Getenv("ANYMCP_HEALTH_URL")
	if def == "" {
		def = "http://127.0.0.1:8080/healthz"
	}
	url := fs.String("url", def, "health endpoint (default from ANYMCP_HEALTH_URL)")
	timeout := fs.Duration("timeout", 3*time.Second, "request timeout")
	fs.Parse(args)
	client := http.Client{Timeout: *timeout}
	resp, err := client.Get(*url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy: HTTP", resp.StatusCode)
		return 1
	}
	return 0
}

var envRef = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandEnv replaces ${VAR} and ${VAR:-default} in a config file with
// environment values, JSON-escaped so they are safe inside strings. $$ is a
// literal $. A variable that is unset and has no default is an error, so a
// missing secret fails at startup instead of silently becoming empty.
func expandEnv(raw []byte) ([]byte, error) {
	var missing []string
	out := envRef.ReplaceAllFunc(raw, func(m []byte) []byte {
		if string(m) == "$$" {
			return []byte("$")
		}
		sub := envRef.FindSubmatch(m)
		name, hasDefault := string(sub[1]), len(sub[2]) > 0
		val, ok := os.LookupEnv(name)
		if !ok || (val == "" && hasDefault) {
			if !hasDefault {
				missing = append(missing, name)
				return m
			}
			val = string(sub[3])
		}
		quoted, _ := json.Marshal(val)
		return quoted[1 : len(quoted)-1]
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("unset environment variables: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func loadJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if raw, err = expandEnv(raw); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func orDefault(d Duration, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return time.Duration(d)
}

func runGateway(ctx context.Context, cfg gatewayConfig, log *slog.Logger) error {
	if cfg.Listen == "" {
		cfg.Listen = ":25565"
	}
	if cfg.APIListen == "" {
		cfg.APIListen = ":8080"
	}
	if _, ok := cfg.Owners["static"]; ok {
		return errors.New(`owner name "static" is reserved for configured servers`)
	}

	globalNets, err := netguard.ParsePrefixes(cfg.AllowedNetworks)
	if err != nil {
		return fmt.Errorf("allowedNetworks: %w", err)
	}
	owners := map[string]api.Owner{}
	policies := map[string]netguard.Policy{}
	for name, o := range cfg.Owners {
		if o.Token == "" {
			return fmt.Errorf("owner %q has no token", name)
		}
		policy := netguard.Policy{Allowed: globalNets, AllowLoopback: cfg.AllowLoopbackBackends}
		if len(o.AllowedNetworks) > 0 {
			nets, err := netguard.ParsePrefixes(o.AllowedNetworks)
			if err != nil {
				return fmt.Errorf("owner %q allowedNetworks: %w", name, err)
			}
			policy.Allowed = nets
		}
		owners[name] = api.Owner{Token: o.Token, Policy: policy}
		policies[name] = policy
	}
	defaultPolicy := netguard.Policy{Allowed: globalNets, AllowLoopback: cfg.AllowLoopbackBackends}
	// Static servers come from the admin's config and are dialed directly;
	// owner-registered ones go through their owner's policy on every dial.
	dial := func(ctx context.Context, s registry.Server) (net.Conn, error) {
		if s.Static {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", s.Address)
		}
		policy, ok := policies[s.Owner]
		if !ok {
			policy = defaultPolicy
		}
		return policy.DialContext(ctx, "tcp", s.Address)
	}
	guardKeys := map[string][]byte{}
	for name, o := range cfg.Owners {
		guardKeys[name] = guard.Key(o.Token)
	}
	backend := registry.Backend{
		Dial: dial,
		GuardKeys: func(s registry.Server) []byte {
			if s.Static {
				if s.GuardSecret == "" {
					return nil
				}
				return guard.Key(s.GuardSecret)
			}
			return guardKeys[s.Owner]
		},
	}

	dataFile := cfg.DataFile
	switch dataFile {
	case "":
		dataFile = "anymcp-state.json"
	case "-":
		dataFile = ""
	}
	st, err := store.Open(dataFile)
	if err != nil {
		return fmt.Errorf("dataFile: %w", err)
	}
	go st.Run(ctx, 30*time.Second, func(err error) { log.Warn("state flush failed", "err", err) })

	reg := registry.New()
	for _, s := range cfg.Servers {
		if _, err := reg.Upsert("static", s, 0); err != nil {
			return fmt.Errorf("static server %q: %w", s.ID, err)
		}
	}

	m := metrics.New()
	m.GaugeFunc("anymcp_servers_registered", "Registered servers", func() float64 { return float64(len(reg.List())) })
	m.GaugeFunc("anymcp_servers_online", "Servers passing health checks", func() float64 { return float64(len(reg.Online())) })

	secret := []byte(cfg.CookieSecret)
	if len(secret) == 0 {
		secret = make([]byte, 32)
		rand.Read(secret)
		log.Warn("cookieSecret not set; using a random one (transfers in flight break on restart)")
	}
	if !cfg.AcceptProxyProtocol && (cfg.RateLimit.PerSecond > 0 || cfg.RateLimit.MaxConnectionsPerIP > 0 || cfg.PersonalizedMOTD) {
		log.Warn("per-IP rate limits and personalized MOTD need real client IPs; enable acceptProxyProtocol behind a proxy")
	}
	maxLobby := 500
	if cfg.MaxLobbySessions != nil {
		maxLobby = *cfg.MaxLobbySessions
	}

	rt := router.New(router.Config{
		Listen:              cfg.Listen,
		Domain:              cfg.Domain,
		TransferHost:        cfg.TransferHost,
		TransferPort:        cfg.TransferPort,
		CookieSecret:        secret,
		AcceptProxyProtocol: cfg.AcceptProxyProtocol,
		AutoMinScore:        cfg.AutoMinScore,
		AutoMargin:          cfg.AutoMargin,
		ProbeWindow:         time.Duration(cfg.ProbeWindow),
		PendingTTL:          time.Duration(cfg.PendingTTL),
		ForgeModQuery:       cfg.ForgeModQuery,
		MOTD:                cfg.MOTD,
		Store:               st,
		RateLimitPerSecond:  cfg.RateLimit.PerSecond,
		RateLimitBurst:      cfg.RateLimit.Burst,
		MaxConnectionsPerIP: cfg.RateLimit.MaxConnectionsPerIP,
		MaxLobbySessions:    maxLobby,
		StickyTTL:           time.Duration(cfg.StickyTTL),
		PersonalizedMOTD:    cfg.PersonalizedMOTD,
		Dial:                dial,
		GuardKeys:           backend.GuardKeys,
		Metrics:             m,
	}, reg, log)

	go registry.RunHealthChecks(ctx, reg, orDefault(cfg.HealthInterval, 10*time.Second), backend, log)

	httpSrv := &http.Server{
		Addr: cfg.APIListen,
		Handler: api.New(api.Options{
			Registry: reg,
			Owners:   owners,
			TTL:      orDefault(cfg.RegistrationTTL, 45*time.Second),
			Domain:   cfg.Domain,
			OnRegister: func(s registry.Server) {
				go registry.CheckOne(ctx, reg, s, backend, log)
			},
			Tickets:      rt,
			Metrics:      m,
			MetricsToken: cfg.MetricsToken,
			Log:          log,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	go func() {
		log.Info("http api started", "addr", cfg.APIListen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http api", "err", err)
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(sctx)
		if err := st.Flush(); err != nil {
			log.Warn("state flush failed", "err", err)
		}
	}()

	return rt.Serve(ctx)
}
