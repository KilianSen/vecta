package router

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"vecta/internal/limbo"
	"vecta/internal/match"
	"vecta/internal/proto"
	"vecta/internal/registry"
)

// lobby learns more about the client, then routes it (transfer on 1.20.5+,
// remembered route + reconnect before that) or lets the player choose.
func (r *Router) lobby(s *session) {
	if !r.lobbySlots.tryAcquire() {
		r.m.rejected.Inc("lobby-full")
		s.send(proto.LoginDisconnect(proto.C("The lobby is full right now. Please try again in a moment.", "red")))
		return
	}
	defer r.lobbySlots.release()
	r.lobbyActive.Add(1)
	defer r.lobbyActive.Add(-1)
	client := r.handshakeClient(s)
	if s.hs.Protocol >= proto.Proto1_20_5 {
		r.lobbyConfig(s, client)
		return
	}
	r.lobbyLogin(s, client)
}

// lobbyLogin handles pre-1.20.5 clients entirely inside the login phase: the
// only way to talk to them without a full play-state server is a disconnect
// screen.
func (r *Router) lobbyLogin(s *session, client match.Client) {
	marker := s.hs.Marker()
	if marker == "FML2" || marker == "FML3" {
		mods, err := r.forgeModQuery(s, client, marker == "FML3")
		switch {
		case errors.Is(err, errNoForgeProbe):
		case err != nil:
			s.log.Info("forge mod query failed", "err", err)
			return // the client closed the connection after rejecting the mod list
		default:
			client.Mods = mods
		}
	}
	var cands []match.Candidate
	for _, c := range match.Rank(client, r.visibleOnline()) {
		if !r.pending.Rejected(s.login.Name, c.Server.ID) {
			cands = append(cands, c)
		}
	}
	if pick := match.Pick(cands, r.cfg.AutoMinScore, r.cfg.AutoMargin); pick != nil && !(s.forceMenu && limbo.Supported(s.hs.Protocol)) {
		r.pending.Set(s.login.Name, pick.Server.ID, r.cfg.PendingTTL)
		s.log.Info("matched, awaiting reconnect", "server", pick.Server.ID, "score", pick.Score)
		s.send(proto.LoginDisconnect(r.reconnectText(pick.Server, cands, true)))
		return
	}
	if len(cands) == 0 || !limbo.Supported(s.hs.Protocol) {
		s.send(proto.LoginDisconnect(r.listText(s, cands)))
		return
	}
	r.limboLobby(s, client)
}

// limboLobby hosts a pre-1.20.5 client in an empty world with a clickable
// server menu. Such clients cannot be transferred, so a choice is remembered
// and the player reconnects once.
func (r *Router) limboLobby(s *session, client match.Client) {
	var uuid [16]byte
	if s.login.HasUUID {
		uuid = s.login.UUID
	}
	s.log.Info("player entered limbo")
	r.m.lobby.Inc("limbo")
	err := limbo.Serve(s.conn, s.br, limbo.Options{
		Protocol: s.hs.Protocol,
		Name:     s.login.Name,
		UUID:     uuid,
		Title:    "Choose a server",
		Log:      s.log,
		Entries: func() []limbo.Entry {
			cands := match.Rank(client, r.visibleOnline())
			entries := make([]limbo.Entry, 0, len(cands))
			for i, c := range cands {
				srv := c.Server
				entries = append(entries, limbo.Entry{
					ID:          srv.ID,
					Name:        srv.Name,
					Detail:      fmt.Sprintf("%s %s · %d/%d", srv.Loader, srv.VersionName, srv.Players, srv.MaxPlayers),
					Recommended: i == 0 && c.Certain,
				})
			}
			return entries
		},
		Choose: func(id string) (*proto.Text, string) {
			srv, ok := r.reg.Get(id)
			if !ok || !srv.Online || !srv.Accepts(s.hs.Protocol) {
				return nil, "That server is not available right now."
			}
			r.pending.Set(s.login.Name, srv.ID, r.cfg.PendingTTL)
			s.log.Info("player chose server in limbo", "server", srv.ID)
			msg := proto.Join(
				proto.C("You picked ", "gray"), proto.C(srv.Name, "green"), proto.C(".\n\n", "gray"),
				proto.C("Reconnect now to join it.", "white"),
			)
			return &msg, ""
		},
	})
	if err != nil {
		s.log.Debug("limbo ended", "err", err)
	}
}

var errNoForgeProbe = errors.New("no forge backend to probe with")

