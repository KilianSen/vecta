// Package router is the player-facing Minecraft listener. It reads just
// enough of each connection to pick a backend, then pipes bytes untouched.
package router

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vecta/internal/guard"
	"vecta/internal/match"
	"vecta/internal/metrics"
	"vecta/internal/proto"
	"vecta/internal/registry"
	"vecta/internal/store"
)

type Config struct {
	Listen string
	// Domain is the public base hostname (e.g. play.example.com). Servers are
	// reachable directly at <id>.<Domain>; lobby.<Domain> always opens the lobby.
	Domain string
	// TransferHost/TransferPort is where 1.20.5+ clients are transferred to
	// after choosing. Defaults: Domain (or the host the client used) and the
	// port from the client's handshake.
	TransferHost string
	TransferPort int
	CookieSecret []byte
	// AcceptProxyProtocol requires a PROXY v1/v2 header on every connection
	// (enable proxy_protocol on the NPM stream).
	AcceptProxyProtocol bool
	AutoMinScore        int
	AutoMargin          int
	ProbeWindow         time.Duration // how long the lobby listens for client brand/channels
	PendingTTL          time.Duration
	ForgeModQuery       bool
	MOTD                string
	// Store persists remembered routes and preferences (nil: memory only).
	Store *store.Store

	// Per-client-IP limits. Only useful when the router sees real client
	// IPs (directly or through PROXY protocol). Zero disables each limit.
	RateLimitPerSecond  float64
	RateLimitBurst      int
	MaxConnectionsPerIP int
	// MaxLobbySessions caps concurrent lobby/limbo sessions (0: unlimited).
	MaxLobbySessions int
	// StickyTTL remembers each player's last server and sends them back
	// there on their next plain join (0: 30 days, negative: disabled).
	StickyTTL time.Duration
	// PersonalizedMOTD shows "next join"/"last played" in the server list,
	// looked up by client IP. Enable only with real client IPs.
	PersonalizedMOTD bool
	// Dial connects to backends (nil: direct dialing). Use it to enforce the
	// address policy on owner-registered servers.
	Dial registry.Dialer
	// GuardKeys provides the signing key for servers with guard enabled.
	GuardKeys registry.GuardKeys
	// Metrics receives router counters (nil: disabled).
	Metrics *metrics.Registry
}

type routerMetrics struct {
	connections  *metrics.CounterVec
	rejected     *metrics.CounterVec
	routes       *metrics.CounterVec
	lobby        *metrics.CounterVec
	dialFailures *metrics.CounterVec
	tickets      *metrics.CounterVec
}

type Router struct {
	cfg         Config
	reg         *registry.Registry
	pending     *pendingRoutes
	log         *slog.Logger
	limiter     *limiter
	lobbySlots  semaphore
	lobbyActive atomic.Int64
	m           routerMetrics

	statusMu    sync.Mutex
	statusCache map[int32]statusSummary
}

func New(cfg Config, reg *registry.Registry, log *slog.Logger) *Router {
	if cfg.AutoMinScore == 0 {
		cfg.AutoMinScore = 50
	}
	if cfg.AutoMargin == 0 {
		cfg.AutoMargin = 15
	}
	if cfg.ProbeWindow == 0 {
		cfg.ProbeWindow = 1500 * time.Millisecond
	}
	if cfg.PendingTTL == 0 {
		cfg.PendingTTL = 3 * time.Minute
	}
	switch {
	case cfg.StickyTTL == 0:
		cfg.StickyTTL = 30 * 24 * time.Hour
	case cfg.StickyTTL < 0:
		cfg.StickyTTL = 0
	}
	cfg.Domain = strings.ToLower(strings.TrimSuffix(cfg.Domain, "."))
	r := &Router{
		cfg:         cfg,
		reg:         reg,
		pending:     newPendingRoutes(cfg.Store),
		log:         log,
		limiter:     newLimiter(cfg.RateLimitPerSecond, cfg.RateLimitBurst, cfg.MaxConnectionsPerIP),
		lobbySlots:  newSemaphore(cfg.MaxLobbySessions),
		statusCache: map[int32]statusSummary{},
	}
	if m := cfg.Metrics; m != nil {
		r.m = routerMetrics{
			connections:  m.Counter("vecta_connections_total", "Player connections by handshake intent", "intent"),
			rejected:     m.Counter("vecta_connections_rejected_total", "Connections refused by limits", "reason"),
			routes:       m.Counter("vecta_routes_total", "Players piped to a backend", "via", "server"),
			lobby:        m.Counter("vecta_lobby_outcomes_total", "How lobby sessions ended", "outcome"),
			dialFailures: m.Counter("vecta_backend_dial_failures_total", "Failed backend dials", "server"),
			tickets:      m.Counter("vecta_transfer_tickets_total", "Transfer tickets issued via the owner API", "mode"),
		}
		m.GaugeFunc("vecta_lobby_sessions_active", "Players currently in the lobby or limbo",
			func() float64 { return float64(r.lobbyActive.Load()) })
	}
	return r
}

