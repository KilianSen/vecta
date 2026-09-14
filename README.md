# anymcp

One public address for many independent Minecraft Java servers: any version,
vanilla, plugin or modded. Players connect to `play.example.com` and get
matched to a compatible server automatically, or pick one. Servers register
and deregister themselves at runtime.

```
players ──TCP──► NPM stream :25565 ──► anymcp gateway :25565 ──► backend servers
owners' agents ──HTTPS──► NPM proxy host ──► anymcp API :8080
```

## How routing works

The gateway reads only the handshake and login start, picks a backend, then
**replays those bytes and pipes the connection untouched**. It doesn't translate
versions or proxy the game protocol, so every version and mod loader works
once routed. Each backend authenticates players itself.

For each login it checks, in order:

1. **Route cookie**: a 1.20.5+ client returning from a transfer carries an
   HMAC-signed cookie naming its server. The handshake intent is rewritten to a
   normal login, so backends need no `accepts-transfers`.
2. **`lobby.<domain>`** always opens the lobby.
3. **`<id>.<domain>`** connects straight to that server.
4. **Remembered choice**: clients that can't transfer (pre-1.20.5) are
   matched, told to reconnect, and routed on their next join.
5. **Single candidate**: if only one online server can accept this version and
   loader, go there directly.
6. Otherwise, the **lobby**:
   - **1.20.5+**: the client is held in the configuration phase (no world
     needed). The gateway reads the client brand (`fabric`, `neoforge`,
     `vanilla`, …) and registered channels (≈ mod IDs), then ranks servers.
     - Clear winner → cookie + transfer to it.
     - **1.21.6+**, no clear winner → a native dialog lists compatible servers.
     - 1.20.5–1.21.5, no clear winner → disconnect screen with best match
       remembered and direct addresses for the others.
   - **Older**: login-phase disconnect screen with the match / address list.
     Optionally (`forgeModQuery`) Forge 1.13–1.20.1 clients are asked for their
     mod list first. Off by default: clients whose mods reject a server without
     those mods close the connection with a "mismatched mod channel list" error.

### Matching

- Version: the client protocol must be in `[minProtocol, maxProtocol]`. If
  omitted, the protocol reported by the health ping is used. Set a range if the
  backend runs ViaVersion/ViaBackwards.
- Loader: plugin servers (paper, spigot, purpur, …) accept everyone. Fabric,
  Forge and NeoForge servers need a matching client unless their mods are
  server-side only (no `requiredClientMods`, and for Forge/NeoForge no mods).
  NeoForge 1.20.1 counts as Forge.
- Score: loader match plus mod overlap (Jaccard of client mods/channels vs server
  mods). Missing a `requiredClientMods` entry excludes the server. Auto-route
  when the best is certain, ≥ `autoMinScore`, and leads the runner-up by
  ≥ `autoMargin`.

## Deployment with Nginx Proxy Manager

1. **DNS**: `play.example.com` and `*.play.example.com` → NPM host (A records).
   Wildcard is needed for direct `<id>.play.example.com` joins and `lobby.`.
   If players must use a non-default port, add an SRV record
   `_minecraft._tcp.play.example.com`.
2. **NPM → Streams → Add Stream**: incoming port `25565` (TCP) → forward to the
   gateway host, port `25565`. A stream is plain TCP forwarding, which is what
   Minecraft needs. HTTP proxy hosts cannot carry it. The hostname survives
   because the handshake is passed through.
   - Real client IPs (optional): enable `proxy_protocol` for the stream (custom
     nginx config) and set `"acceptProxyProtocol": true`. Every connection must
     then carry the header.
3. **NPM → Proxy Hosts**: `mc-api.example.com` → gateway `:8080` (with SSL).
   This serves the registration API and a public server list page.
4. Run the gateway: `anymcp gateway -config gateway.json`
   (see [examples/gateway.json](examples/gateway.json)). Set a fixed
   `cookieSecret`.

Backend servers must be reachable from the gateway. To pass real IPs to them,
set `"proxyProtocol": true` per server and enable PROXY protocol on the backend
(Paper: `proxies.proxy-protocol: true` in `config/paper-global.yml`).