// forgeModQuery asks a Forge 1.13-1.20.1 client for its mod list, presenting
// the channel list of a candidate Forge backend. A client with the same pack
// accepts it; one that closes the connection is incompatible with that
// backend, which is then skipped for this player for a while.
func (r *Router) forgeModQuery(s *session, client match.Client, fml3 bool) (map[string]bool, error) {
	var probe *registry.Server
	for _, c := range match.Rank(client, r.visibleOnline()) {
		srv := c.Server
		if len(srv.ForgeChannels) > 0 && !srv.ForgeChannelsTruncated && !r.pending.Rejected(s.login.Name, srv.ID) {
			probe = &srv
			break
		}
	}
	var mods []string
	var channels []proto.ForgeChannel
	switch {
	case probe != nil:
		mods, channels = probe.Mods, probe.ForgeChannels
	case r.cfg.ForgeModQuery:
		// Empty lists: clients with strict mods will refuse this.
	default:
		return nil, errNoForgeProbe
	}
	got, err := r.queryForgeMods(s, fml3, mods, channels)
	if err != nil && probe != nil {
		r.pending.Reject(s.login.Name, probe.ID, 10*time.Minute)
		s.log.Info("forge client rejected backend mod list", "server", probe.ID)
	}
	return got, err
}

func (r *Router) queryForgeMods(s *session, fml3 bool, mods []string, channels []proto.ForgeChannel) (map[string]bool, error) {
	const msgID = 1
	if err := s.send(proto.FMLModListRequest(msgID, fml3, mods, channels)); err != nil {
		return nil, err
	}
	_, payload, err := proto.ReadFrame(s.br)
	if err != nil {
		return nil, err
	}
	b := proto.NewBuffer(payload)
	if id, _ := b.VarInt(); id != proto.LoginPluginResponseID {
		return nil, fmt.Errorf("expected login plugin response, got 0x%02x", id)
	}
	if id, _ := b.VarInt(); id != msgID {
		return nil, fmt.Errorf("unexpected plugin message id %d", id)
	}
	if ok, err := b.Bool(); err != nil || !ok {
		return nil, fmt.Errorf("client did not understand fml handshake")
	}
	clientMods, clientChannels, err := proto.ParseFMLModListReply(b.Remaining())
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, m := range clientMods {
		set[m] = true
	}
	for _, c := range clientChannels {
		ns, _, _ := strings.Cut(c, ":")
		set[ns] = true
	}
	return set, nil
}

// readFrames pumps client packets into a channel so the lobby can wait on
// packets and timers together.
func (s *session) readFrames() <-chan []byte {
	ch := make(chan []byte, 16)
	go func() {
		defer close(ch)
		for {
			_, payload, err := proto.ReadFrame(s.br)
			if err != nil {
				return
			}
			ch <- payload
		}
	}()
	return ch
}

// lobbyConfig completes login and holds the client in the configuration
// phase, which needs no world, registries or chunks.
func (r *Router) lobbyConfig(s *session, client match.Client) {
	s.conn.SetDeadline(time.Now().Add(6 * time.Minute))
	uuid := s.login.UUID
	if err := s.send(proto.LoginSuccess(s.hs.Protocol, uuid, s.login.Name)); err != nil {
		return
	}
	frames := s.readFrames()
	if !waitLoginAck(frames) {
		return
	}
	// The NeoForge query must precede the brand: a NeoForge client that sees
	// a brand first treats us as vanilla and disconnects if any of its mods
	// require the server. It answers with its full channel list; other
	// clients ignore the unknown channel.
	s.send(
		proto.NewPacket(proto.CfgPluginMessageOutID).String("neoforge:register").VarInt(0).Frame(),
		proto.NewPacket(proto.CfgPluginMessageOutID).String("minecraft:brand").String("vecta").Frame(),
	)

	probe := probeClient(frames, r.cfg.ProbeWindow)
	if l := brandLoader(probe.brand); !probe.neoforge && (l == match.Fabric || l == match.Quilt) && len(probe.channels) == 0 {
		// Fabric API announces its channels only once the server starts the
		// common registration handshake. c:version must come first: the client
		// rejects a c:register whose version it has not negotiated. Only sent
		// to Fabric clients; NeoForge ones disconnect on unnegotiated payloads.
		s.send(
			proto.NewPacket(proto.CfgPluginMessageOutID).String("minecraft:register").Raw([]byte("c:version\x00c:register")).Frame(),
			proto.NewPacket(proto.CfgPluginMessageOutID).String("c:version").VarInt(1).VarInt(1).Frame(),
			proto.NewPacket(proto.CfgPluginMessageOutID).String("c:register").VarInt(1).String("play").VarInt(0).Frame(),
		)
		more := probeClient(frames, r.cfg.ProbeWindow)
		for ns := range more.channels {
			probe.channels[ns] = true
		}
	}
	if l := brandLoader(probe.brand); l != match.Unknown {
		client.Loader = l
	}
	if probe.neoforge {
		client.Loader = match.NeoForge
	}
	switch {
	case client.Loader == match.Vanilla:
		client.Mods = map[string]bool{}
	case len(probe.channels) > 0:
		client.Mods = probe.channels
	}
	s.log.Debug("client probed", "brand", probe.brand, "loader", client.Loader,
		"channels", len(probe.channels), "neoforge", probe.neoforge)

	cands := match.Rank(client, r.visibleOnline())
	menuAvailable := s.hs.Protocol >= proto.Proto1_21_6
	if pick := match.Pick(cands, r.cfg.AutoMinScore, r.cfg.AutoMargin); pick != nil && !(s.forceMenu && menuAvailable) {
		s.log.Info("auto-matched", "server", pick.Server.ID, "score", pick.Score)
		r.transfer(s, frames, pick.Server)
		return
	}
	if len(cands) == 0 {
		s.send(cfgDisconnect(r.listText(s, cands)))
		return
	}
	if s.hs.Protocol < proto.Proto1_21_6 {
		// No UI in the configuration phase before dialogs existed.
		best := cands[0].Server
		r.pending.Set(s.login.Name, best.ID, r.cfg.PendingTTL)
		s.send(cfgDisconnect(r.reconnectText(best, cands, false)))
		return
	}
	r.dialogLoop(s, frames, cands)
}

