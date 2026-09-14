// Package api is the HTTP side of the gateway: owners register servers and
// request transfer tickets with a bearer token, everyone can list servers.
// Meant to sit behind an HTTP reverse proxy (Nginx Proxy Manager proxy host).
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"anymcp/internal/metrics"
	"anymcp/internal/netguard"
	"anymcp/internal/registry"
	"anymcp/internal/router"
)

// Owner is an API client allowed to register servers.
type Owner struct {
	Token string
	// Policy limits which addresses this owner may register and the gateway
	// will dial for them.
	Policy netguard.Policy
}

// TicketIssuer creates transfer tickets (implemented by *router.Router).
type TicketIssuer interface {
	IssueTicket(player string, protocol int32, target string) (router.Ticket, error)
}

type Options struct {
	Registry *registry.Registry
	Owners   map[string]Owner
	TTL      time.Duration
	Domain   string
	// OnRegister is called after a new or re-addressed registration so the
	// server can be health-checked immediately.
	OnRegister func(registry.Server)
	Tickets    TicketIssuer
	Metrics    *metrics.Registry
	// MetricsToken, if set, is required as bearer token for /metrics.
	MetricsToken string
	Log          *slog.Logger
}

type api struct {
	Options
	global *broker
}

func New(o Options) http.Handler {
	a := &api{Options: o, global: newBroker()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /metrics", a.metrics)
	mux.HandleFunc("GET /api/v1/servers", a.list)
	mux.HandleFunc("PUT /api/v1/servers/{id}", a.register)
	mux.HandleFunc("DELETE /api/v1/servers/{id}", a.unregister)
	mux.HandleFunc("POST /api/v1/servers/{id}/transfer-ticket", a.ticket)
	mux.HandleFunc("POST /api/v1/servers/{id}/global", a.postGlobal)
	mux.HandleFunc("GET /api/v1/global", a.pollGlobal)
	mux.HandleFunc("GET /{$}", a.page)
	return mux
}

func bearer(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token
}

func (a *api) owner(r *http.Request) (string, Owner, bool) {
	token := bearer(r)
	if token == "" {
		return "", Owner{}, false
	}
	for name, o := range a.Owners {
		if subtle.ConstantTimeCompare([]byte(o.Token), []byte(token)) == 1 {
			return name, o, true
		}
	}
	return "", Owner{}, false
}

// publicServer omits the backend address and owner.
type publicServer struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Join        string   `json:"join,omitempty"`
	Loader      string   `json:"loader"`
	Software    string   `json:"software,omitempty"`
	VersionName string   `json:"versionName,omitempty"`
	MinProtocol int32    `json:"minProtocol,omitempty"`
	MaxProtocol int32    `json:"maxProtocol,omitempty"`
	Mods        []string `json:"mods,omitempty"`
	Online      bool     `json:"online"`
	Players     int      `json:"players"`
	MaxPlayers  int      `json:"maxPlayers"`
}

