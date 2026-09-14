package router

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"anymcp/internal/proto"
)

// lobbyRoute is the cookie route that opens the gateway menu. "lobby" is a
// reserved server ID, so it never collides with a real server.
const lobbyRoute = "lobby"

// Ticket tells the server jar how to move a player (docs/owner-api.md).
type Ticket struct {
	Mode      string `json:"mode"` // "transfer" or "reconnect"
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	CookieKey string `json:"cookieKey,omitempty"`
	Cookie    string `json:"cookie,omitempty"` // base64
	Address   string `json:"address,omitempty"`
	Message   string `json:"message,omitempty"`
}

var (
	ErrUnknownTarget   = errors.New("target server is unknown or offline")
	ErrIncompatible    = errors.New("target server does not accept this client version")
	ErrNoPublicAddress = errors.New("gateway has no domain or transferHost configured")
)

// IssueTicket prepares moving player (on client protocol) to target, a
// server ID or "lobby". 1.20.5+ clients get a signed route cookie and a
// transfer address; older clients get a remembered route and must reconnect.
func (r *Router) IssueTicket(player string, protocol int32, target string) (Ticket, error) {
	target = strings.ToLower(strings.TrimSpace(target))
	targetName := "the lobby"
	if target != lobbyRoute {
		srv, ok := r.reg.Get(target)
		if !ok || !srv.Online {
			return Ticket{}, ErrUnknownTarget
		}
		if !srv.Accepts(protocol) {
			return Ticket{}, ErrIncompatible
		}
		targetName = srv.Name
	}
	host := r.cfg.TransferHost
	if host == "" {
		host = r.cfg.Domain
	}
	if host == "" {
		return Ticket{}, ErrNoPublicAddress
	}
	port := r.cfg.TransferPort
	if port == 0 {
		port = 25565
	}

	if protocol >= proto.Proto1_20_5 {
		cookie := signRoute(r.cfg.CookieSecret, target, player, time.Now().Add(2*time.Minute))
		r.m.tickets.Inc("transfer")
		return Ticket{
			Mode:      "transfer",
			Host:      host,
			Port:      port,
			CookieKey: CookieKey,
			Cookie:    base64.StdEncoding.EncodeToString(cookie),
		}, nil
	}

	address := host
	if r.cfg.Domain != "" {
		address = r.cfg.Domain
		if target == lobbyRoute {
			address = "lobby." + r.cfg.Domain
		}
	}
	if port != 25565 {
		address = fmt.Sprintf("%s:%d", address, port)
	}
	r.m.tickets.Inc("reconnect")
	if target == lobbyRoute {
		r.pending.Clear(player)
		return Ticket{Mode: "reconnect", Address: address, Message: fmt.Sprintf("Reconnect to %s to choose a server.", address)}, nil
	}
	r.pending.Set(player, target, r.cfg.PendingTTL)
	return Ticket{Mode: "reconnect", Address: address, Message: fmt.Sprintf("Reconnect to %s to join %s.", address, targetName)}, nil
}