func waitLoginAck(frames <-chan []byte) bool {
	timeout := time.After(10 * time.Second)
	for {
		select {
		case p, ok := <-frames:
			if !ok {
				return false
			}
			if len(p) > 0 && p[0] == proto.LoginAcknowledgedID {
				return true
			}
		case <-timeout:
			return false
		}
	}
}

type clientProbe struct {
	brand    string
	channels map[string]bool // channel namespaces (~ mod ids)
	neoforge bool            // answered the neoforge:register query
}

// probeClient collects the client brand and registered channel namespaces.
func probeClient(frames <-chan []byte, window time.Duration) clientProbe {
	pr := clientProbe{channels: map[string]bool{}}
	timeout := time.After(window)
	for {
		select {
		case p, ok := <-frames:
			if !ok {
				return pr
			}
			b := proto.NewBuffer(p)
			if id, _ := b.VarInt(); id != proto.CfgPluginMessageInID {
				continue
			}
			ch, err := b.String(32767)
			if err != nil {
				continue
			}
			switch ch {
			case "minecraft:brand":
				pr.brand, _ = b.String(32767)
			case "neoforge:register":
				pr.neoforge = true
				for _, id := range parseNeoForgeQuery(b.Remaining()) {
					if ns, _, ok := strings.Cut(id, ":"); ok && ns != "" {
						pr.channels[ns] = true
					}
				}
			case "c:register":
				// Fabric common protocol: VarInt version, phase, identifier set.
				if _, err := b.VarInt(); err != nil {
					continue
				}
				if _, err := b.String(64); err != nil {
					continue
				}
				n, err := b.VarInt()
				if err != nil {
					continue
				}
				for i := int32(0); i < n; i++ {
					id, err := b.String(32767)
					if err != nil {
						break
					}
					if ns, _, ok := strings.Cut(id, ":"); ok && ns != "" {
						pr.channels[ns] = true
					}
				}
			case "c:version":
			case "minecraft:register", "fabric:register":
				for _, c := range strings.Split(string(b.Remaining()), "\x00") {
					if ns, _, ok := strings.Cut(c, ":"); ok && ns != "" {
						pr.channels[ns] = true
					}
				}
			default:
				if ns, _, ok := strings.Cut(ch, ":"); ok && ns != "minecraft" {
					pr.channels[ns] = true
				}
			}
		case <-timeout:
			return pr
		}
	}
}

// parseNeoForgeQuery decodes a NeoForge client's neoforge:register reply:
// for each connection protocol, the payload ids it registered. Parsing stops
// at the first malformed entry and returns what was read.
func parseNeoForgeQuery(data []byte) []string {
	var ids []string
	b := proto.NewBuffer(data)
	protocols, err := b.VarInt()
	if err != nil {
		return nil
	}
	for i := int32(0); i < protocols; i++ {
		if _, err := b.VarInt(); err != nil { // protocol ordinal
			return ids
		}
		n, err := b.VarInt()
		if err != nil {
			return ids
		}
		for j := int32(0); j < n; j++ {
			id, err := b.String(32767)
			if err != nil {
				return ids
			}
			if _, err := b.String(32767); err != nil { // version
				return ids
			}
			hasFlow, err := b.Bool()
			if err != nil {
				return ids
			}
			if hasFlow {
				if _, err := b.VarInt(); err != nil {
					return ids
				}
			}
			if _, err := b.Bool(); err != nil { // optional
				return ids
			}
			ids = append(ids, id)
		}
	}
	return ids
}