func (r *Router) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", r.cfg.Listen)
	if err != nil {
		return err
	}
	return r.ServeListener(ctx, ln)
}

// ServeListener accepts player connections on ln until ctx ends.
func (r *Router) ServeListener(ctx context.Context, ln net.Listener) error {
	r.log.Info("minecraft listener started", "addr", ln.Addr().String())
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.pending.prune()
				r.limiter.sweep()
			}
		}
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go r.handle(conn)
	}
}

// session carries per-connection state through routing.
type session struct {
	conn     net.Conn
	br       *bufio.Reader
	client   net.Addr // real client address (after PROXY header)
	hs       proto.Handshake
	hsRaw    []byte
	login    proto.LoginStart
	loginRaw []byte
	log      *slog.Logger
	// forceMenu is set for lobby.<domain>: the player wants to choose, so
	// menus are shown even when a single best match exists.
	forceMenu bool
}

func (s *session) send(frames ...[]byte) error {
	for _, f := range frames {
		if _, err := s.conn.Write(f); err != nil {
			return err
		}
	}
	return nil
}

// clientIP is the client's IP without port.
func (s *session) clientIP() string {
	host, _, err := net.SplitHostPort(s.client.String())
	if err != nil {
		return s.client.String()
	}
	return host
}

func (r *Router) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	s := &session{conn: conn, br: bufio.NewReaderSize(conn, 4096), client: conn.RemoteAddr()}

	if r.cfg.AcceptProxyProtocol {
		addr, err := readProxyHeader(s.br)
		if err != nil {
			r.log.Debug("proxy protocol", "remote", conn.RemoteAddr().String(), "err", err)
			return
		}
		if addr != nil {
			s.client = addr
		}
	}
	release, reason := r.limiter.acquire(s.clientIP())
	if reason != "" {
		r.m.rejected.Inc(reason)
		r.log.Debug("connection refused by limits", "client", s.clientIP(), "reason", reason)
		return
	}
	defer release()

	if b, err := s.br.Peek(1); err != nil || b[0] == 0xFE {
		return // closed or pre-1.7 legacy ping
	}

	raw, payload, err := proto.ReadFrame(s.br)
	if err != nil {
		return
	}
	if s.hs, err = proto.ParseHandshake(payload); err != nil {
		r.log.Debug("bad handshake", "client", s.client.String(), "err", err)
		return
	}
	s.hsRaw = raw
	s.log = r.log.With("client", s.client.String(), "protocol", s.hs.Protocol, "host", s.hs.Host())

	switch s.hs.Intent {
	case proto.IntentStatus:
		r.m.connections.Inc("status")
		r.handleStatus(s)
	case proto.IntentLogin, proto.IntentTransfer:
		r.m.connections.Inc(map[int32]string{proto.IntentLogin: "login", proto.IntentTransfer: "transfer"}[s.hs.Intent])
		r.handleLogin(s)
	}
}

// subdomainServer maps "<id>.<domain>" to a server ID.
func (r *Router) subdomainServer(host string) string {
	if r.cfg.Domain == "" || !strings.HasSuffix(host, "."+r.cfg.Domain) {
		return ""
	}
	id := strings.TrimSuffix(host, "."+r.cfg.Domain)
	if strings.Contains(id, ".") {
		return ""
	}
	return id
}

func (r *Router) isLobbyHost(host string) bool {
	return r.cfg.Domain != "" && host == "lobby."+r.cfg.Domain
}

