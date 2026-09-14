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
	"time"

	"anymcp/internal/match"
	"anymcp/internal/proto"
	"anymcp/internal/registry"
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
}

type Router struct {
	cfg     Config
	reg     *registry.Registry
	pending *pendingRoutes
	log     *slog.Logger
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
	cfg.Domain = strings.ToLower(strings.TrimSuffix(cfg.Domain, "."))
	return &Router{cfg: cfg, reg: reg, pending: newPendingRoutes(), log: log}
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
}

func (s *session) send(frames ...[]byte) error {
	for _, f := range frames {
		if _, err := s.conn.Write(f); err != nil {
			return err
		}
	}
	return nil
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
		r.handleStatus(s)
	case proto.IntentLogin, proto.IntentTransfer:
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
	s.send(proto.StatusResponse(r.statusDoc(s.hs.Protocol)))
	_, payload, err := proto.ReadFrame(s.br)
	if err != nil || len(payload) < 1 || payload[0] != 0x01 {
		return
	}
	pong := append(proto.AppendVarInt(nil, int32(len(payload))), payload...)
	s.send(pong)
}

func (r *Router) statusDoc(protocol int32) map[string]any {
	online := r.reg.Online()
	players, maxPlayers := 0, 0
	type sample struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	var samples []sample
	compatible := 0
	for _, srv := range online {
		if srv.Hidden {
			continue
		}
		players += srv.Players
		maxPlayers += srv.MaxPlayers
		mark := "§7"
		if srv.Accepts(protocol) {
			mark = "§a"
			compatible++
		}
		samples = append(samples, sample{
			Name: fmt.Sprintf("%s%s §8(%s, %s)", mark, srv.Name, srv.Loader, srv.VersionName),
			ID:   "00000000-0000-0000-0000-000000000000",
		})
	}
	motd := r.cfg.MOTD
	if motd == "" {
		motd = "anymcp gateway"
	}
	return map[string]any{
		"version": map[string]any{"name": "anymcp", "protocol": protocol},
		"players": map[string]any{"online": players, "max": maxPlayers, "sample": samples},
		"description": proto.Join(
			proto.C(motd+"\n", "gold"),
			proto.C(fmt.Sprintf("%d servers online, %d for %s", len(samples), compatible, proto.VersionName(protocol)), "gray"),
		),
	}
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

	host := s.hs.Host()

	// 1. Returning from a transfer with a signed route cookie.
	if s.hs.Intent == proto.IntentTransfer && s.hs.Protocol >= proto.Proto1_20_5 {
		if id, err := r.readRouteCookie(s); err == nil {
			r.connect(s, id, "cookie")
			return
		} else if !errors.Is(err, errNoCookie) {
			s.log.Info("route cookie rejected", "err", err)
		}
	}
	// 2. Explicit lobby.
	if r.isLobbyHost(host) {
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
			r.connect(s, id, "pending")
			return
		}
	}
	// 5. Only one server could possibly take this client: skip the lobby.
	cands := match.Rank(r.handshakeClient(s), r.visibleOnline())
	if len(cands) == 1 && cands[0].Certain {
		r.connect(s, cands[0].Server.ID, "only-candidate")
		return
	}
	r.lobby(s)
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
	s.log.Info("routing player", "server", srv.ID, "via", reason)
	r.pipe(s, srv, hsRaw, s.loginRaw)
}

// pipe dials the backend, replays the given frames and copies bytes both ways.
func (r *Router) pipe(s *session, srv registry.Server, replay ...[]byte) {
	backend, err := net.DialTimeout("tcp", srv.Address, 5*time.Second)
	if err != nil {
		s.log.Warn("backend dial failed", "server", srv.ID, "err", err)
		if s.hs.Intent != proto.IntentStatus {
			s.send(proto.LoginDisconnect(proto.Join(proto.C(srv.Name, "yellow"), proto.C(" is unreachable.", "red"))))
		}
		return
	}
	defer backend.Close()
	if srv.ProxyProtocol {
		if _, err := backend.Write(proxyV2Header(s.client, backend.RemoteAddr())); err != nil {
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
