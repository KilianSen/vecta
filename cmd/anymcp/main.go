// Command anymcp runs the Minecraft gateway or the backend registration agent.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"anymcp/internal/agent"
	"anymcp/internal/api"
	"anymcp/internal/registry"
	"anymcp/internal/router"
)

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

type gatewayConfig struct {
	Listen              string            `json:"listen"`
	APIListen           string            `json:"apiListen"`
	Domain              string            `json:"domain"`
	TransferHost        string            `json:"transferHost"`
	TransferPort        int               `json:"transferPort"`
	CookieSecret        string            `json:"cookieSecret"`
	AcceptProxyProtocol bool              `json:"acceptProxyProtocol"`
	RegistrationTTL     Duration          `json:"registrationTTL"`
	HealthInterval      Duration          `json:"healthInterval"`
	AutoMinScore        int               `json:"autoMinScore"`
	AutoMargin          int               `json:"autoMargin"`
	ProbeWindow         Duration          `json:"probeWindow"`
	PendingTTL          Duration          `json:"pendingTTL"`
	ForgeModQuery       bool              `json:"forgeModQuery"`
	MOTD                string            `json:"motd"`
	Owners              map[string]string `json:"owners"` // owner name -> API token
	Servers             []registry.Server `json:"servers"`
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
	fmt.Fprintln(os.Stderr, "usage: anymcp gateway|agent [-config file.json] [-v]")
	os.Exit(2)
}

func loadJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
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

	reg := registry.New()
	for _, s := range cfg.Servers {
		if _, err := reg.Upsert("static", s, 0); err != nil {
			return fmt.Errorf("static server %q: %w", s.ID, err)
		}
	}

	secret := []byte(cfg.CookieSecret)
	if len(secret) == 0 {
		secret = make([]byte, 32)
		rand.Read(secret)
		log.Warn("cookieSecret not set; using a random one (transfers in flight break on restart)")
	}

	go registry.RunHealthChecks(ctx, reg, orDefault(cfg.HealthInterval, 10*time.Second), log)

	httpSrv := &http.Server{
		Addr: cfg.APIListen,
		Handler: api.New(api.Options{
			Registry: reg,
			Owners:   cfg.Owners,
			TTL:      orDefault(cfg.RegistrationTTL, 45*time.Second),
			Domain:   cfg.Domain,
			OnRegister: func(s registry.Server) {
				go registry.CheckOne(ctx, reg, s, log)
			},
			Log: log,
		}),
		ReadHeaderTimeout: 10 * time.Second,
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
	}()

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
	}, reg, log)
	return rt.Serve(ctx)
}
