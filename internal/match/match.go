// Package match ranks backend servers for a connecting client by version and
// loader compatibility and mod overlap.
package match

import (
	"sort"
	"strings"

	"anymcp/internal/proto"
	"anymcp/internal/registry"
)

// Client loader values. Unknown means the gateway could not tell (e.g. a
// pre-1.20.5 client without a Forge marker may be vanilla or Fabric).
const (
	Vanilla  = "vanilla"
	Fabric   = "fabric"
	Quilt    = "quilt"
	Forge    = "forge"
	NeoForge = "neoforge"
	Unknown  = "unknown"
)

// Client is what the gateway learned about a connecting player.
type Client struct {
	Protocol int32
	Loader   string
	// Mods is the set of mod IDs / channel namespaces; nil means unknown.
	Mods map[string]bool
}

// Family groups server loaders into client-compatibility families.
func Family(loader string, protocol int32) string {
	switch strings.ToLower(loader) {
	case "fabric", "quilt":
		return Fabric
	case "forge":
		return Forge
	case "neoforge":
		if protocol != 0 && protocol <= proto.Proto1_20_1 {
			return Forge // NeoForge 1.20.1 is wire-compatible with Forge
		}
		return NeoForge
	default: // vanilla, paper, spigot, purpur, folia, ...: plugins only
		return Vanilla
	}
}

// ClientFamily normalizes a detected client loader.
func ClientFamily(c Client) string {
	switch c.Loader {
	case Fabric, Quilt:
		return Fabric
	case Forge, NeoForge:
		return Family(c.Loader, c.Protocol)
	case Vanilla:
		return Vanilla
	}
	return Unknown
}

// Candidate is a compatible server with its score (0-100). Certain is false
// when compatibility could not be fully verified.
type Candidate struct {
	Server  registry.Server
	Score   int
	Certain bool
}

// ignored namespaces carry no modpack signal.
var ignored = map[string]bool{
	"minecraft": true, "forge": true, "neoforge": true, "fml": true, "fabric": true,
	"fabricloader": true, "fabric-api": true, "quilt_loader": true, "java": true, "c": true,
	"mixinextras": true, "velocity": true, "bungeecord": true,
}

func modSet(mods []string) map[string]bool {
	m := map[string]bool{}
	for _, id := range mods {
		id = strings.ToLower(id)
		if !ignored[id] && !strings.HasPrefix(id, "fabric-") {
			m[id] = true
		}
	}
	return m
}

// Rank returns compatible servers, best first.
func Rank(c Client, servers []registry.Server) []Candidate {
	cf := ClientFamily(c)
	var clientMods map[string]bool
	if c.Mods != nil {
		clientMods = map[string]bool{}
		for id := range c.Mods {
			clientMods[strings.ToLower(id)] = true
		}
		for id := range clientMods {
			if ignored[id] || strings.HasPrefix(id, "fabric-") {
				delete(clientMods, id)
			}
		}
	}

	var out []Candidate
	for _, s := range servers {
		if !s.Accepts(c.Protocol) {
			continue
		}
		cand, ok := score(c, cf, clientMods, s)
		if ok {
			out = append(out, cand)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Server.Players > out[j].Server.Players
	})
	return out
}

func score(c Client, cf string, clientMods map[string]bool, s registry.Server) (Candidate, bool) {
	sf := Family(s.Loader, c.Protocol)
	serverMods := modSet(s.Mods)
	required := modSet(s.RequiredClientMods)
	// Channels reported by owner plugins: their namespaces are what clients
	// announce, and required channels must be present on the client.
	for _, ch := range s.Channels {
		ns, _, ok := strings.Cut(ch.Name, ":")
		if !ok || ignored[ns] || strings.HasPrefix(ns, "fabric-") {
			continue
		}
		serverMods[ns] = true
		if ch.Required {
			required[ns] = true
		}
	}
	cand := Candidate{Server: s, Certain: true}

	// A modded server whose mods are all server-side (nothing required from
	// the client) behaves like vanilla for joining purposes.
	needsLoader := sf != Vanilla && (len(required) > 0 || (sf != Fabric && len(serverMods) > 0))

	switch {
	case !needsLoader:
		// A plain server for a plain client is the natural fit; a modded
		// server that merely tolerates the client, or a modded client on a
		// plain server, is a weaker one.
		cand.Score = 40
		if (sf == Vanilla && (cf == Vanilla || cf == Unknown)) || cf == sf {
			cand.Score += 20
		}
	case cf == sf:
		cand.Score = 50
	case cf == Unknown && sf == Fabric:
		cand.Score, cand.Certain = 20, false // may be a Fabric client
	default:
		return cand, false
	}

	if clientMods == nil {
		if len(required) > 0 {
			cand.Certain = false
		}
		return cand, true
	}
	for id := range required {
		if !clientMods[id] {
			return cand, false
		}
	}
	if len(serverMods) > 0 || len(clientMods) > 0 {
		inter, union := 0, len(serverMods)
		for id := range clientMods {
			if serverMods[id] {
				inter++
			} else {
				union++
			}
		}
		if union > 0 {
			cand.Score += 50 * inter / union
		}
	}
	return cand, true
}

// Pick returns the candidate to auto-route to, or nil if the choice should be
// left to the player. A lone certain candidate is always picked; otherwise the
// best must be certain, reach minScore and lead the runner-up by margin.
func Pick(cands []Candidate, minScore, margin int) *Candidate {
	if len(cands) == 0 || !cands[0].Certain {
		return nil
	}
	if len(cands) == 1 {
		return &cands[0]
	}
	if cands[0].Score < minScore || cands[0].Score-cands[1].Score < margin {
		return nil
	}
	return &cands[0]
}
