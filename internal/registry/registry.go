// Package registry tracks the backend servers players can be routed to.
// Servers come from static config or register themselves (via the agent) and
// must heartbeat before their TTL expires.
package registry

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"vecta/internal/proto"
)

// Server describes one backend. The first block is owner-declared metadata,
// the second is live state maintained by the gateway.
type Server struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Description        string   `json:"description,omitempty"`
	Address            string   `json:"address"` // host:port, dialed by the gateway
	Loader             string   `json:"loader"`  // vanilla, paper, fabric, quilt, forge, neoforge, ...
	MinProtocol        int32    `json:"minProtocol,omitempty"`
	MaxProtocol        int32    `json:"maxProtocol,omitempty"`
	Mods               []string `json:"mods,omitempty"`
	RequiredClientMods []string `json:"requiredClientMods,omitempty"`
	Hidden             bool     `json:"hidden,omitempty"` // reachable by subdomain only
	ProxyProtocol      bool     `json:"proxyProtocol,omitempty"`
	// Guard means the backend only accepts connections carrying the gateway's
	// signed PROXY v2 header (docs/guard-protocol.md). The key comes from the
	// owner token, or GuardSecret for static servers.
	Guard       bool   `json:"guard,omitempty"`
	GuardSecret string `json:"guardSecret,omitempty"`
	// SidePorts are extra backend ports (voice chat, maps, ...) the gateway
	// exposes on ports it assigns (docs/side-ports.md).
	SidePorts []SidePort `json:"sidePorts,omitempty"`

	// Reported by the server jar or agent (see docs/owner-api.md).
	Reporter    string            `json:"reporter,omitempty"`
	Software    string            `json:"software,omitempty"`
	Protocols   []int32           `json:"protocols,omitempty"` // exact accepted list; overrides the range
	ModVersions map[string]string `json:"modVersions,omitempty"`
	Channels    []Channel         `json:"channels,omitempty"`

	Owner       string    `json:"owner"`
	Static      bool      `json:"static,omitempty"`
	Online      bool      `json:"online"`
	VersionName string    `json:"versionName,omitempty"`
	Protocol    int32     `json:"protocol,omitempty"` // as reported by status ping
	Players     int       `json:"players"`
	MaxPlayers  int       `json:"maxPlayers"`
	LastPing    time.Time `json:"lastPing,omitempty"`
	// ForgeChannels are the network channels a Forge 1.13-1.20.1 server
	// advertises in its status ping, with exact versions.
	ForgeChannels          []proto.ForgeChannel `json:"forgeChannels,omitempty"`
	ForgeChannelsTruncated bool                 `json:"forgeChannelsTruncated,omitempty"`
	ExpiresAt              time.Time            `json:"expiresAt,omitempty"`
}

// Channel is a network channel/payload a server registers.
type Channel struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Required means clients without this channel cannot join.
	Required bool `json:"required,omitempty"`
}

// SidePort is an extra backend port that the gateway forwards from a public
// port it assigns. Public and Error are set by the gateway only.
type SidePort struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"` // tcp or udp
	Port     int    `json:"port"`     // backend port; the host comes from Address
	// PreferredPort asks for a public port, typically the last one assigned,
	// so assignments survive gateway restarts when the port is still free.
	PreferredPort int         `json:"preferredPort,omitempty"`
	Public        *PublicAddr `json:"public,omitempty"`
	Error         string      `json:"error,omitempty"`
}

// PublicAddr is where players reach a side port.
type PublicAddr struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// MaxSidePorts caps side ports per server.
const MaxSidePorts = 16

// Accepts reports whether a client protocol is accepted: by the exact
// protocols list if reported, else by the declared range. With neither, the
// server accepts exactly the protocol its ping reports.
func (s *Server) Accepts(protocol int32) bool {
	if len(s.Protocols) > 0 {
		for _, p := range s.Protocols {
			if p == protocol {
				return true
			}
		}
		return false
	}
	lo, hi := s.MinProtocol, s.MaxProtocol
	if lo == 0 && hi == 0 {
		return s.Protocol != 0 && protocol == s.Protocol
	}
	if lo == 0 {
		lo = hi
	}
	if hi == 0 {
		hi = lo
	}
	return protocol >= lo && protocol <= hi
}

