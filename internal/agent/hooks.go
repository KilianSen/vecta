package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vecta/internal/registry"
)

// HookTimeout bounds one callback run.
const HookTimeout = 30 * time.Second

// SidePortState is what was last applied for one side port.
type SidePortState struct {
	State    string `json:"state"` // assigned, error or released
	Protocol string `json:"protocol"`
	Backend  int    `json:"backendPort"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Error    string `json:"error,omitempty"`
}

// hooks runs the owner's side port callback whenever an assignment changes
// and remembers what it applied, so restarts don't re-run it and the last
// port can be requested again.
type hooks struct {
	argv      []string
	stateFile string
	serverID  string
	log       *slog.Logger
	applied   map[string]SidePortState
}

func newHooks(argv []string, stateFile, serverID string, log *slog.Logger) *hooks {
	h := &hooks{argv: argv, stateFile: stateFile, serverID: serverID, log: log, applied: map[string]SidePortState{}}
	if stateFile == "" {
		return h
	}
	b, err := os.ReadFile(stateFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		log.Warn("side port state unreadable", "file", stateFile, "err", err)
	default:
		if err := json.Unmarshal(b, &h.applied); err != nil {
			log.Warn("side port state invalid; starting fresh", "file", stateFile, "err", err)
			h.applied = map[string]SidePortState{}
		}
	}
	return h
}

// prefer sets PreferredPort from the last applied assignment.
func (h *hooks) prefer(ports []registry.SidePort) {
	for i := range ports {
		if st, ok := h.applied[strings.ToLower(ports[i].Name)]; ok && st.State == "assigned" && ports[i].PreferredPort == 0 {
			ports[i].PreferredPort = st.Port
		}
	}
}

// apply compares the gateway's answer with what was applied and runs the
// callback for every change. declared is what this agent registered.
func (h *hooks) apply(ctx context.Context, declared, answer []registry.SidePort) {
	got := map[string]registry.SidePort{}
	for _, p := range answer {
		got[p.Name] = p
	}
	changed := false
	for _, d := range declared {
		name := strings.ToLower(d.Name)
		p, ok := got[name]
		want := SidePortState{State: "error", Protocol: strings.ToLower(d.Protocol), Backend: d.Port, Error: "gateway does not support side ports"}
		if ok {
			want.Error = p.Error
			if p.Public != nil {
				want = SidePortState{State: "assigned", Protocol: p.Protocol, Backend: p.Port, Host: p.Public.Host, Port: p.Public.Port}
			}
		}
		if h.applied[name] == want {
			continue
		}
		if want.State == "assigned" {
			h.log.Info("side port assigned", "sideport", name, "public", fmt.Sprintf("%s:%d", want.Host, want.Port))
		} else {
			h.log.Warn("side port not assigned", "sideport", name, "err", want.Error)
		}
		if h.run(ctx, name, want) {
			h.applied[name] = want
			changed = true
		}
	}
	for name, st := range h.applied {
		if containsName(declared, name) {
			continue
		}
		rel := SidePortState{State: "released", Protocol: st.Protocol, Backend: st.Backend}
		if st.State == "released" || h.run(ctx, name, rel) {
			delete(h.applied, name)
			changed = true
		}
	}
	if changed {
		h.save()
	}
}

func containsName(ports []registry.SidePort, name string) bool {
	for _, p := range ports {
		if strings.ToLower(p.Name) == name {
			return true
		}
	}
	return false
}

// run executes the callback and reports whether the state counts as applied.
// Without a callback every state is applied.
func (h *hooks) run(ctx context.Context, name string, st SidePortState) bool {
	if len(h.argv) == 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, HookTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.argv[0], h.argv[1:]...)
	cmd.Env = append(os.Environ(), hookEnv(h.serverID, name, st)...)
	out, err := cmd.CombinedOutput()
	if s := strings.TrimSpace(string(out)); s != "" {
		h.log.Info("side port hook output", "sideport", name, "output", s)
	}
	if err != nil {
		h.log.Warn("side port hook failed; retrying next heartbeat", "sideport", name, "state", st.State, "err", err)
		return false
	}
	return true
}

// hookEnv is the environment passed to side port callbacks. The server jar
// passes the same variables plus VECTA_SERVER_STARTED; the agent cannot tell
// whether the server runs, so it leaves that one out.
func hookEnv(serverID, name string, st SidePortState) []string {
	return []string{
		"VECTA_SERVER_ID=" + serverID,
		"VECTA_SIDEPORT_NAME=" + name,
		"VECTA_SIDEPORT_PROTOCOL=" + st.Protocol,
		"VECTA_SIDEPORT_BACKEND_PORT=" + strconv.Itoa(st.Backend),
		"VECTA_SIDEPORT_STATE=" + st.State,
		"VECTA_SIDEPORT_HOST=" + st.Host,
		"VECTA_SIDEPORT_PORT=" + portString(st.Port),
		"VECTA_SIDEPORT_ERROR=" + st.Error,
	}
}

func portString(p int) string {
	if p == 0 {
		return ""
	}
	return strconv.Itoa(p)
}

func (h *hooks) save() {
	if h.stateFile == "" {
		return
	}
	b, _ := json.MarshalIndent(h.applied, "", "  ")
	tmp := h.stateFile + ".tmp"
	err := os.MkdirAll(filepath.Dir(h.stateFile), 0o755)
	if err == nil {
		err = os.WriteFile(tmp, b, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, h.stateFile)
	}
	if err != nil {
		h.log.Warn("side port state not saved", "file", h.stateFile, "err", err)
	}
}
