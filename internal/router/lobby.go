package router

import (
	"fmt"
	"strings"
	"time"

	"anymcp/internal/match"
	"anymcp/internal/proto"
	"anymcp/internal/registry"
)

// lobby learns more about the client, then routes it (transfer on 1.20.5+,
// remembered route + reconnect before that) or lets the player choose.
func (r *Router) lobby(s *session) {
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
	if r.cfg.ForgeModQuery && (marker == "FML2" || marker == "FML3") {
		mods, err := r.queryForgeMods(s, marker == "FML3")
		if err != nil {
			s.log.Info("forge mod query failed", "err", err)
			return // the client closes the connection when it rejects our mod list
		}
		client.Mods = mods
	}
	cands := match.Rank(client, r.visibleOnline())
	if pick := match.Pick(cands, r.cfg.AutoMinScore, r.cfg.AutoMargin); pick != nil {
		r.pending.Set(s.login.Name, pick.Server.ID, r.cfg.PendingTTL)
		s.log.Info("matched, awaiting reconnect", "server", pick.Server.ID, "score", pick.Score)
		s.send(proto.LoginDisconnect(r.reconnectText(pick.Server, cands, true)))
		return
	}
	s.send(proto.LoginDisconnect(r.listText(s, cands)))
}

func (r *Router) queryForgeMods(s *session, fml3 bool) (map[string]bool, error) {
	const msgID = 1
	if err := s.send(proto.FMLModListRequest(msgID, fml3)); err != nil {
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
	mods, channels, err := proto.ParseFMLModListReply(b.Remaining())
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, m := range mods {
		set[m] = true
	}
	for _, c := range channels {
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
	s.send(proto.NewPacket(proto.CfgPluginMessageOutID).String("minecraft:brand").String("anymcp").Frame())

	brand, channels := probeClient(frames, r.cfg.ProbeWindow)
	if l := brandLoader(brand); l != match.Unknown {
		client.Loader = l
	}
	switch {
	case client.Loader == match.Vanilla:
		client.Mods = map[string]bool{}
	case len(channels) > 0:
		client.Mods = channels
	}
	s.log.Debug("client probed", "brand", brand, "loader", client.Loader, "channels", len(channels))

	cands := match.Rank(client, r.visibleOnline())
	if pick := match.Pick(cands, r.cfg.AutoMinScore, r.cfg.AutoMargin); pick != nil {
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

// probeClient collects the client brand and registered channel namespaces.
func probeClient(frames <-chan []byte, window time.Duration) (string, map[string]bool) {
	brand := ""
	channels := map[string]bool{}
	timeout := time.After(window)
	for {
		select {
		case p, ok := <-frames:
			if !ok {
				return brand, channels
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
				brand, _ = b.String(32767)
			case "minecraft:register", "c:register", "fabric:register":
				for _, c := range strings.Split(string(b.Remaining()), "\x00") {
					if ns, _, ok := strings.Cut(c, ":"); ok && ns != "" {
						channels[ns] = true
					}
				}
			default:
				if ns, _, ok := strings.Cut(ch, ":"); ok && ns != "minecraft" {
					channels[ns] = true
				}
			}
		case <-timeout:
			return brand, channels
		}
	}
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
	actionJoinPrefix = "anymcp:join/"
	actionLeave      = "anymcp:leave"
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
