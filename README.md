<div align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://logo.kiliansen.de/vecta/dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="https://logo.kiliansen.de/vecta/light.svg">
    <img alt="VectaMine" src="https://logo.kiliansen.de/vecta/light.svg" height="88">
  </picture>
</div>

One public address for many independent Minecraft Java servers: any version,
vanilla, plugin or modded. Players connect to `play.example.com` and get
matched to a compatible server automatically, or pick one. Servers register
and deregister themselves at runtime. Owners just add the vecta server jar,
which works on any loader and version, and can guard the server so it's only
reachable through the gateway.

```
players ──TCP──► NPM stream :25565 ──► vecta gateway :25565 ──► backend servers (optional guard)
owners' server jar / agent / plugin ──HTTPS──► NPM proxy host ──► vecta API :8080
```

## How routing works

The gateway reads only the handshake and login start, picks a backend, then
**replays those bytes and pipes the connection untouched**. It doesn't translate
versions or proxy the game protocol, so every version and mod loader works
once routed. Each backend authenticates players itself.

For each login it checks, in order:

1. **Route cookie.** A 1.20.5+ client returning from a transfer carries an
   HMAC-signed cookie naming its server, or `lobby`. The handshake intent is
   rewritten to a normal login, so backends need no `accepts-transfers`.
   Cookies come from the lobby and from the server jar's `/hub` and `/server`.
2. **`lobby.<domain>`** always opens the lobby. It shows the menu even when one
   server is the obvious match: the 1.21.6+ dialog, or the limbo before 1.20.5.
3. **`<id>.<domain>`** connects straight to that server.
4. **Remembered choice.** Clients that can't transfer (pre-1.20.5) pick in the
   limbo, are matched, or use a plugin's `/server`. The choice is remembered
   (`pendingTTL`), and their next plain join goes there.
5. **Sticky last server.** A returning player goes back to the server they
   last played on, if it's online, accepts their version and fits what the
   handshake shows (`stickyTTL`).
6. **Single candidate.** If exactly one visible online server accepts the
   client version at all, and it certainly fits, the player goes straight
   there. When several servers share the version, the lobby first learns the
   loader and mods, because the handshake alone can't tell a NeoForge or Fabric
   client from vanilla.
7. Otherwise, the **lobby**:
   - **1.20.5+**: the client is held in the configuration phase, which needs
     no world.
     - **NeoForge query first.** The gateway sends a `neoforge:register` query
       before its brand. Without it, a NeoForge client with server-required mods
       disconnects as soon as it sees a non-NeoForge brand. NeoForge clients
       answer with their full channel list; other clients ignore it.
     - **Brand and channels.** The gateway reads the client brand (`fabric`,
       `neoforge`, `vanilla`, …) and registered channels (≈ mod IDs).
     - **Fabric handshake.** Fabric/Quilt clients announce their mod channels
       only after the server starts Fabric's common handshake, so the gateway
       sends `c:version` and `c:register` once the brand identifies Fabric.
     - **Outcome:**
       - clear winner → cookie + transfer to it;
       - **1.21.6+**, no clear winner → a native dialog lists compatible servers;
       - 1.20.5–1.21.5, no clear winner → disconnect screen with the best match
         remembered and direct addresses for the others.
   - **1.7.10–1.20.4**:
     - A clear match gets "matched you with X, reconnect".
     - Otherwise the player joins the **limbo**, an empty void world
       ([internal/limbo](internal/limbo)) that speaks the client's own version.
       It shows a clickable chat menu (`/join <id>`, `/servers`). Choosing a
       server remembers it and asks the player to reconnect once.
     - The limbo never starts a Forge handshake, so legacy Forge, Forge 1.13+
       and Fabric clients join it like a vanilla server.
     - With no fitting server at all, the player gets a disconnect screen
       explaining why.
   - **Forge 1.13–1.20.1** (`FML2`/`FML3` marker): before any menu, the gateway
     asks the client for its mod list.
     - It presents the exact channel list of a compatible Forge backend, read
       from that backend's status ping. A client with the same pack accepts it.
     - A client that refuses closes the connection. That backend is then
       skipped for this player for 10 minutes, and the next join probes another.
     - `forgeModQuery: true` also allows an empty-list query when no backend
       channel data exists. Strict mods refuse it.

