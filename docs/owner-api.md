# vecta owner API (v1)

The vecta server jar ([plugins/universal](../plugins/universal)) and the
sidecar agent (`vecta agent`) use this API. Requests go to the gateway's HTTP API,
behind the NPM proxy host (e.g. `https://mc-api.example.com`). Every request
except the public list carries `Authorization: Bearer <owner token>`.

JSON field names are camelCase. The gateway ignores unknown fields. Errors are
`{"error": "..."}`.

## Register / heartbeat a server

`PUT /api/v1/servers/{id}`

Send every 10–15 s. A server disappears when its registration expires (gateway
`registrationTTL`, default 45 s) or on `DELETE`. Registrations are kept in
memory only; after a gateway restart the next heartbeat restores them.

```json
{
  "name": "Create Pack",
  "description": "optional",
  "address": "10.0.0.31:25565",
  "loader": "neoforge",
  "minProtocol": 767,
  "maxProtocol": 767,

  "reporter": "jar",
  "software": "NeoForge 21.1.250 (Minecraft 1.21.1)",
  "protocols": [767],
  "mods": ["create", "jei"],
  "modVersions": {"create": "6.0.4", "jei": "19.51.0.418"},
  "requiredClientMods": ["create"],
  "channels": [
    {"name": "create:main", "version": "6", "required": true},
    {"name": "jei:recipes", "version": "1", "required": false}
  ],
  "players": 3,
  "maxPlayers": 20,
  "hidden": false,
  "proxyProtocol": false
}
```