> **Connection throttle:** without PROXY protocol every player reaches the
> backend from the gateway's IP. Bukkit/Paper's default `connection-throttle:
> 4000` (in `bukkit.yml`) then rejects players who join within 4 s of each
> other ("Connection throttled!"). Enable PROXY protocol, or set
> `connection-throttle: -1`. Found in the end-to-end tests.

## For server owners

Get a token from the gateway admin (`owners` in the gateway config), then run
the agent next to your server:

```
anymcp agent -config agent.json
```

See [examples/agent.json](examples/agent.json). The agent re-registers every
`interval`. If it stops, the server disappears after `registrationTTL`
(immediately on a clean shutdown). With `modsDir` it reads mod IDs from
`fabric.mod.json`, `quilt.mod.json`, `mods.toml`, `neoforge.mods.toml` and
`mcmod.info` in each jar. List mods players must have in `requiredClientMods`.

Or call the API yourself:

```
curl -X PUT https://mc-api.example.com/api/v1/servers/createpack \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"Create Pack","address":"10.0.0.31:25565","loader":"fabric",
       "minProtocol":767,"maxProtocol":767,"requiredClientMods":["create"]}'
curl -X DELETE https://mc-api.example.com/api/v1/servers/createpack -H "Authorization: Bearer $TOKEN"
curl https://mc-api.example.com/api/v1/servers
```

Server IDs are `[a-z0-9-]`, max 32 chars. `lobby`, `api`, `www` and `play`
are reserved. An ID belongs to the first owner who registers it until that
registration expires. `"hidden": true` keeps a server out of matching and
lists, so it is reachable only by its subdomain.

Protocol numbers: 763 = 1.20.1, 766 = 1.20.5/6, 767 = 1.21/1.21.1,
769 = 1.21.4, 771 = 1.21.6, 774 = 1.21.11, 775 = 26.1.x, 776 = 26.2.

## Limits and notes

- Players choose once per connection. Switching servers means reconnecting
  (`lobby.<domain>` returns to the menu). This is deliberate: modded clients
  cannot switch between different packs in one session.
- Fabric/Quilt clients don't send a mod list. Matching uses their registered
  network channel namespaces, which covers mods with networking.
- Pre-1.20.5 clients cannot be transferred, so they reconnect once after
  matching. The remembered route is keyed by username.
- Pre-1.7 legacy pings are ignored.
- Owners are trusted to register addresses. The gateway will dial whatever
  address a token holder registers.

## Development

```
go test ./...
go build -o bin/anymcp ./cmd/anymcp
```

### End-to-end tests (real servers, real protocol clients)

[test/e2e](test/e2e) runs the gateway and agents in Docker against five real servers:

| id | software | versions accepted | registered by | notes |
|---|---|---|---|---|
| paper-a | Paper 1.21.11 | 774 | static config | PROXY protocol v2 enabled |
| paper-via | Paper 1.21.4 + ViaVersion/ViaBackwards | 763–774 | agent | |
| vanilla-old | Vanilla 1.20.1 | 763 | static config | |
| fabric-pack | Fabric 1.21.1: fabric-api, appleskin, lithium | 767 | agent, mods scanned | requires appleskin |
| neo-pack | NeoForge 1.21.1: JEI | 767 | agent, mods scanned | |

```
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o test/e2e/anymcp ./cmd/anymcp
docker compose -f test/e2e/compose.yaml up -d --build   # on the Docker host
sh test/e2e/tune-paper.sh                               # once servers are healthy
npm install minecraft-protocol
node test/e2e/clients.cjs                               # GW_HOST=<docker host>
docker compose -f test/e2e/compose.yaml down -v
```

[clients.cjs](test/e2e/clients.cjs) uses node-minecraft-protocol clients (1.19.4–1.21.11). It covers:
- status
- dialog, then transfer with cookie, then spawn
- direct single-candidate routing
- Fabric mod matching
- lobby auto-match
- legacy address list
- subdomains
- a too-old client
- the `lobby.` host
- forged cookies
- agent stop/restart
- a server going offline
- PROXY protocol client IPs

Each check is verified against the backend server's own log.