func (r *Router) handleStatus(s *session) {
	if id := r.subdomainServer(s.hs.Host()); id != "" {
		if srv, ok := r.reg.Get(id); ok && srv.Online {
			r.pipe(s, srv, s.hsRaw)
			return
		}
	}
	// Status request (0x00), then optional ping (0x01).
	if _, payload, err := proto.ReadFrame(s.br); err != nil || len(payload) != 1 {
		return
	}
	s.send(proto.StatusResponse(r.statusDoc(s.hs.Protocol, s.clientIP())))
	_, payload, err := proto.ReadFrame(s.br)
	if err != nil || len(payload) < 1 || payload[0] != 0x01 {
		return
	}
	pong := append(proto.AppendVarInt(nil, int32(len(payload))), payload...)
	s.send(pong)
}

type statusSample struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// statusSummary is the per-protocol part of the status response, cached
// briefly so server-list refresh floods don't rebuild it every time.
type statusSummary struct {
	built      time.Time
	players    int
	maxPlayers int
	compatible int
	samples    []statusSample
}

const statusCacheTTL = 2 * time.Second

func (r *Router) summary(protocol int32) statusSummary {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	if sum, ok := r.statusCache[protocol]; ok && time.Since(sum.built) < statusCacheTTL {
		return sum
	}
	if len(r.statusCache) > 256 {
		r.statusCache = map[int32]statusSummary{}
	}
	sum := statusSummary{built: time.Now()}
	for _, srv := range r.reg.Online() {
		if srv.Hidden {
			continue
		}
		sum.players += srv.Players
		sum.maxPlayers += srv.MaxPlayers
		mark := "§7"
		if srv.Accepts(protocol) {
			mark = "§a"
			sum.compatible++
		}
		sum.samples = append(sum.samples, statusSample{
			Name: fmt.Sprintf("%s%s §8(%s, %s)", mark, srv.Name, srv.Loader, srv.VersionName),
			ID:   "00000000-0000-0000-0000-000000000000",
		})
	}
	r.statusCache[protocol] = sum
	return sum
}

func (r *Router) statusDoc(protocol int32, clientIP string) map[string]any {
	sum := r.summary(protocol)
	motd := r.cfg.MOTD
	if motd == "" {
		motd = "vecta gateway"
	}
	second := proto.C(fmt.Sprintf("%d servers online, %d for %s", len(sum.samples), sum.compatible, proto.VersionName(protocol)), "gray")
	if line, ok := r.personalLine(clientIP); ok {
		second = line
	}
	return map[string]any{
		"version":     map[string]any{"name": "vecta", "protocol": protocol},
		"players":     map[string]any{"online": sum.players, "max": sum.maxPlayers, "sample": sum.samples},
		"description": proto.Join(proto.C(motd+"\n", "gold"), second),
	}
}

// personalLine describes where this client's next join goes.
func (r *Router) personalLine(clientIP string) (proto.Text, bool) {
	if !r.cfg.PersonalizedMOTD {
		return proto.Text{}, false
	}
	player, ok := r.pending.IPPlayer(clientIP)
	if !ok {
		return proto.Text{}, false
	}
	if id, ok := r.pending.Get(player); ok {
		if srv, ok := r.reg.Get(id); ok {
			return proto.Join(proto.C("Next join: ", "gray"), proto.C(srv.Name, "green")), true
		}
	}
	if r.cfg.StickyTTL > 0 {
		if id, ok := r.pending.Sticky(player); ok {
			if srv, ok := r.reg.Get(id); ok && srv.Online {
				switch_ := ""
				if r.cfg.Domain != "" {
					switch_ = " · lobby." + r.cfg.Domain + " to switch"
				}
				return proto.Join(proto.C("Back to ", "gray"), proto.C(srv.Name, "green"), proto.C(switch_, "dark_gray")), true
			}
		}
	}
	return proto.Text{}, false
}

