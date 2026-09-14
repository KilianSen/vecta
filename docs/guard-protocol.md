# Guard protocol (v1)

A guarded backend accepts only connections that come from the gateway. The
anymcp server jar implements the backend side: it listens on the public port
and forwards verified connections to the real server on localhost. Players who
try the backend address directly are refused.

Enable it with `"guard": true` in the registration (the jar sets this when its
guard is on), or on a static server with `"guard": true, "guardSecret": "..."`.

## Key

```
key = SHA-256("anymcp-guard-key-v1:" + secret)
```

`secret` is the owner token for registered servers, or `guardSecret` for
static servers. Anyone holding the owner token can already register and
remove the server, so the token doesn't grant anything new here.

The jar derives its key from its `token`, so for a **static** server set the
jar's `token` equal to the gateway's `guardSecret`. For a **registered** server
the jar's `token` is the owner token and the gateway already knows it, so no
`guardSecret` is needed.

## Header

The gateway starts every connection to a guarded backend with a PROXY
protocol v2 header:

- **Player connections:** `PROXY` command (`0x21`) with the TCP4 (`0x11`) or
  TCP6 (`0x21`) address block (source = the player's real address).
- **Health pings:** `LOCAL` command (`0x20`), family `0x00`, no address block.

After the address block comes one TLV of type `0xE0` with a 53-byte value:

| Offset | Size | Field |
|---|---|---|
| 0 | 1 | version, `0x01` |
| 1 | 8 | Unix time in seconds, big endian |
| 9 | 12 | random nonce |
| 21 | 32 | HMAC-SHA256 |

```
mac = HMAC-SHA256(key, "anymcp-guard-v1" || verCmd || family || addressBlock
                       || version || timestamp || nonce)
```

`verCmd` and `family` are header bytes 12 and 13. `addressBlock` is empty for
`LOCAL`. Other TLVs may appear and are not signed.

## Verification

The backend reads at most 16 + 1024 bytes of header, then refuses the
connection unless all of these hold:

1. The header is PROXY v2 with `LOCAL`, or `PROXY` over TCP4/TCP6.
2. Exactly one `0xE0` TLV, version 1, and the MAC matches (constant-time compare).
3. The timestamp is within 120 s of the backend's clock.
4. The nonce was not seen within the last 240 s.

The jar then connects to the local server. If the local server expects PROXY
protocol, the jar sends it a plain PROXY v2 header with the verified address.

## Test vector

key = `Key("tok-plugin")`, source `203.0.113.9:51234`, destination
`10.0.0.5:25565`, time `1800000000`, nonce twelve `0x07` bytes. The header
starts with:

```
0d0a0d0a000d0a515549540a 2111 0044 cb007109 0a000005 c822 63dd
e00035 01 000000006b49d200 070707070707070707070707
```

followed by the 32-byte MAC. `internal/guard/guard_test.go` logs the full
vector, and the jar's tests check it.
