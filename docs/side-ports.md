# Side ports

Some mods and plugins open a second port next to Minecraft:

| Software | Port | Protocol |
|---|---|---|
| Simple Voice Chat | 24454 | UDP |
| Plasmo Voice | game port | UDP |
| Geyser (Bedrock players) | 19132 | UDP |
| BlueMap / Dynmap / squaremap | 8100 / 8123 / 8080 | TCP (HTTP) |
| NuVotifier | 8192 | TCP |

The gateway only routes the Minecraft connection, so these ports are not
reachable through it. **Side ports** fix that:

1. The server declares its extra ports when it registers.
2. The gateway assigns each one a public port from a pool, and forwards raw TCP
   or UDP from it to the backend. It never looks inside the traffic, so any
   protocol works.
3. The owner's side (the server jar or the agent) runs a **hook** with the
   assigned address whenever it changes. The hook points the mod at it, for
   example Simple Voice Chat's `voice_host`.

```
player ──UDP──► publicHost:24500 ──► gateway ──► backend 10.0.0.5:24454
                (assigned port)                  (declared port)
```

## Gateway

```json
"sidePorts": {
  "range": "24500-24599",
  "publicHost": "voice.example.com"
}
```

| Key | Default | Meaning |
|---|---|---|
| `range` | empty (disabled) | Public port pool, `low-high`. Registrations with side ports still succeed when disabled; each side port reports an `error`. |
| `listen` | all interfaces | Host to bind the public ports on. |
| `publicHost` | `domain` | Host players use to reach side ports. |
| `udpIdleTimeout` | `2m` | A UDP flow without traffic for this long is closed. |
| `releaseGrace` | `2m` | A released port stays reserved this long, so a quick restart gets it back. |
| `maxUdpFlows` | 4096 | UDP flows across all ports. |
| `maxFlowsPerPort` | 512 | UDP flows or TCP connections per public port. |

**Publishing the range:**
- Nginx Proxy Manager streams can't forward a port range. Publish the range
  (TCP and UDP) directly on the gateway host, as
  [deploy/compose.yaml](../deploy/compose.yaml) does.
- If `domain` resolves to the NPM host, point `publicHost` at a name that
  resolves to the gateway host itself.

**How assignment works:**
- Assignments live in memory. After a gateway restart, each server asks for
  its previous port again (`preferredPort`) and gets it back if it's still
  free.
- A port is released when:
  - the server stops declaring it,
  - the server unregisters,
  - or the registration expires.

**Forwarding:**
- **UDP:** every client address gets its own upstream socket. The backend
  therefore sees one stable source per player, which Simple Voice Chat needs.
- **Address policy:** backends are dialed at the registered address's host
  and the declared port, under the owner's address policy.

**Metrics:**
- `vecta_sideport_assigned`
- `vecta_sideport_flows`
- `vecta_sideport_connections_total{protocol}`
- `vecta_sideport_bytes_total{protocol,direction}`
- `vecta_sideport_dropped_total{reason}`

## Server jar

In `vecta.properties`:

```properties
sidePorts=voice:udp:24454,map:tcp:8100
sidePortHook=./hooks/simple-voice-chat.sh
```

- **`sidePorts`:** a comma-separated list of `name:protocol:port`.
  - `name` is `[a-z0-9-]`, unique per server.
  - `protocol` is `tcp` or `udp`.
  - `port` is the port on this server.
- **`sidePortHook`:** a command split on whitespace and run without a shell.
  - A relative path is resolved against the server directory.
  - The command runs in the server directory.

When side ports are declared, the jar registers once **before the server
starts**. The hook can then edit mod configs before the mods read them, so a
fresh server needs no restart. The server shows as offline until it answers
the gateway's health check.

The jar keeps what it applied in `.vecta-sideports.json` in the server
directory. The hook therefore only runs when something changes, and that file
also supplies the preferred port.

## Agent

In `agent.json`:

```json
{
  "sidePortHook": ["/srv/hooks/simple-voice-chat.sh"],
  "stateFile": "/srv/vecta-sideports.json",
  "server": {
    "sidePorts": [{"name": "voice", "protocol": "udp", "port": 24454}]
  }
}
```

- **`sidePortHook`:** the argument list, run in the agent's working directory.
- **`stateFile`:** defaults to `vecta-sideports.json` next to the config.
- **Timing:** the agent may run while the server is down, so it can't tell
  whether a mod already read its config. The hook decides whether a restart
  is needed.

## The hook contract

The hook runs once per side port whenever the state changes:
- the port was assigned,
- the public address changed,
- the gateway reported an error,
- or the side port was removed from the config.

**Environment:**

| Variable | Value |
|---|---|
| `VECTA_SERVER_ID` | The server ID. |
| `VECTA_SIDEPORT_NAME` | The side port's name. |
| `VECTA_SIDEPORT_PROTOCOL` | `tcp` or `udp`. |
| `VECTA_SIDEPORT_BACKEND_PORT` | The declared port on the server. |
| `VECTA_SIDEPORT_STATE` | `assigned`, `error` or `released`. |
| `VECTA_SIDEPORT_HOST` / `VECTA_SIDEPORT_PORT` | The public address (`assigned` only). |
| `VECTA_SIDEPORT_ERROR` | Why no port was assigned (`error` only), e.g. `no free public port`. |
| `VECTA_SERVER_STARTED` | Server jar only: `0` before the Minecraft server started, `1` after. |

**Rules:**
- **Timeout and output:** a hook has 30 seconds. Its output goes to the log.
- **Failures:** a non-zero exit (or a timeout) leaves the change pending, and
  it is retried on the next heartbeat.
- **Untrusted values:** gateway values are only passed as environment
  variables, never on a command line. Still, validate them before writing
  them into files.

**Example hooks** in [examples/sideport-hooks](../examples/sideport-hooks):
- `simple-voice-chat.sh`: sets `voice_host` in `voicechat-server.properties`.
  It looks in `config/voicechat/` for mod loaders and `plugins/voicechat/` for
  Paper; override with `VOICECHAT_CONFIG`.
- `print.sh`: logs the assignment and keeps `sideports.txt` up to date.

## Recipes

| Software | Declare | Hook |
|---|---|---|
| **Simple Voice Chat** | `voice:udp:<port>` (the `port` in its config; for `port=-1` use the game port) | `simple-voice-chat.sh` |
| **Plasmo Voice** | `voice:udp:<port>` (its `[host] port`; `0` means the game port) | Set `[host.public]` to the assigned host and port (not verified against every Plasmo Voice version). |
| **Geyser** | `bedrock:udp:19132` | `print.sh`: Bedrock players connect to the assigned `host:port`. |
| **BlueMap / Dynmap** | `map:tcp:8100` | `print.sh`: the map is at `http://host:port/`. |
| **NuVotifier** | `votes:tcp:8192` | `print.sh`: give vote sites the assigned `host:port`. |

- **Plasmo Voice with the game port:** the game port can be declared as a UDP
  side port, since UDP and TCP are separate.
- **Never expose RCON** (or anything else that trusts its network) as a side
  port.

## Security notes

- **Side ports bypass the guard.** Anyone can reach the assigned public port,
  and the gateway forwards to the backend port without the signed header.
- **Hiding backends:** to keep backend ports hidden, firewall them to the
  gateway host.
- **Source address:** the backend sees the gateway as the source address.
- **Which targets can be dialed:** only a registered server's own host can be
  a target, under the owner's address policy. An owner can't use side ports to
  reach other hosts.
- **Caps:** UDP flows and TCP connections are capped per port and globally.
  Drops show in `vecta_sideport_dropped_total`.
