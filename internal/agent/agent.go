// Package agent runs next to a backend server and keeps its registration at
// the gateway alive. Stopping the agent unregisters the server.
package agent

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"vecta/internal/registry"
)

type Config struct {
	Gateway  string // base URL of the gateway API, e.g. https://mc-api.example.com
	Token    string
	Interval time.Duration
	ModsDir  string // optional: scan jars here and add their mod IDs
	Server   registry.Server
	// SidePortHook is run (argv, no shell) when a side port assignment
	// changes; see docs/side-ports.md.
	SidePortHook []string
	// StateFile remembers applied side port assignments across restarts.
	StateFile string
}

func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	if cfg.Gateway == "" || cfg.Token == "" || cfg.Server.ID == "" {
		return fmt.Errorf("gateway, token and server.id are required")
	}
	srv := cfg.Server
	if cfg.ModsDir != "" {
		mods, err := ScanMods(cfg.ModsDir, log)
		if err != nil {
			return fmt.Errorf("scan mods: %w", err)
		}
		srv.Mods = mergeUnique(srv.Mods, mods)
		log.Info("scanned mods", "dir", cfg.ModsDir, "count", len(mods))
	}

	hk := newHooks(cfg.SidePortHook, cfg.StateFile, srv.ID, log)
	declared := srv.SidePorts

	url := strings.TrimRight(cfg.Gateway, "/") + "/api/v1/servers/" + srv.ID
	client := &http.Client{Timeout: 10 * time.Second}
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	registered := false
	for {
		srv.SidePorts = append([]registry.SidePort(nil), declared...)
		hk.prefer(srv.SidePorts)
		body, _ := json.Marshal(srv)
		resp, err := call(ctx, client, http.MethodPut, url, cfg.Token, body)
		if err != nil {
			log.Warn("registration failed", "err", err)
			registered = false
		} else {
			if !registered {
				log.Info("registered at gateway", "server", srv.ID, "gateway", cfg.Gateway)
				registered = true
			}
			var saved registry.Server
			if err := json.Unmarshal(resp, &saved); err != nil {
				log.Warn("unreadable registration response", "err", err)
			} else if len(declared) > 0 || len(hk.applied) > 0 {
				hk.apply(ctx, declared, saved.SidePorts)
			}
		}
		select {
		case <-ctx.Done():
			dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := call(dctx, client, http.MethodDelete, url, cfg.Token, nil); err != nil {
				log.Warn("unregister failed", "err", err)
			} else {
				log.Info("unregistered", "server", srv.ID)
			}
			return nil
		case <-t.C:
		}
	}
}

func call(ctx context.Context, c *http.Client, method, url, token string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("%s %s: %s %s", method, url, resp.Status, strings.TrimSpace(string(msg)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func mergeUnique(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range append(append([]string{}, a...), b...) {
		set[strings.ToLower(s)] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ScanMods reads mod IDs from every jar in dir (Fabric, Quilt, Forge,
// NeoForge and legacy mcmod.info metadata).
func ScanMods(dir string, log *slog.Logger) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".jar") {
			continue
		}
		found, err := modIDsFromJar(filepath.Join(dir, e.Name()))
		if err != nil {
			log.Warn("skipping jar", "jar", e.Name(), "err", err)
			continue
		}
		ids = append(ids, found...)
	}
	return mergeUnique(nil, ids), nil
}

var tomlModID = regexp.MustCompile(`^\s*modId\s*=\s*["']([^"']+)["']`)

func modIDsFromJar(path string) ([]string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var ids []string
	for _, f := range zr.File {
		switch f.Name {
		case "fabric.mod.json", "quilt.mod.json", "META-INF/mods.toml", "META-INF/neoforge.mods.toml", "mcmod.info":
		default:
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		if err != nil {
			return nil, err
		}
		switch f.Name {
		case "fabric.mod.json":
			var m struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(data, &m) == nil && m.ID != "" {
				ids = append(ids, m.ID)
			}
		case "quilt.mod.json":
			var m struct {
				Loader struct {
					ID string `json:"id"`
				} `json:"quilt_loader"`
			}
			if json.Unmarshal(data, &m) == nil && m.Loader.ID != "" {
				ids = append(ids, m.Loader.ID)
			}
		case "mcmod.info":
			var list []struct {
				ModID string `json:"modid"`
			}
			if json.Unmarshal(data, &list) != nil {
				var wrapped struct {
					ModList []struct {
						ModID string `json:"modid"`
					} `json:"modList"`
				}
				json.Unmarshal(data, &wrapped)
				list = wrapped.ModList
			}
			for _, m := range list {
				ids = append(ids, m.ModID)
			}
		default: // mods.toml: only modId entries inside [[mods]] tables
			inMods := false
			sc := bufio.NewScanner(bytes.NewReader(data))
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if strings.HasPrefix(line, "[") {
					header, _, _ := strings.Cut(line, "#") // e.g. `[[mods]] #mandatory`
					inMods = strings.TrimSpace(header) == "[[mods]]"
					continue
				}
				if m := tomlModID.FindStringSubmatch(line); inMods && m != nil {
					ids = append(ids, m[1])
				}
			}
		}
	}
	return ids, nil
}
