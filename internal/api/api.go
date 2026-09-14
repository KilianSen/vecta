// Package api is the HTTP side of the gateway: owners register servers with a
// bearer token, everyone can list servers. Meant to sit behind an HTTP
// reverse proxy (Nginx Proxy Manager proxy host).
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"anymcp/internal/registry"
)

type Options struct {
	Registry *registry.Registry
	// Owners maps owner name to API token.
	Owners map[string]string
	TTL    time.Duration
	Domain string
	// OnRegister is called after a new or re-addressed registration so the
	// server can be health-checked immediately.
	OnRegister func(registry.Server)
	Log        *slog.Logger
}

type api struct{ Options }

func New(o Options) http.Handler {
	a := &api{o}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/v1/servers", a.list)
	mux.HandleFunc("PUT /api/v1/servers/{id}", a.register)
	mux.HandleFunc("DELETE /api/v1/servers/{id}", a.unregister)
	mux.HandleFunc("GET /{$}", a.page)
	return mux
}

func (a *api) owner(r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	for name, t := range a.Owners {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			return name, true
		}
	}
	return "", false
}

// publicServer omits the backend address and owner.
type publicServer struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Join        string   `json:"join,omitempty"`
	Loader      string   `json:"loader"`
	VersionName string   `json:"versionName,omitempty"`
	MinProtocol int32    `json:"minProtocol,omitempty"`
	MaxProtocol int32    `json:"maxProtocol,omitempty"`
	Mods        []string `json:"mods,omitempty"`
	Online      bool     `json:"online"`
	Players     int      `json:"players"`
	MaxPlayers  int      `json:"maxPlayers"`
}

func (a *api) public() []publicServer {
	var out []publicServer
	for _, s := range a.Registry.List() {
		if s.Hidden {
			continue
		}
		p := publicServer{
			ID: s.ID, Name: s.Name, Description: s.Description, Loader: s.Loader,
			VersionName: s.VersionName, MinProtocol: s.MinProtocol, MaxProtocol: s.MaxProtocol,
			Mods: s.Mods, Online: s.Online, Players: s.Players, MaxPlayers: s.MaxPlayers,
		}
		if a.Domain != "" {
			p.Join = s.ID + "." + a.Domain
		}
		out = append(out, p)
	}
	return out
}

func (a *api) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.public())
}

func (a *api) register(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.owner(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	var s registry.Server
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(&s); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	s.ID = r.PathValue("id")
	prev, existed := a.Registry.Get(strings.ToLower(s.ID))
	saved, err := a.Registry.Upsert(owner, s, a.TTL)
	switch {
	case errors.Is(err, registry.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !existed || prev.Address != saved.Address {
		a.Log.Info("server registered", "server", saved.ID, "owner", owner, "address", saved.Address)
		if a.OnRegister != nil {
			a.OnRegister(saved)
		}
	}
	writeJSON(w, http.StatusOK, saved)
}

func (a *api) unregister(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.owner(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	id := strings.ToLower(r.PathValue("id"))
	switch err := a.Registry.Remove(owner, id); {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeError(w, http.StatusForbidden, err.Error())
	default:
		a.Log.Info("server unregistered", "server", id, "owner", owner)
		w.WriteHeader(http.StatusNoContent)
	}
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>anymcp servers</title>
<style>
body{font:15px system-ui,sans-serif;margin:0;padding:24px 16px;background:#111418;color:#e6e6e6}
main{max-width:860px;margin:auto}h1{font-size:22px}code{background:#232830;padding:2px 6px;border-radius:4px}
table{width:100%;border-collapse:collapse}td,th{text-align:left;padding:8px;border-bottom:1px solid #2a2f37}
.on{color:#5fd068}.off{color:#8a8f98}.wrap{overflow-x:auto}
</style>
<main>
<h1>Minecraft servers</h1>
{{if .Domain}}<p>Connect to <code>{{.Domain}}</code> and you'll be matched automatically, or join a server directly by its address.</p>{{end}}
<div class="wrap"><table>
<tr><th>Server</th><th>Status</th><th>Type</th><th>Version</th><th>Players</th><th>Address</th></tr>
{{range .Servers}}<tr>
<td>{{.Name}}{{if .Description}}<br><small>{{.Description}}</small>{{end}}</td>
<td>{{if .Online}}<span class="on">online</span>{{else}}<span class="off">offline</span>{{end}}</td>
<td>{{.Loader}}{{if .Mods}} ({{len .Mods}} mods){{end}}</td>
<td>{{.VersionName}}</td><td>{{.Players}}/{{.MaxPlayers}}</td>
<td>{{if .Join}}<code>{{.Join}}</code>{{end}}</td>
</tr>{{else}}<tr><td colspan="6">No servers registered.</td></tr>{{end}}
</table></div>
</main>`))

func (a *api) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pageTmpl.Execute(w, map[string]any{"Domain": a.Domain, "Servers": a.public()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
