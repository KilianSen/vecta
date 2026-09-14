package match

import (
	"testing"

	"anymcp/internal/registry"
)

func servers() []registry.Server {
	return []registry.Server{
		{ID: "paper", Loader: "paper", MinProtocol: 767, MaxProtocol: 776},
		{ID: "create", Loader: "fabric", MinProtocol: 767, MaxProtocol: 767,
			Mods: []string{"create", "sodium", "jei"}, RequiredClientMods: []string{"create"}},
		{ID: "atm", Loader: "neoforge", MinProtocol: 767, MaxProtocol: 767, Mods: []string{"mekanism", "ae2"}},
		{ID: "old", Loader: "forge", MinProtocol: 763, MaxProtocol: 763, Mods: []string{"create"}},
	}
}

func ids(c []Candidate) []string {
	var out []string
	for _, x := range c {
		out = append(out, x.Server.ID)
	}
	return out
}

func TestVanillaClientOnlyGetsJoinableServers(t *testing.T) {
	got := Rank(Client{Protocol: 767, Loader: Vanilla, Mods: map[string]bool{}}, servers())
	if len(got) != 1 || got[0].Server.ID != "paper" {
		t.Fatalf("got %v", ids(got))
	}
	if Pick(got, 50, 15) == nil {
		t.Fatal("expected auto pick for single certain candidate")
	}
}

func TestFabricClientPrefersOverlappingPack(t *testing.T) {
	c := Client{Protocol: 767, Loader: Fabric, Mods: map[string]bool{"create": true, "sodium": true, "fabric-api": true}}
	got := Rank(c, servers())
	if len(got) != 2 || got[0].Server.ID != "create" {
		t.Fatalf("got %v", ids(got))
	}
	if p := Pick(got, 50, 15); p == nil || p.Server.ID != "create" {
		t.Fatalf("pick = %v", p)
	}
}

func TestMissingRequiredModExcludes(t *testing.T) {
	c := Client{Protocol: 767, Loader: Fabric, Mods: map[string]bool{"sodium": true}}
	for _, cand := range Rank(c, servers()) {
		if cand.Server.ID == "create" {
			t.Fatal("create pack must require the create mod")
		}
	}
}

func TestUnknownLoaderIsUncertain(t *testing.T) {
	got := Rank(Client{Protocol: 767, Loader: Unknown}, servers())
	if len(got) != 2 {
		t.Fatalf("got %v", ids(got))
	}
	for _, c := range got {
		if c.Server.ID == "create" && c.Certain {
			t.Fatal("fabric pack cannot be certain for an unknown client")
		}
	}
}

func TestNeoForge1201IsForgeFamily(t *testing.T) {
	if Family("neoforge", 763) != Forge || Family("neoforge", 767) != NeoForge {
		t.Fatal("neoforge family mapping")
	}
	got := Rank(Client{Protocol: 763, Loader: Forge}, servers())
	if len(got) != 1 || got[0].Server.ID != "old" {
		t.Fatalf("got %v", ids(got))
	}
}

// Regression from the e2e run: a NeoForge server whose mods were not detected
// must not tie with a real plugin server for a vanilla client.
func TestVanillaClientPrefersPlainServerOverModdedWithoutMods(t *testing.T) {
	s := []registry.Server{
		{ID: "neo", Loader: "neoforge", MinProtocol: 767, MaxProtocol: 767},
		{ID: "paper", Loader: "paper", MinProtocol: 767, MaxProtocol: 767},
	}
	p := Pick(Rank(Client{Protocol: 767, Loader: Vanilla, Mods: map[string]bool{}}, s), 50, 15)
	if p == nil || p.Server.ID != "paper" {
		t.Fatalf("pick = %+v", p)
	}
}

// A modded client whose pack is offline still gets the one server it can join.
func TestLoneCertainCandidatePickedDespiteLowScore(t *testing.T) {
	s := []registry.Server{{ID: "paper", Loader: "paper", MinProtocol: 767, MaxProtocol: 767}}
	got := Rank(Client{Protocol: 767, Loader: Fabric, Mods: map[string]bool{"appleskin": true}}, s)
	if len(got) != 1 || got[0].Score >= 50 {
		t.Fatalf("setup: %+v", got)
	}
	if Pick(got, 50, 15) == nil {
		t.Fatal("lone certain candidate must be picked")
	}
}

// Channels reported by owner plugins: namespaces add overlap, required
// channels exclude clients that lack them, optional ones don't.
func TestPluginReportedChannels(t *testing.T) {
	servers := []registry.Server{
		{ID: "neo", Loader: "neoforge", MinProtocol: 767, MaxProtocol: 767, Mods: []string{"jei"},
			Channels: []registry.Channel{
				{Name: "create:main", Version: "6", Required: true},
				{Name: "jei:recipes", Version: "3"},
				{Name: "neoforge:register", Version: "1", Required: true}, // loader channel: ignored
			}},
		{ID: "paper", Loader: "paper", MinProtocol: 767, MaxProtocol: 767},
	}
	withCreate := Client{Protocol: 767, Loader: NeoForge, Mods: map[string]bool{"create": true, "jei": true}}
	got := Rank(withCreate, servers)
	if len(got) != 2 || got[0].Server.ID != "neo" || got[0].Score <= 50 {
		t.Fatalf("client with create: %+v", got)
	}
	withoutCreate := Client{Protocol: 767, Loader: NeoForge, Mods: map[string]bool{"jei": true}}
	for _, c := range Rank(withoutCreate, servers) {
		if c.Server.ID == "neo" {
			t.Fatal("client missing a required channel's mod must be excluded")
		}
	}
}

func TestAmbiguousNoPick(t *testing.T) {
	s := []registry.Server{
		{ID: "a", Loader: "paper", MinProtocol: 767, MaxProtocol: 767},
		{ID: "b", Loader: "vanilla", MinProtocol: 767, MaxProtocol: 767},
	}
	if Pick(Rank(Client{Protocol: 767, Loader: Vanilla}, s), 50, 15) != nil {
		t.Fatal("equal candidates must not auto-pick")
	}
}