func (r *Router) handleLogin(s *session) {
	raw, payload, err := proto.ReadFrame(s.br)
	if err != nil {
		return
	}
	if s.login, err = proto.ParseLoginStart(payload, s.hs.Protocol); err != nil {
		s.log.Debug("bad login start", "err", err)
		return
	}
	s.loginRaw = raw
	s.log = s.log.With("player", s.login.Name)
	if r.cfg.PersonalizedMOTD {
		r.pending.SetIPPlayer(s.clientIP(), s.login.Name, 30*24*time.Hour)
	}

	host := s.hs.Host()

	// 1. Returning from a transfer with a signed route cookie.
	if s.hs.Intent == proto.IntentTransfer && s.hs.Protocol >= proto.Proto1_20_5 {
		if id, err := r.readRouteCookie(s); err == nil {
			if id == lobbyRoute {
				s.forceMenu = true
				r.lobby(s)
				return
			}
			r.connect(s, id, "cookie")
			return
		} else if !errors.Is(err, errNoCookie) {
			s.log.Info("route cookie rejected", "err", err)
		}
	}
	// 2. Explicit lobby.
	if r.isLobbyHost(host) {
		s.forceMenu = true
		r.lobby(s)
		return
	}
	// 3. Direct subdomain.
	if id := r.subdomainServer(host); id != "" {
		r.connect(s, id, "subdomain")
		return
	}
	// 4. Remembered choice (clients that reconnect instead of transferring).
	if id, ok := r.pending.Get(s.login.Name); ok {
		if srv, ok := r.reg.Get(id); ok && srv.Online && srv.Accepts(s.hs.Protocol) {
			r.pending.Clear(s.login.Name)
			r.connect(s, id, "pending")
			return
		}
	}
	// 5. Back to the last server played, if it still fits.
	if id, ok := r.stickyServer(s); ok {
		r.connect(s, id, "sticky")
		return
	}
	// 6. Only one online server accepts this version at all, and it surely
	// fits: nothing to choose. Other servers on the same version might fit
	// better once the lobby learns the loader and mods (the handshake alone
	// cannot tell a NeoForge or Fabric client from vanilla), so they force
	// the lobby.
	if srv, ok := r.onlyServerFor(s); ok {
		r.connect(s, srv.ID, "only-candidate")
		return
	}
	r.lobby(s)
}

// stickyServer returns the player's last server when it is online, accepts
// the client version and is not excluded by what the handshake reveals.
func (r *Router) stickyServer(s *session) (string, bool) {
	if r.cfg.StickyTTL <= 0 {
		return "", false
	}
	id, ok := r.pending.Sticky(s.login.Name)
	if !ok {
		return "", false
	}
	srv, ok := r.reg.Get(id)
	if !ok || !srv.Online || !srv.Accepts(s.hs.Protocol) || r.pending.Rejected(s.login.Name, id) {
		return "", false
	}
	return id, len(match.Rank(r.handshakeClient(s), []registry.Server{srv})) == 1
}

// onlyServerFor returns the server when exactly one visible online server
// accepts the client's protocol and is a certain match for the handshake.
func (r *Router) onlyServerFor(s *session) (registry.Server, bool) {
	var only registry.Server
	accepting := 0
	for _, srv := range r.visibleOnline() {
		if srv.Accepts(s.hs.Protocol) {
			accepting++
			only = srv
		}
	}
	if accepting != 1 {
		return registry.Server{}, false
	}
	cands := match.Rank(r.handshakeClient(s), []registry.Server{only})
	return only, len(cands) == 1 && cands[0].Certain
}

// handshakeClient derives what is known before the lobby probes further.
func (r *Router) handshakeClient(s *session) match.Client {
	c := match.Client{Protocol: s.hs.Protocol, Loader: match.Unknown}
	marker := s.hs.Marker()
	switch {
	case strings.HasPrefix(marker, "FML"):
		c.Loader = match.Forge
	case strings.Contains(marker, "FORGE"):
		c.Loader = match.Forge
	}
	return c
}

func (r *Router) visibleOnline() []registry.Server {
	all := r.reg.Online()
	out := all[:0]
	for _, srv := range all {
		if !srv.Hidden {
			out = append(out, srv)
		}
	}
	return out
}

var errNoCookie = errors.New("no route cookie")

