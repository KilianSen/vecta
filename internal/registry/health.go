package registry

import (
	"context"
	"log/slog"
	"time"

	"anymcp/internal/proto"
)

// pingProtocol is sent in health pings. -1 makes version-translating plugins
// such as ViaVersion report the server's native version.
const pingProtocol = -1

// RunHealthChecks pings every registered server each interval until ctx ends.
func RunHealthChecks(ctx context.Context, reg *Registry, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		reg.Prune()
		for _, s := range reg.List() {
			go CheckOne(ctx, reg, s, log)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CheckOne pings a single server and records the result.
func CheckOne(ctx context.Context, reg *Registry, s Server, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := proto.Ping(ctx, s.Address, pingProtocol, s.ProxyProtocol)
	h := Health{Online: err == nil}
	if err == nil {
		h.Protocol = st.Version.Protocol
		h.VersionName = st.Version.Name
		h.Players, h.MaxPlayers = st.Players.Online, st.Players.Max
		h.Mods = st.ModIDs()
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