### Matching

- **Version:**
  - The client protocol must be in the server's `protocols` list if reported
    (the server jar sends ViaVersion's exact list), else in
    `[minProtocol, maxProtocol]`.
  - With neither, only the protocol reported by the health ping is accepted.
  - Declare a range if the backend runs ViaVersion/ViaBackwards/ViaRewind.
- **Loader:**
  - Plugin servers (paper, spigot, purpur, folia, …) accept everyone.
  - Fabric, Forge and NeoForge servers need a matching client, unless their
    mods are server-side only: no `requiredClientMods` and no required channels,
    and for Forge/NeoForge no mods.
  - NeoForge 1.20.1 counts as Forge.
- **Mods:**
  - Server mods are the declared or scanned mod IDs plus the namespaces of
    reported `channels`.
  - Missing a `requiredClientMods` entry, or the mod behind a required channel,
    excludes the server.
- **Score:**
  - Loader fit plus mod overlap (Jaccard of client mods/channels vs server mods).
  - Auto-route when the best candidate is certain, ≥ `autoMinScore`, and leads
    the runner-up by ≥ `autoMargin`. A lone certain candidate is always picked.
- **Hidden servers** (`"hidden": true`) are never matched or listed. They are
  still reachable by subdomain, sticky routing and plugin `/server` transfers.

## Deployment with Nginx Proxy Manager

1. **DNS:** point `play.example.com` and `*.play.example.com` at the NPM host
   (A records). The wildcard is needed for direct `<id>.play.example.com` joins
   and for `lobby.`. If players must use a non-default port, add an SRV record
   `_minecraft._tcp.play.example.com`.
2. **NPM → Streams → Add Stream:** incoming port `25565` (TCP), forwarded to
   the gateway host on port `25565`.
   - A stream is plain TCP forwarding, which is what Minecraft needs; HTTP proxy
     hosts cannot carry it.
   - The hostname survives because the handshake is passed through.
   - **Real client IPs** (recommended): enable `proxy_protocol` for the stream
     (custom nginx config) and set `"acceptProxyProtocol": true`. Every
     connection must then carry the header. Per-IP rate limits and the
     personalized MOTD depend on it.
3. **NPM → Proxy Hosts:** `mc-api.example.com` → gateway `:8080`, with SSL.
   This serves the owner API, the public server list page and `/metrics`.
4. **Run the gateway:** `vecta gateway -config gateway.json`
   (see [examples/gateway.json](examples/gateway.json)). Set a fixed
   `cookieSecret` and a persistent `dataFile` path.

Backend servers must be reachable from the gateway, ideally only from it. To
pass real IPs to them, set `"proxyProtocol": true` per server and enable PROXY
protocol on the backend (Paper: `proxies.proxy-protocol: true` in
`config/paper-global.yml`).

> **Connection throttle:** without PROXY protocol, every player reaches the
> backend from the gateway's IP. Bukkit/Paper's default `connection-throttle:
> 4000` (in `bukkit.yml`) then rejects players who join within 4 s of each
> other ("Connection throttled!"). Enable PROXY protocol, or set
> `connection-throttle: -1`. This was found in the end-to-end tests.

## Docker Compose deployment

[deploy/](deploy) contains a ready-to-use setup:

| File | Purpose |
|---|---|
| [Dockerfile](Dockerfile) | Multi-stage build to a static binary on distroless `nonroot`, with a built-in health check. Multi-arch via buildx. |
| [deploy/compose.yaml](deploy/compose.yaml) | The gateway: config mount, persistent `vecta-data` volume, read-only root filesystem, dropped capabilities, log rotation. |
| [deploy/gateway.json](deploy/gateway.json) | Config template using `${VAR}` placeholders. |
| [deploy/.env.example](deploy/.env.example) | Domain, secrets, tokens and ports. |
| [deploy/compose.server-example.yaml](deploy/compose.server-example.yaml) | Example backend: a Minecraft server with the vecta server jar as a Java agent and the guard on. |
| [deploy/agent.json](deploy/agent.json) | Config for the sidecar agent, if you use it instead of the jar. |

```
cd deploy
cp .env.example .env          # set VECTA_DOMAIN, VECTA_COOKIE_SECRET, VECTA_METRICS_TOKEN, VECTA_OWNER_TOKEN
docker compose up -d --build
docker compose ps             # STATUS shows (healthy) once the API answers
```

**Config and secrets:**
- Config files (gateway and agent) support `${VAR}` and `${VAR:-default}`,
  filled from the container environment. Compose loads `.env` into it.
- Values are JSON-escaped, and `$$` is a literal `$`.
- A referenced variable that is unset, with no default, stops the gateway at
  startup. A missing secret can't silently become empty.

**Image:**
- The default command is `gateway -config /etc/vecta/gateway.json`.
- `vecta version` prints the build version; `vecta healthcheck` is the
  container health check. It reads `VECTA_HEALTH_URL` if you move `apiListen`.
- It runs as uid 65532. `/data` is the working directory and volume, so relative
  `dataFile` paths persist there.

**Nginx Proxy Manager:**
- **Separate host or host network:** keep the published ports and point NPM's
  stream at `<host>:25565` and its proxy host at `<host>:8080`.
- **Container on the same Docker host:**
  1. Remove `ports`.
  2. Uncomment the `npm` network in `compose.yaml`.
  3. Point NPM at `vecta:25565` (stream) and `vecta:8080` (proxy host).
- **Real client IPs:** enable PROXY protocol on the stream and set
  `"acceptProxyProtocol": true`.

**Operations:**

| Task | How |
|---|---|
| Upgrade | `git pull && docker compose up -d --build`, or set `VECTA_IMAGE` to a published tag and `docker compose pull && docker compose up -d`. |
| Back up state | `docker run --rm -v vecta_vecta-data:/data -v "$PWD":/backup alpine cp /data/state.json /backup/` |
| Publish an image | `docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=1.0.0 -t registry.example.com/vecta:1.0.0 --push .` |

**Backend servers:**
- On another host, put `vecta.jar` next to
  [deploy/compose.server-example.yaml](deploy/compose.server-example.yaml)
  and run it with `VECTA_TOKEN` and `VECTA_ADDRESS` set.
- Or add the jar to an existing server (see [For server owners](#for-server-owners)).

## Configuration reference (`gateway.json`)

Durations are strings like `"10s"`, `"5m"` or `"720h"`. Unknown keys are
rejected at startup.

| Key | Default | Meaning |
|---|---|---|
| `listen` | `:25565` | Minecraft listener. |
| `apiListen` | `:8080` | HTTP API, status page and metrics. |
| `domain` | none | Public base hostname. Enables `<id>.<domain>`, `lobby.<domain>` and the addresses shown to players. |
| `transferHost` / `transferPort` | `domain` / the client's port | Where transfers (lobby and plugin tickets) send 1.20.5+ clients. Plugin tickets use port 25565 when `transferPort` is unset. |
| `cookieSecret` | random at startup | HMAC key for route cookies. Set it, or transfers in flight break on restart. |
| `acceptProxyProtocol` | false | Require a PROXY v1/v2 header on every Minecraft connection. |
| `registrationTTL` | `45s` | A registration expires this long after its last heartbeat. |
| `healthInterval` | `10s` | Status-ping interval for all servers. |
| `autoMinScore` / `autoMargin` | 50 / 15 | Auto-match thresholds. |
| `probeWindow` | `1500ms` | How long the 1.20.5+ lobby listens for brand and channels (again after the Fabric handshake). |
| `pendingTTL` | `3m` | How long a remembered choice waits for the player to reconnect. |
| `forgeModQuery` | false | Also query Forge 1.13–1.20.1 clients with an empty mod list when no backend channel data exists. |
| `motd` | `vecta gateway` | First line of the server-list MOTD. |
| `dataFile` | `vecta-state.json` (`"-"`: memory only) | Persists remembered routes, Forge rejections, sticky servers and IP→player for the MOTD. Written atomically every 30 s and on shutdown. |
| `stickyTTL` | `720h` (negative: off) | How long a player's last server is remembered for sticky routing. |
| `personalizedMotd` | false | Server-list second line becomes "Next join: X" or "Back to X · lobby.<domain> to switch", looked up by client IP. |
| `rateLimit.perSecond` / `.burst` / `.maxConnectionsPerIp` | off | Per-IP token bucket and concurrent connection cap. Refused connections are closed before the handshake is parsed. |
| `maxLobbySessions` | 500 (0: unlimited) | Concurrent lobby/limbo sessions. Beyond that, players see "The lobby is full". |
| `allowedNetworks` | anything but loopback, link-local, cloud metadata, unspecified, multicast | CIDRs owners may register backends in. Checked at registration and on every dial, so DNS rebinding can't bypass it. |
| `allowLoopbackBackends` | false | Allow 127.0.0.0/8 and ::1 backends (single-host setups). |
| `metricsToken` | none (open) | Bearer token required for `GET /metrics`. |
| `owners` | none | `name → token`, or `name → {"token": ..., "allowedNetworks": [...]}`. An owner-level list replaces the global one. |
| `servers` | none | Static servers, same fields as a registration (see [docs/owner-api.md](docs/owner-api.md)). Trusted and dialed without the address policy. A static server may set `guard: true` with a `guardSecret` that matches the jar's `token`. |

## For server owners

Get an owner token from the gateway admin. There are three ways to register a
server:

1. **The vecta server jar** (recommended), from
   [plugins/universal](plugins/universal). One jar for vanilla, Paper/Spigot,
   Fabric/Quilt, Forge (1.7.10+) and NeoForge, on Java 8 and newer.

   ```
   java -jar vecta.jar paper.jar nogui               # wrapper mode
   java -javaagent:vecta.jar -jar server.jar nogui   # agent mode (or in user_jvm_args.txt for run.sh)
   ```

   - **Configuration:** `vecta.properties` (created on first start) or `VECTA_*`
     environment variables.
   - **Detection:** it pings the local server and reads the mods, plugins and
     network channels that actually loaded, by reflection on each platform. It
     also reads ViaVersion's protocol list. Where that fails, it falls back to
     scanning the mod jars.
   - **What it reports:** loader, software, client-relevant mods (without
     platform IDs, bundled libraries and server-only mods), channels with their
     required flag (NeoForge), and protocols.
   - **Commands** (`commands=true`): `/hub`, `/server <id>` and `/global <message>`
     (operators only), handled at the network layer with no plugin. `/hub` and
     `/server` transfer 1.20.5+ players via a cookie and reconnect older ones;
     `/global` broadcasts across every vecta server. Needs Minecraft 1.12+.
   - **Guard** (`guard=true`): the jar takes the public port and only forwards
     connections carrying the gateway's signed PROXY header
     ([docs/guard-protocol.md](docs/guard-protocol.md)). Players who try the
     backend address directly are told to use the gateway.

2. **The sidecar agent** next to any server:

   ```
   vecta agent -config agent.json
   ```

   - **Config:** see [examples/agent.json](examples/agent.json).
   - **Heartbeat:** it re-registers every `interval`. If it stops, the server
     disappears after `registrationTTL`, or immediately on a clean shutdown.
   - **Mods:** with `modsDir` it reads mod IDs from `fabric.mod.json`,
     `quilt.mod.json`, `mods.toml`, `neoforge.mods.toml` and `mcmod.info` in each
     jar.
   - **Required mods:** list mods players must have in `requiredClientMods`.

3. **The API directly:**

   ```
   curl -X PUT https://mc-api.example.com/api/v1/servers/createpack \
     -H "Authorization: Bearer $TOKEN" \
     -d '{"name":"Create Pack","address":"10.0.0.31:25565","loader":"fabric",
          "minProtocol":767,"maxProtocol":767,"requiredClientMods":["create"]}'
   curl -X DELETE https://mc-api.example.com/api/v1/servers/createpack -H "Authorization: Bearer $TOKEN"
   curl https://mc-api.example.com/api/v1/servers
   ```

The full contract is in [docs/owner-api.md](docs/owner-api.md).

**IDs and addresses:**
- Server IDs are `[a-z0-9-]`, up to 32 characters. `lobby`, `api`, `www` and
  `play` are reserved.
- An ID belongs to the first owner who registers it until that registration
  expires.
- Addresses must pass the gateway's address policy.

**Protocol numbers:** 5 = 1.7.10, 47 = 1.8.x, 340 = 1.12.2, 754 = 1.16.5,
763 = 1.20.1, 765 = 1.20.4, 766 = 1.20.5/6, 767 = 1.21/1.21.1, 769 = 1.21.4,
771 = 1.21.6, 774 = 1.21.11, 775 = 26.1.x, 776 = 26.2.

## Operations and security

The gateway is the only public entry point:
- **Harden it** with `rateLimit`, `maxLobbySessions`, `allowedNetworks`,
  `metricsToken` and PROXY protocol (see the configuration reference).
- **Limits need real IPs.** Per-IP limits and the personalized MOTD only make
  sense with real client IPs, so turn on `acceptProxyProtocol` with PROXY
  protocol on the NPM stream. The gateway warns at startup otherwise.
- **API timeouts.** The HTTP API has read and write timeouts. Registration
  bodies are capped at 1 MiB, ticket requests at 4 KiB.
- **State** is kept in `dataFile`. Server registrations themselves are not
  persisted; agents and plugins re-register within one heartbeat.

**Metrics** (`GET /metrics`, Prometheus text format):
- `vecta_connections_total{intent}`: status, login, transfer
- `vecta_connections_rejected_total{reason}`: rate, concurrency, lobby-full
- `vecta_routes_total{via,server}`: cookie, subdomain, pending, sticky, only-candidate
- `vecta_lobby_outcomes_total{outcome}`: transfer, limbo
- `vecta_backend_dial_failures_total{server}`
- `vecta_transfer_tickets_total{mode}`
- `vecta_lobby_sessions_active`, `vecta_servers_registered`, `vecta_servers_online`

**Fuzzing:** every network parser has a Go fuzz target, and CI runs each briefly
on every push. Covered:
- handshake, login start, frames, buffers
- Forge mod lists and `forgeData`
- PROXY headers
- NeoForge replies
- route cookies

## Limits and notes

- **Switching servers means reconnecting.** Players choose once per connection,
  and `lobby.<domain>` or `/hub` returns to the menu. This is deliberate:
  modded clients cannot switch between different packs in one session. Owner
  plugins make it a transfer on 1.20.5+.
- **Pre-1.20.5 clients cannot be transferred.** They reconnect once after
  choosing or matching. Sticky routing sends them back directly afterwards.
  Remembered routes and sticky servers are keyed by username; the backend still
  authenticates the player.
- **Fabric/Quilt clients don't send a mod list.** Matching uses the channel
  namespaces they announce after the Fabric handshake, which covers mods with
  networking.
- **Supported client range:** 1.7.10 (protocol 5) to 26.2 (776) for routing.
  - The limbo covers 1.7.10–1.20.4. Its packet tables and registry data are
    generated from [minecraft-data](https://github.com/PrismarineJS/minecraft-data)
    by [tools/limbogen](tools/limbogen/gen.py).
  - 1.20.5+ uses the configuration-phase lobby.
  - Pre-1.7 legacy pings are ignored.
- **1.7.10 Forge clients send no `FML` marker,** so they are matched as unknown
  loaders. They still join the limbo and their backend normally.
- **NeoForge 1.20.2–1.20.4 clients:** the limbo sends the NeoForge query before
  its brand, so NeoForge's "not a NeoForge server" check is skipped. Whether
  mods with required channels behave in the limbo without a negotiated channel
  set is untested. Those versions have no transfer. If in doubt, route them by
  subdomain or give them a single matching server.
- **Owners stay trusted within their allowed networks.** The gateway dials the
  addresses they register, as long as the address policy allows them.
- **Backends are reachable directly unless guarded.** Without the guard, a
  backend's own port must be firewalled to the gateway. The guard makes that
  unnecessary: its key comes from the owner token, so rotating the token
  rotates the key. A static server needs `"guard": true` and a `guardSecret` in
  the gateway config.

## Development

```
go test ./...
go build -o bin/vecta ./cmd/vecta
```

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)):
- **Every push:**
  - `gofmt`, `go vet`, `go test -race`
  - 30 s of fuzzing per target (including the guard header)
  - a check that the generated tables are up to date (the limbo tables and the
    jar's `packets.json`)
- **Jar:** the server jar is built (Java 8 bytecode) and its self-test run; the
  jar is uploaded as an artifact.
- **Image:** the multi-arch Docker image (amd64, arm64) is built on every push.
  `v*` tags publish it to `ghcr.io/<owner>/<repo>` as `<version>` and `latest`.
- **Nightly and on demand:** the full e2e run, on a self-hosted runner labeled
  `vecta-e2e` that can reach the Docker host.

Regenerate the limbo tables and the jar's `packets.json` after a minecraft-data
update with `python tools/limbogen/gen.py [cache-dir]`.

The server jar builds with `sh plugins/universal/build.sh` (a JDK 11+ or
Docker). It compiles to Java 8 bytecode and runs the self-test, which checks the
guard test vector shared with `internal/guard`.

### End-to-end tests

One command, run from a machine with Go, Node, ssh access to the Docker host,
and a docker CLI context pointing at it:

```
npm ci --prefix test/e2e
E2E_HOST=root@docker-host GW_HOST=docker-host bash test/e2e/run.sh
```

[test/e2e/run.sh](test/e2e/run.sh) does the following:
1. Runs the unit tests and builds the gateway.
2. Builds the vecta server jar on the Docker host and runs its self-test, then
   deploys the stack (restarting the jar-loaded servers if the jar changed).
3. Waits for all servers, and tunes the Paper servers
   ([tune-paper.sh](test/e2e/tune-paper.sh), idempotent).
4. Runs the bot suite, the in-game command suite, and the real-client scenarios.
5. Writes `test/e2e/results/`: logs and a metrics snapshot.

It exits non-zero on any failure. `SKIP_HMC=1` skips the real clients.

The stack ([test/e2e/compose.yaml](test/e2e/compose.yaml)) runs the gateway
and eight real servers:

| id | software | versions accepted | registered by | notes |
|---|---|---|---|---|
| paper-a | Paper 1.21.11 | 774 | static config | PROXY protocol v2 enabled |
| paper-via | Paper 1.21.4 + ViaVersion/ViaBackwards/ViaRewind | 4–776 (ViaVersion, read at runtime) | server jar, agent mode | |
| paper-legacy | Paper 1.8.8 on Java 8 | 47 | server jar, wrapper mode | guard on: Paper on 25566, only signed gateway connections pass |
| vanilla-old | Vanilla 1.20.1 | 763 | static config | |
| fabric-pack | Fabric 1.21.1: fabric-api, appleskin, lithium | 767 | server jar, agent mode | requires appleskin; mods and channels from the runtime check |
| neo-pack | NeoForge 1.21.1: JEI | 767 | server jar, agent mode (`user_jvm_args.txt`) | payload channels with required flags |
| forge-pack | Forge 1.20.1: Waystones, Balm | 763 | server jar, agent mode | channels read from status ping |
| paper-plugin | Paper 1.21.4 + ViaVersion/ViaBackwards | 4–776 | server jar, agent mode | hidden: subdomain and `/server` target only |

**[clients.cjs](test/e2e/clients.cjs)** drives node-minecraft-protocol bots
(1.8.8–26.1). Each check is verified against the backend server's own log. It
covers:
- the status ping
- server jar registrations: loader, software, mods and protocols detected at
  runtime on Fabric, NeoForge, Forge, Paper+Via and Paper 1.8.8
- the guard: direct status pings and logins get the join hint, while players
  through the gateway reach the guarded server (L1)
- dialog → transfer with cookie → spawn
- direct single-candidate routing
- Fabric mod matching and lobby auto-match
- limbo menu → `/join` → reconnect → spawn on 1.8.8, 1.12.2, 1.16.5, 1.19.4,
  1.20.1 and 1.20.4
- subdomains, an unsupported client version, and the `lobby.` host
- forged cookies
- a server stop (the jar unregisters on shutdown) and restart, and a static
  server going offline
- PROXY protocol client IPs
- **jar commands through the full flow** (a player joins a hidden server, runs the
  command, and lands on the target):
  - `/server` → cookie → spawn
  - `/server` for a 1.20.1 player → reconnect → spawn
  - `/hub` → lobby → spawn

[commands.cjs](test/e2e/commands.cjs) adds the network-layer command tests: a
1.21.4 `/server` (store cookie + transfer), a 1.8.8 `/hub` (reconnect kick), and
`/global` from an operator reaching another player, plus a non-operator refusal.

**Real game clients** run headless with
[HeadlessMC](https://github.com/headlesshq/headlessmc) in
[test/e2e/hmc](test/e2e/hmc).
`bash test/e2e/hmc/scenarios.sh [parallelism] [name-regex]`, run on the Docker
host, launches them into the `vecta-test_default` network. It checks that:
- real vanilla 1.8.9, 1.12.2, 1.20.1 and 1.20.4 clients reach the limbo menu;
- direct joins by single match and by subdomain work;
- real modded clients join their pack through the gateway, confirmed in the
  backend server's log:
  - NeoForge 1.21.1 + JEI: lobby → transfer
  - Fabric 1.21.1 + appleskin: Fabric handshake → transfer
  - Forge 1.20.1 + Waystones: mod query → match → reconnect

`hmc-run.sh <version> <user> <host> <port> [timeout]` launches one client.
`<version>` is `1.20.1`, `fabric:1.21.1`, `forge:1.20.1` or `neoforge:1.21.1`.
Options come from the environment:
- `MODS=slug,…`: download those Modrinth mods for the client's loader and version.
- `ASSETS=real`: use real assets instead of HeadlessMC's dummy ones.
- `XVFB=1`: run under Xvfb + Mesa, for GL-heavy mods.

**Known limitation:** the 1.16.5 client stalls during startup under HeadlessMC,
even under Xvfb, and never connects. Its scenario is disabled; the 1.16.5 limbo
is covered by the bot suite.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE). The generated
protocol tables are derived from [minecraft-data](https://github.com/PrismarineJS/minecraft-data)
(MIT); the NOTICE file carries the attribution.

Vecta is not affiliated with Mojang or Microsoft.