| Field | Required | Meaning |
|---|---|---|
| `{id}` (path) | yes | `[a-z0-9][a-z0-9-]{0,31}`, case-insensitive. `lobby`, `api`, `www` and `play` are reserved. |
| `address` | yes | `host:port` the gateway dials. Must pass the [address policy](#address-policy). |
| `name` | no | Display name. Defaults to the ID. |
| `description` | no | Shown in menus and on the status page. |
| `loader` | no (default `vanilla`) | `vanilla`, `paper`, `spigot`, `purpur`, `folia`, `fabric`, `quilt`, `forge`, `neoforge`. |
| `minProtocol` / `maxProtocol` | recommended | Accepted client protocol range. |
| `protocols` | no | Exact list of accepted protocol numbers (e.g. from ViaVersion's API). When present it takes precedence over the range. At most 1000 entries. |
| `reporter` | no | `jar`, `agent` or `plugin`. |
| `software` | no | Free text shown in the public list. |
| `mods` | no | Mod IDs, stored lowercase. At most 2000. |
| `modVersions` | no | Mod ID → version. |
| `requiredClientMods` | no | Mods a client must have to be matched to this server. |
| `channels` | no | Network channels/payloads the server registers. `version` is the exact protocol version string. `required` means clients without the channel cannot join. The gateway counts channel namespaces as server mods, and treats the namespace of a required channel as a required client mod. Loader namespaces (`minecraft`, `neoforge`, `fabric`, `c`, …) are ignored. At most 5000. |
| `players` / `maxPlayers` | no | Informational. The gateway's status ping overwrites them every `healthInterval`. |
| `hidden` | no | Not matched, not listed, not in menus. Still reachable by `<id>.<domain>`, sticky routing and transfer tickets. |
| `proxyProtocol` | no | The gateway sends a PROXY v2 header (the real client address) on player connections and health pings. |
| `sidePorts` | no | Extra ports to expose, e.g. voice chat: `[{"name":"voice","protocol":"udp","port":24454,"preferredPort":24500}]`. `name` is `[a-z0-9-]`, unique; `protocol` is `tcp` or `udp`; `port` is the backend port on the `address` host; `preferredPort` asks for a public port (usually the last one assigned). At most 16. The gateway ignores `public` and `error` in requests. See [side-ports.md](side-ports.md). |
| `guard` | no | The backend only accepts connections signed by the gateway. The gateway sends a signed PROXY v2 header, keyed by the owner token, on player connections and health pings (instead of the plain one). See [guard-protocol.md](guard-protocol.md). |

The request body is capped at 1 MiB.

**Responses:**

| Code | Meaning |
|---|---|
| `200` | The stored server, including live state such as `online` and `versionName`. Each side port carries either `public` (`{"host","port"}`, where players reach it) or `error` (e.g. side ports are disabled, or no port is free). |
| `400` | Invalid JSON, ID or limits. |
| `401` | Bad token. |
| `403` | Address not allowed for this owner. |
| `409` | The ID is registered by another owner. |

`DELETE /api/v1/servers/{id}` releases the server's side ports and returns `204`, `404` (not registered) or `403` (another owner's server).

## Transfer a player (`/hub`, `/server <id>`)

`POST /api/v1/servers/{id}/transfer-ticket`

`{id}` is the server the player is on now. The token must own it.

```json
{"player": "Steve", "uuid": "8667ba71-b85a-4004-af54-457a9734eed7", "protocol": 767, "target": "survival"}
```

| Field | Meaning |
|---|---|
| `player` | Player name, `[A-Za-z0-9_]{1,16}`. |
| `uuid` | Optional. Currently unused. |
| `protocol` | The player's client protocol version. The server jar sends the player's own protocol, read from their handshake. |
| `target` | A registered server ID (hidden servers included) or `"lobby"`, which opens the gateway's menu. |

The request body is capped at 4 KiB.

**Response for 1.20.5+ clients (`protocol >= 766`):**

```json
{
  "mode": "transfer",
  "host": "play.example.com",
  "port": 25565,
  "cookieKey": "vecta:route",
  "cookie": "<base64 bytes>"
}
```

`host` is the gateway's `transferHost`, or its `domain`. `port` is
`transferPort`, or 25565. The plugin/mod then:

1. Stores the cookie on the client: decode the base64 and send Store Cookie
   with key `cookieKey`.
   - Paper: `player.storeCookie(NamespacedKey.fromString(cookieKey), bytes)`
   - Vanilla packet: `ClientboundStoreCookiePacket`
2. Transfers the client:
   - Paper: `player.transfer(host, port)`
   - Vanilla packet: `ClientboundTransferPacket(host, port)`

The cookie is HMAC-signed by the gateway, bound to the player name, and valid
for 2 minutes. When the client reconnects, the gateway routes it to `target`,
or opens the lobby for `"lobby"`.

**Response for older clients (`protocol < 766`):**

```json
{"mode": "reconnect", "address": "play.example.com", "message": "Reconnect to play.example.com to join Survival."}
```

- **Target is a server:** the gateway remembers the choice for this player
  (`pendingTTL`), and their next plain join goes there.
- **Target is `"lobby"`:** `address` is `lobby.<domain>`, and any remembered
  choice is cleared.
- **Port:** `address` includes `:port` when the gateway's `transferPort` isn't
  25565.
- **Client action:** the plugin/mod disconnects the player with `message`.

**Errors:**

| Code | Meaning |
|---|---|
| `400` | Bad body, player name or protocol, or missing target. |
| `401` | Bad token. |
| `403` | The token does not own `{id}`. |
| `404` | `{id}` not registered, or the target is unknown or offline. |
| `409` | The target does not accept `protocol`. |
| `501` | Transfers not enabled on this gateway. |
| `503` | The gateway has no `domain` or `transferHost` configured. |

## Global chat (`/global`)

Cross-server broadcast. The server jar implements `/global <message>` for
operators; these two endpoints carry it between the jar and the gateway. Both
take any valid owner token.

**Submit a message**

`POST /api/v1/servers/{id}/global` — the token must own `{id}` (the server the
sender is on).

```json
{"player": "Steve", "message": "server restarting in 5m"}
```

| Field | Meaning |
|---|---|
| `player` | Sender name, `[A-Za-z0-9_]{1,16}`. |
| `message` | The text. Control characters and `§` are stripped; trimmed to 256 chars. |

The gateway assigns a sequence number and returns `{"seq": <n>}`. Operator
status is checked by the jar (from `ops.json`) before it calls this; the gateway
trusts the owner token. Body capped at 4 KiB.

**Long-poll for messages**

`GET /api/v1/global?after=<seq>` blocks up to ~25 s and returns messages newer
than `after`:

```json
{"messages": [{"seq": 42, "server": "survival", "player": "Steve", "message": "hi", "time": 1712345678}], "seq": 42}
```

With no `after` parameter it returns the current `seq` immediately and no
messages, so a jar starting up learns its position without replaying history.
Each jar then polls with the last `seq` it saw and shows new messages to its
players. Errors: `401` (bad token).

## Public list

`GET /api/v1/servers` returns all non-hidden servers, without address or
owner. Fields:

`id`, `name`, `description`, `join` (`<id>.<domain>`), `loader`, `software`,
`versionName`, `minProtocol`, `maxProtocol`, `mods`, `online`, `players`,
`maxPlayers`.

## Address policy

- **Always refused:** addresses that resolve to loopback (unless the gateway sets
  `allowLoopbackBackends`), link-local (including cloud metadata
  `169.254.169.254`), unspecified or multicast IPs.
- **Allowed networks:** if the gateway has `allowedNetworks`, globally or for
  this owner, the address must resolve into them. An owner-level list replaces
  the global one.
- **Hostnames:** every resolved IP must pass.
- **When:** the check runs at registration (`403`) and again on every dial,
  including health pings, so a DNS change cannot bypass it.
- **Static servers** from the gateway's own config are trusted and not checked.