var (
	ErrNotFound = errors.New("server not found")
	ErrConflict = errors.New("server id owned by another owner")
	idPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	sideName    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

// ReservedIDs cannot be registered because they are gateway subdomains.
var ReservedIDs = map[string]bool{"lobby": true, "api": true, "www": true, "play": true}

func Validate(s *Server) error {
	s.ID = strings.ToLower(strings.TrimSpace(s.ID))
	s.Loader = strings.ToLower(strings.TrimSpace(s.Loader))
	switch {
	case !idPattern.MatchString(s.ID):
		return fmt.Errorf("id must match %s", idPattern)
	case ReservedIDs[s.ID]:
		return fmt.Errorf("id %q is reserved", s.ID)
	case s.Address == "":
		return errors.New("address is required")
	case s.MinProtocol > s.MaxProtocol && s.MaxProtocol != 0:
		return errors.New("minProtocol > maxProtocol")
	}
	if s.Name == "" {
		s.Name = s.ID
	}
	if s.Loader == "" {
		s.Loader = "vanilla"
	}
	for i, m := range s.Mods {
		s.Mods[i] = strings.ToLower(m)
	}
	for i, m := range s.RequiredClientMods {
		s.RequiredClientMods[i] = strings.ToLower(m)
	}
	switch {
	case len(s.Mods) > 2000, len(s.Channels) > 5000, len(s.Protocols) > 1000:
		return errors.New("too many mods, channels or protocols")
	}
	for i := range s.Channels {
		s.Channels[i].Name = strings.ToLower(s.Channels[i].Name)
	}
	return validateSidePorts(s.SidePorts)
}

func validateSidePorts(ports []SidePort) error {
	if len(ports) > MaxSidePorts {
		return fmt.Errorf("at most %d side ports", MaxSidePorts)
	}
	seen := map[string]bool{}
	for i := range ports {
		p := &ports[i]
		p.Name = strings.ToLower(strings.TrimSpace(p.Name))
		p.Protocol = strings.ToLower(strings.TrimSpace(p.Protocol))
		p.Public, p.Error = nil, ""
		switch {
		case !sideName.MatchString(p.Name):
			return fmt.Errorf("side port name must match %s", sideName)
		case seen[p.Name]:
			return fmt.Errorf("duplicate side port %q", p.Name)
		case p.Protocol != "tcp" && p.Protocol != "udp":
			return fmt.Errorf("side port %q: protocol must be tcp or udp", p.Name)
		case p.Port < 1 || p.Port > 65535:
			return fmt.Errorf("side port %q: port out of range", p.Name)
		case p.PreferredPort < 0 || p.PreferredPort > 65535:
			return fmt.Errorf("side port %q: preferredPort out of range", p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

type Registry struct {
	mu      sync.RWMutex
	servers map[string]*Server
	now     func() time.Time
}

func New() *Registry {
	return &Registry{servers: map[string]*Server{}, now: time.Now}
}

// Upsert registers or refreshes a server. ttl <= 0 means static (no expiry).
// Live state from health checks is preserved across heartbeats.
func (r *Registry) Upsert(owner string, s Server, ttl time.Duration) (Server, error) {
	if err := Validate(&s); err != nil {
		return Server{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old, exists := r.servers[s.ID]
	if exists && old.Owner != owner && !r.expired(old) {
		return Server{}, ErrConflict
	}
	s.Owner = owner
	s.Static = ttl <= 0
	if !s.Static {
		s.GuardSecret = "" // registered servers are keyed by their owner token
		s.ExpiresAt = r.now().Add(ttl)
	}
	if exists && old.Address == s.Address {
		s.Online, s.VersionName, s.Protocol = old.Online, old.VersionName, old.Protocol
		s.Players, s.MaxPlayers, s.LastPing = old.Players, old.MaxPlayers, old.LastPing
		s.ForgeChannels, s.ForgeChannelsTruncated = old.ForgeChannels, old.ForgeChannelsTruncated
	}
	r.servers[s.ID] = &s
	return s, nil
}

func (r *Registry) Remove(owner, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.servers[id]
	if !ok {
		return ErrNotFound
	}
	if s.Owner != owner {
		return ErrConflict
	}
	delete(r.servers, id)
	return nil
}

func (r *Registry) expired(s *Server) bool {
	return !s.Static && r.now().After(s.ExpiresAt)
}

// Get returns a copy of a non-expired server.
func (r *Registry) Get(id string) (Server, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.servers[id]
	if !ok || r.expired(s) {
		return Server{}, false
	}
	return *s, true
}

// List returns copies of all non-expired servers sorted by ID.
func (r *Registry) List() []Server {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Server, 0, len(r.servers))
	for _, s := range r.servers {
		if !r.expired(s) {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Online returns non-expired servers whose last health check succeeded.
func (r *Registry) Online() []Server {
	all := r.List()
	out := all[:0]
	for _, s := range all {
		if s.Online {
			out = append(out, s)
		}
	}
	return out
}

// Health is the result of one status ping.
type Health struct {
	Online      bool
	Protocol    int32
	VersionName string
	Players     int
	MaxPlayers  int
	Mods        []string // discovered from forgeData/modinfo, used if none declared

	ForgeChannels          []proto.ForgeChannel
	ForgeChannelsTruncated bool
}

func (r *Registry) SetHealth(id, address string, h Health) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.servers[id]
	if !ok || s.Address != address {
		return // re-registered with a different address meanwhile
	}
	s.Online = h.Online
	s.LastPing = r.now()
	if h.Online {
		s.Protocol, s.VersionName = h.Protocol, h.VersionName
		s.Players, s.MaxPlayers = h.Players, h.MaxPlayers
		if len(s.Mods) == 0 && len(h.Mods) > 0 {
			s.Mods = h.Mods
		}
		s.ForgeChannels, s.ForgeChannelsTruncated = h.ForgeChannels, h.ForgeChannelsTruncated
	}
}

// SetSidePorts stores the gateway's assignments for a server, if it is still
// registered with the same side ports.
func (r *Registry) SetSidePorts(id string, ports []SidePort) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.servers[id]
	if !ok || len(s.SidePorts) != len(ports) {
		return
	}
	for i := range ports {
		if s.SidePorts[i].Name != ports[i].Name {
			return
		}
	}
	s.SidePorts = append([]SidePort(nil), ports...)
}

// Prune deletes expired registrations.
func (r *Registry) Prune() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.servers {
		if r.expired(s) {
			delete(r.servers, id)
		}
	}
}