func (r *Router) readRouteCookie(s *session) (string, error) {
	if err := s.send(proto.NewPacket(proto.LoginCookieRequestID).String(CookieKey).Frame()); err != nil {
		return "", err
	}
	_, payload, err := proto.ReadFrame(s.br)
	if err != nil {
		return "", err
	}
	b := proto.NewBuffer(payload)
	if id, _ := b.VarInt(); id != proto.LoginCookieResponseID {
		return "", fmt.Errorf("expected cookie response, got 0x%02x", id)
	}
	if _, err := b.String(32767); err != nil {
		return "", err
	}
	has, err := b.Bool()
	if err != nil || !has {
		return "", errNoCookie
	}
	data, err := b.ByteArray(5120)
	if err != nil {
		return "", err
	}
	return verifyRoute(r.cfg.CookieSecret, data, s.login.Name, time.Now())
}

// connect routes the session to a registered server or disconnects with a reason.
func (r *Router) connect(s *session, id, reason string) {
	srv, ok := r.reg.Get(id)
	switch {
	case !ok:
		s.send(proto.LoginDisconnect(proto.Join(proto.C("Unknown server ", "red"), proto.C(id, "yellow"))))
		return
	case !srv.Online:
		s.send(proto.LoginDisconnect(proto.Join(proto.C(srv.Name, "yellow"), proto.C(" is offline right now.", "red"))))
		return
	case !srv.Accepts(s.hs.Protocol):
		s.send(proto.LoginDisconnect(proto.Join(
			proto.C(srv.Name, "yellow"),
			proto.C(fmt.Sprintf(" needs %s, you are on %s.", srv.VersionName, proto.VersionName(s.hs.Protocol)), "red"),
		)))
		return
	}
	hsRaw := s.hsRaw
	if s.hs.Intent == proto.IntentTransfer {
		// Backends need accepts-transfers for intent 3; to them this is a
		// normal login.
		hs := s.hs
		hs.Intent = proto.IntentLogin
		hsRaw = hs.Frame()
	}
	if r.cfg.StickyTTL > 0 {
		r.pending.SetSticky(s.login.Name, srv.ID, r.cfg.StickyTTL)
	}
	r.m.routes.Inc(reason, srv.ID)
	s.log.Info("routing player", "server", srv.ID, "via", reason)
	r.pipe(s, srv, hsRaw, s.loginRaw)
}

func (r *Router) dial(srv registry.Server) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r.cfg.Dial != nil {
		return r.cfg.Dial(ctx, srv)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", srv.Address)
}

// pipe dials the backend, replays the given frames and copies bytes both ways.
func (r *Router) pipe(s *session, srv registry.Server, replay ...[]byte) {
	var guardKey []byte
	if srv.Guard {
		if r.cfg.GuardKeys != nil {
			guardKey = r.cfg.GuardKeys(srv)
		}
		if guardKey == nil {
			s.log.Error("guarded server has no guard key", "server", srv.ID)
			if s.hs.Intent != proto.IntentStatus {
				s.send(proto.LoginDisconnect(proto.Join(proto.C(srv.Name, "yellow"), proto.C(" is misconfigured on the gateway.", "red"))))
			}
			return
		}
	}
	backend, err := r.dial(srv)
	if err != nil {
		r.m.dialFailures.Inc(srv.ID)
		s.log.Warn("backend dial failed", "server", srv.ID, "err", err)
		if s.hs.Intent != proto.IntentStatus {
			s.send(proto.LoginDisconnect(proto.Join(proto.C(srv.Name, "yellow"), proto.C(" is unreachable.", "red"))))
		}
		return
	}
	defer backend.Close()
	var preamble []byte
	switch {
	case srv.Guard:
		preamble = guard.Header(guardKey, s.client, backend.RemoteAddr(), time.Now(), guard.NewNonce())
	case srv.ProxyProtocol:
		preamble = proxyV2Header(s.client, backend.RemoteAddr())
	}
	if len(preamble) > 0 {
		if _, err := backend.Write(preamble); err != nil {
			return
		}
	}
	for _, f := range replay {
		if _, err := backend.Write(f); err != nil {
			return
		}
	}
	s.conn.SetDeadline(time.Time{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(backend, s.br) // drains buffered bytes first
		closeWrite(backend)
	}()
	go func() {
		defer wg.Done()
		io.Copy(s.conn, backend)
		closeWrite(s.conn)
	}()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
		return
	}
	c.Close()
}