func brandLoader(brand string) string {
	b := strings.ToLower(brand)
	switch {
	case strings.Contains(b, "quilt"):
		return match.Quilt
	case strings.Contains(b, "fabric"):
		return match.Fabric
	case strings.Contains(b, "neoforge"):
		return match.NeoForge
	case strings.Contains(b, "forge"):
		return match.Forge
	case b == "vanilla":
		return match.Vanilla
	}
	return match.Unknown
}

const (
	actionJoinPrefix = "vecta:join/"
	actionLeave      = "vecta:leave"
	maxDialogServers = 30
)

func (r *Router) dialogLoop(s *session, frames <-chan []byte, cands []match.Candidate) {
	if err := s.send(showDialog(s, cands)); err != nil {
		return
	}
	keepAlive := time.NewTicker(10 * time.Second)
	defer keepAlive.Stop()
	timeout := time.After(5 * time.Minute)
	for {
		select {
		case p, ok := <-frames:
			if !ok {
				return
			}
			b := proto.NewBuffer(p)
			if id, _ := b.VarInt(); id != proto.CfgCustomClickActionID {
				continue
			}
			action, err := b.String(32767)
			if err != nil {
				continue
			}
			if action == actionLeave {
				s.send(cfgDisconnect(proto.C("See you!", "gray")))
				return
			}
			id, ok := strings.CutPrefix(action, actionJoinPrefix)
			if !ok {
				continue
			}
			srv, ok := r.reg.Get(id)
			if !ok || !srv.Online || !srv.Accepts(s.hs.Protocol) {
				s.send(cfgDisconnect(proto.Join(proto.C(id, "yellow"), proto.C(" is no longer available. Reconnect to pick again.", "red"))))
				return
			}
			s.log.Info("player chose server", "server", srv.ID)
			r.transfer(s, frames, srv)
			return
		case <-keepAlive.C:
			if err := s.send(proto.NewPacket(proto.CfgKeepAliveOutID).Int64(time.Now().UnixMilli()).Frame()); err != nil {
				return
			}
		case <-timeout:
			s.send(cfgDisconnect(proto.C("Timed out choosing a server.", "red")))
			return
		}
	}
}

func showDialog(s *session, cands []match.Candidate) []byte {
	if len(cands) > maxDialogServers {
		cands = cands[:maxDialogServers]
	}
	actions := make(proto.List, 0, len(cands))
	for _, c := range cands {
		srv := c.Server
		tip := fmt.Sprintf("%s %s · %d/%d players", srv.Loader, srv.VersionName, srv.Players, srv.MaxPlayers)
		if srv.Description != "" {
			tip = srv.Description + "\n" + tip
		}
		actions = append(actions, proto.Compound{
			{Name: "label", Value: proto.C(srv.Name, "green").NBT()},
			{Name: "tooltip", Value: proto.T(tip).NBT()},
			{Name: "width", Value: 200},
			{Name: "action", Value: proto.Compound{
				{Name: "type", Value: "custom"},
				{Name: "id", Value: actionJoinPrefix + srv.ID},
			}},
		})
	}
	columns := 1
	if len(cands) > 5 {
		columns = 2
	}
	body := fmt.Sprintf("Hi %s! These servers accept %s. Best matches for your client come first.",
		s.login.Name, proto.VersionName(s.hs.Protocol))
	dialog := proto.Compound{
		{Name: "type", Value: "minecraft:multi_action"},
		{Name: "title", Value: proto.C("Choose a server", "gold").NBT()},
		{Name: "body", Value: proto.List{proto.Compound{
			{Name: "type", Value: "minecraft:plain_message"},
			{Name: "contents", Value: proto.T(body).NBT()},
			{Name: "width", Value: 300},
		}}},
		{Name: "actions", Value: actions},
		{Name: "columns", Value: columns},
		{Name: "can_close_with_escape", Value: false},
		{Name: "exit_action", Value: proto.Compound{
			{Name: "label", Value: proto.T("Disconnect").NBT()},
			{Name: "action", Value: proto.Compound{
				{Name: "type", Value: "custom"},
				{Name: "id", Value: actionLeave},
			}},
		}},
	}
	return proto.NewPacket(proto.CfgShowDialogID).Raw(proto.NetworkNBT(dialog)).Frame()
}