func (a *api) public() []publicServer {
	out := []publicServer{} // encode as [] rather than null when empty
	for _, s := range a.Registry.List() {
		if s.Hidden {
			continue
		}
		p := publicServer{
			ID: s.ID, Name: s.Name, Description: s.Description, Loader: s.Loader, Software: s.Software,
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
	name, owner, ok := a.owner(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	var s registry.Server
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&s); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	s.ID = r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if _, err := owner.Policy.Resolve(ctx, s.Address); err != nil {
		writeError(w, http.StatusForbidden, "address not allowed: "+err.Error())
		return
	}
	prev, existed := a.Registry.Get(strings.ToLower(s.ID))
	saved, err := a.Registry.Upsert(name, s, a.TTL)
	switch {
	case errors.Is(err, registry.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !existed || prev.Address != saved.Address {
		a.Log.Info("server registered", "server", saved.ID, "owner", name, "address", saved.Address, "reporter", saved.Reporter)
		if a.OnRegister != nil {
			a.OnRegister(saved)
		}
	}
	writeJSON(w, http.StatusOK, saved)
}

func (a *api) unregister(w http.ResponseWriter, r *http.Request) {
	name, _, ok := a.owner(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	id := strings.ToLower(r.PathValue("id"))
	switch err := a.Registry.Remove(name, id); {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeError(w, http.StatusForbidden, err.Error())
	default:
		a.Log.Info("server unregistered", "server", id, "owner", name)
		w.WriteHeader(http.StatusNoContent)
	}
}

var playerName = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)

type ticketRequest struct {
	Player   string `json:"player"`
	UUID     string `json:"uuid"`
	Protocol int32  `json:"protocol"`
	Target   string `json:"target"`
}

func (a *api) ticket(w http.ResponseWriter, r *http.Request) {
	name, _, ok := a.owner(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	id := strings.ToLower(r.PathValue("id"))
	src, ok := a.Registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "server "+id+" is not registered")
		return
	}
	if src.Owner != name {
		writeError(w, http.StatusForbidden, "token does not own server "+id)
		return
	}
	if a.Tickets == nil {
		writeError(w, http.StatusNotImplemented, "transfers are not enabled on this gateway")
		return
	}
	var req ticketRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	switch {
	case !playerName.MatchString(req.Player):
		writeError(w, http.StatusBadRequest, "invalid player name")
		return
	case req.Protocol <= 0:
		writeError(w, http.StatusBadRequest, "protocol is required")
		return
	case strings.TrimSpace(req.Target) == "":
		writeError(w, http.StatusBadRequest, "target is required")
		return
	}
	t, err := a.Tickets.IssueTicket(req.Player, req.Protocol, req.Target)
	switch {
	case errors.Is(err, router.ErrUnknownTarget):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, router.ErrIncompatible):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, router.ErrNoPublicAddress):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.Log.Info("transfer ticket issued", "from", id, "player", req.Player, "target", req.Target, "mode", t.Mode)
	writeJSON(w, http.StatusOK, t)
}

type globalRequest struct {
	Player  string `json:"player"`
	Message string `json:"message"`
}

// controlChars matches C0/C1 control characters and the section sign used for
// legacy color codes, so a broadcast can't inject formatting or newlines.
var controlChars = regexp.MustCompile(`[\x00-\x1f\x7f\xa7§]`)

func (a *api) postGlobal(w http.ResponseWriter, r *http.Request) {
	name, _, ok := a.owner(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	id := strings.ToLower(r.PathValue("id"))
	src, ok := a.Registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "server "+id+" is not registered")
		return
	}
	if src.Owner != name {
		writeError(w, http.StatusForbidden, "token does not own server "+id)
		return
	}
	var req globalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	msg := controlChars.ReplaceAllString(strings.TrimSpace(req.Message), "")
	if !playerName.MatchString(req.Player) {
		writeError(w, http.StatusBadRequest, "invalid player name")
		return
	}
	if msg == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}
	if len(msg) > 256 {
		msg = msg[:256]
	}
	seq := a.global.publish(src.ID, req.Player, msg)
	writeJSON(w, http.StatusOK, map[string]int64{"seq": seq})
}

func (a *api) pollGlobal(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.owner(r); !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	// No "after" means a fresh poller initializing its position: return the
	// current seq immediately, without replaying history.
	if !r.URL.Query().Has("after") {
		writeJSON(w, http.StatusOK, map[string]any{"messages": []GlobalMessage{}, "seq": a.global.current()})
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	msgs, max := a.global.wait(ctx, after, 25*time.Second)
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "seq": max})
}

func (a *api) metrics(w http.ResponseWriter, r *http.Request) {
	if a.Metrics == nil {
		http.NotFound(w, r)
		return
	}
	if a.MetricsToken != "" && subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(a.MetricsToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	a.Metrics.Handler().ServeHTTP(w, r)
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
