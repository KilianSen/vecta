# anymcp server jar

One jar for any Java Minecraft server (vanilla, Paper/Spigot, Fabric/Quilt,
Forge, NeoForge; Java 8 and newer). It registers the server at an anymcp
gateway and can guard it so players can only join through the gateway. No
dependencies and nothing compiled against a platform, so the same jar works
across loaders and versions.

## Install

Put `anymcp.jar` in the server directory and pick one way to start it:

| Mode | Start command | Use when |
|---|---|---|
| Wrapper | `java -jar anymcp.jar [server.jar] [server args]` | The server starts from a single jar (vanilla, Paper, Fabric launcher, Forge ≤1.16, Forge/NeoForge server shims). Without `server.jar`, the jar is detected (or set `serverJar`). |
| Java agent | `java -javaagent:anymcp.jar -jar server.jar` | The server starts from a script, e.g. Forge/NeoForge `run.sh`: add `-javaagent:anymcp.jar` to `user_jvm_args.txt`. |

On the first start it creates `anymcp.properties`. Fill in `gateway`, `token`,
`serverId` and `address`, then restart. Every key can also come from an
environment variable (`ANYMCP_TOKEN`, `ANYMCP_SERVER_ID`, …), a system
property (`-Danymcp.token=…`) or agent arguments
(`-javaagent:anymcp.jar=serverId=pack;guard=true`, or `=path/to/anymcp.properties`).

With `itzg/minecraft-server`, set `JVM_OPTS=-javaagent:/path/anymcp.jar` and
the `ANYMCP_*` variables.

## What it reports

Once the server answers a local status ping, the jar registers it and sends a
heartbeat every `heartbeatSeconds`. On shutdown it unregisters.

| Field | Source |
|---|---|
| players, version, protocol | Local status ping |
| loader, software | Runtime check, else files (`libraries/`, `.fabric/`, `paper-global.yml`, …) |
| mods | Runtime check (what actually loaded), else the jars in `mods/` |
| channels | Runtime check: NeoForge 1.20.5+ payloads with their required flag, Fabric receivers and payload types |
| protocols | ViaVersion's supported list at runtime (Paper, ViaFabric, …), else the server's own |

**File scan:** reads `fabric.mod.json`, `quilt.mod.json`, `META-INF/mods.toml`,
`META-INF/neoforge.mods.toml`, `mcmod.info` and `plugin.yml`, including mods
bundled inside other mods (`META-INF/jars`, `META-INF/jarjar`).

**Runtime checks** use reflection on stable entry points: `FabricLoader`,
NeoForge/Forge `ModList`, legacy Forge `Loader` (1.7.10–1.12.2), `Bukkit`,
NeoForge's `NetworkRegistry` and ViaVersion's API. Each step may fail
independently; the report then uses the file scan. Set `runtimeChecks=false` to
skip them.

**Mods sent to the gateway** are the ones clients may care about. Left out:
- the platform itself (`minecraft`, `java`, the loader, Fabric API modules),
- libraries bundled inside other mods,
- server-only mods: Fabric `"environment": "server"`, Quilt `"dedicated_server"`,
- client-only mods (`"environment": "client"`, `clientSideOnly`),
- Bukkit plugins.

Forge/NeoForge mods whose `displayTest` isn't `MATCH_VERSION` (e.g. JEI's
`IGNORE_SERVER_VERSION`) are optional for clients. They stay in the list,
because they still say something about the pack.

`requiredClientMods` still has to be set by hand: metadata can't tell whether a
mod really needs the client. On NeoForge 1.20.5+ the channels already carry
that: a required channel makes its namespace a required client mod.

## Commands

With `commands=true` (the default) the jar adds three commands at the network
layer — no plugin, and the same code on every loader from 1.7.10 up:

| Command | Who | Effect |
|---|---|---|
| `/hub`, `/lobby` | anyone | Moves the player to the gateway's lobby. |
| `/server <id>` | anyone | Moves the player to another registered server. |
| `/global <message>` | operators (`ops.json`) | Broadcast to every player on every anymcp server. |

`/hub` and `/server` use a transfer ticket: 1.20.5+ clients move without
reconnecting (Store Cookie + Transfer), older clients get a "reconnect to …"
message. `/global` posts to the gateway, which relays it to every server's jar.

How it works: the jar splices a handler into each connection's Netty pipeline
(after decryption/decompression) using only Netty's stable, string-named
handlers, reads the protocol and player from the handshake, and catches the
chat/command packet. Packet IDs per version come from a generated table
(`packets.json`). This needs Netty 4.1, i.e. **Minecraft 1.12+** for the
epoll/`ids` map layout — 1.8–1.11 also work. If the jar can't attach (an
unusual server), it logs a warning and everything else still works; players use
`lobby.<domain>` and `<id>.<domain>` instead.

## Guard

With `guard=true` the jar listens on the public port and forwards only
connections that carry the gateway's signed PROXY header
([docs/guard-protocol.md](../../docs/guard-protocol.md)). Direct joins get a
disconnect message, and direct status pings show `guardJoinHint`.

1. In `server.properties`, move the server: `server-port=25566`,
   `server-ip=127.0.0.1`.
2. In `anymcp.properties`: `guard=true`, `guardListen=0.0.0.0:25565`,
   `guardJoinHint=play.example.com`, and `address` = the guard's address as the
   gateway sees it.
3. The registration then carries `"guard": true`, and the gateway signs every
   connection with a key derived from your owner token.

If the server itself expects PROXY protocol (`proxyProtocol=true`), the guard
passes the player's real address on in a plain PROXY v2 header.

## Settings

| Key | Default | Meaning |
|---|---|---|
| `gateway` | | Gateway API base URL. |
| `token` | | Owner token. Also the guard key. |
| `serverId` | | Lowercase ID and subdomain. |
| `name`, `description` | ID, empty | Shown to players. |
| `address` | | `host:port` the gateway connects to. |
| `hidden` | false | Only reachable by subdomain, sticky routing and transfers. |
| `heartbeatSeconds` | 15 | Heartbeat interval (minimum 5). |
| `loader` | detected | Override, e.g. `neoforge`. |
| `protocols` | detected | `763-767` or `763,765`. |
| `requiredClientMods` | | Comma-separated mod IDs clients must have. |
| `proxyProtocol` | false | The server expects a PROXY header. |
| `guard` | false | Enable the guard. |
| `guardListen` | `0.0.0.0:25565` | Public listener of the guard. |
| `guardBackend` | `127.0.0.1:<server-port>` | Where the guard forwards to. |
| `guardJoinHint` | | Address shown to players who connect directly. |
| `commands` | true | In-game `/hub`, `/server` and `/global` (see above). |
| `runtimeChecks` | true | Ask the running server what loaded. |
| `serverJar` | detected | Wrapper mode: the server jar. |
| `debug` | false | Verbose logging. |

## Build

```
sh build.sh
```

Needs a JDK 11+ (compiles to Java 8 bytecode), or Docker. It writes
`build/anymcp.jar` and runs the self-test, which includes the guard test vector
shared with the Go gateway.
