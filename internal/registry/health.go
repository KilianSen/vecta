package registry

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"anymcp/internal/guard"
	"anymcp/internal/proto"
)

// pingProtocol is sent in health pings. -1 makes version-translating plugins
// such as ViaVersion report the server's native version.
const pingProtocol = -1

// Dialer connects to a server's address, e.g. enforcing the address policy
// for owner-registered servers. nil means direct dialing.
type Dialer func(ctx context.Context, s Server) (net.Conn, error)

// GuardKeys returns the guard HMAC key for a guarded server, or nil if none
// is configured.
type GuardKeys func(s Server) []byte

// ErrNoGuardKey means a server has guard enabled but no key is known for it.
var ErrNoGuardKey = errors.New("guard enabled but no key configured")

// Backend bundles how the gateway reaches servers. Zero values mean direct
// dialing and no guard keys.
type Backend struct {
	Dial      Dialer
	GuardKeys GuardKeys
}

// Preamble returns the bytes to send before a health ping: a signed LOCAL
// header for guarded servers, a plain one for PROXY protocol servers.
func (b Backend) Preamble(s Server) ([]byte, error) {
	switch {
	case s.Guard:
		var key []byte
		if b.GuardKeys != nil {
			key = b.GuardKeys(s)
		}
		if key == nil {
			return nil, ErrNoGuardKey
		}
		return guard.LocalHeader(key, time.Now(), guard.NewNonce()), nil
	case s.ProxyProtocol:
		return proto.ProxyV2Local(), nil
	}
	return nil, nil
}

// RunHealthChecks pings every registered server each interval until ctx ends.
func RunHealthChecks(ctx context.Context, reg *Registry, interval time.Duration, b Backend, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		reg.Prune()
		for _, s := range reg.List() {
			go CheckOne(ctx, reg, s, b, log)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CheckOne pings a single server and records the result.
func CheckOne(ctx context.Context, reg *Registry, s Server, b Backend, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var pingDial func(context.Context, string, string) (net.Conn, error)
	if b.Dial != nil {
		pingDial = func(ctx context.Context, _, _ string) (net.Conn, error) { return b.Dial(ctx, s) }
	}
	var st *proto.Status
	preamble, err := b.Preamble(s)
	if err == nil {
		st, err = proto.Ping(ctx, s.Address, pingProtocol, preamble, pingDial)
	}
	h := Health{Online: err == nil}
	if err == nil {
		h.Protocol = st.Version.Protocol
		h.VersionName = st.Version.Name
		h.Players, h.MaxPlayers = st.Players.Online, st.Players.Max
		h.Mods = st.ModIDs()
		if st.ForgeData != nil {
			if _, channels, truncated, err := st.ForgeData.Decode(); err == nil {
				h.ForgeChannels, h.ForgeChannelsTruncated = channels, truncated
			} else {
				log.Debug("forgeData decode failed", "server", s.ID, "err", err)
			}
		}
		if n, ok := versionOnly(h.Protocol); ok {
			h.VersionName = n // server software often prefixes e.g. "Paper 1.21.4"
		}
	}
	if h.Online != s.Online || s.LastPing.IsZero() {
		if err != nil {
			log.Info("server offline", "server", s.ID, "address", s.Address, "err", err)
		} else {
			log.Info("server online", "server", s.ID, "version", h.VersionName, "protocol", h.Protocol)
		}
	}
	reg.SetHealth(s.ID, s.Address, h)
}

func versionOnly(protocol int32) (string, bool) {
	n := proto.VersionName(protocol)
	return n, n != "" && n[0] != 'p' // "protocol N" means unknown
}