// transfer stores a signed route cookie and sends the client back to the
// gateway, which then pipes the new connection to srv.
func (r *Router) transfer(s *session, frames <-chan []byte, srv registry.Server) {
	r.m.lobby.Inc("transfer")
	host := r.cfg.TransferHost
	if host == "" {
		host = r.cfg.Domain
	}
	if host == "" {
		host = s.hs.Host()
	}
	port := r.cfg.TransferPort
	if port == 0 {
		port = int(s.hs.Port)
	}
	cookie := signRoute(r.cfg.CookieSecret, srv.ID, s.login.Name, time.Now().Add(2*time.Minute))
	err := s.send(
		proto.NewPacket(proto.CfgStoreCookieID).String(CookieKey).ByteArray(cookie).Frame(),
		proto.NewPacket(proto.CfgTransferID).String(host).VarInt(int32(port)).Frame(),
	)
	if err != nil {
		return
	}
	// Let the client close the connection itself so it doesn't show an error.
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-frames:
			if !ok {
				return
			}
		case <-timeout:
			return
		}
	}
}

func cfgDisconnect(t proto.Text) []byte {
	return proto.NewPacket(proto.CfgDisconnectID).Raw(proto.NetworkNBT(t.NBT())).Frame()
}

func (r *Router) serverAddress(srv registry.Server) string {
	if r.cfg.Domain == "" {
		return ""
	}
	return srv.ID + "." + r.cfg.Domain
}

// reconnectText tells a client that cannot be transferred or shown a menu
// where its next connection will go. sure is false when chosen is only the
// best of several equally good candidates.
func (r *Router) reconnectText(chosen registry.Server, cands []match.Candidate, sure bool) proto.Text {
	lead := "Matched you with "
	if !sure {
		lead = "Several servers fit. Best guess: "
	}
	parts := []proto.Text{
		proto.C(lead, "gray"),
		proto.C(chosen.Name, "green"),
		proto.C(fmt.Sprintf(" (%s %s).\n", chosen.Loader, chosen.VersionName), "gray"),
		proto.C("Reconnect within a few minutes to join.\n", "white"),
	}
	if len(cands) > 1 && r.cfg.Domain != "" {
		parts = append(parts, proto.C("\nOr join another server directly:\n", "gray"))
		parts = append(parts, r.addressLines(cands, chosen.ID)...)
	}
	return proto.Join(parts...)
}

func (r *Router) listText(s *session, cands []match.Candidate) proto.Text {
	if len(cands) == 0 {
		parts := []proto.Text{proto.C(fmt.Sprintf("No online server accepts %s", proto.VersionName(s.hs.Protocol)), "red")}
		if loader := r.handshakeClient(s).Loader; loader != match.Unknown {
			parts = append(parts, proto.C(" with "+loader, "red"))
		}
		parts = append(parts, proto.C(".\n\nOnline servers:\n", "gray"))
		online := r.visibleOnline()
		if len(online) == 0 {
			parts = append(parts, proto.C("none", "dark_gray"))
		}
		for i, srv := range online {
			if i == 12 {
				parts = append(parts, proto.C(fmt.Sprintf("... and %d more", len(online)-i), "dark_gray"))
				break
			}
			parts = append(parts, proto.C(fmt.Sprintf("%s: %s %s\n", srv.Name, srv.Loader, srv.VersionName), "white"))
		}
		return proto.Join(parts...)
	}
	parts := []proto.Text{proto.C("Several servers fit your client. ", "gold")}
	if r.cfg.Domain == "" {
		parts = append(parts, proto.C("Ask an admin to configure a domain for direct joins.\n", "gray"))
		return proto.Join(parts...)
	}
	parts = append(parts, proto.C("Join one by its address:\n\n", "gray"))
	parts = append(parts, r.addressLines(cands, "")...)
	return proto.Join(parts...)
}

func (r *Router) addressLines(cands []match.Candidate, skip string) []proto.Text {
	var out []proto.Text
	n := 0
	for _, c := range cands {
		if c.Server.ID == skip {
			continue
		}
		if n == 10 {
			out = append(out, proto.C(fmt.Sprintf("... and %d more\n", len(cands)-n), "dark_gray"))
			break
		}
		out = append(out,
			proto.C(c.Server.Name, "green"),
			proto.C(fmt.Sprintf(" (%s %s): ", c.Server.Loader, c.Server.VersionName), "gray"),
			proto.C(r.serverAddress(c.Server)+"\n", "aqua"),
		)
		n++
	}
	return out
}
