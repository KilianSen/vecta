#!/usr/bin/env python3
"""Generate limbo protocol tables from PrismarineJS minecraft-data (MIT).

Outputs (relative to the repo root):
  internal/limbo/zz_packets_gen.go   packet IDs per protocol version
  internal/limbo/data/*.nbt.gz       registry codecs / dimension elements
                                     (compound payload only: no tag id, no name)
  plugins/universal/src/main/resources/packets.json
                                     packet IDs the server jar's command handler
                                     needs, for every Netty-era release (5..latest)

Usage: python tools/limbogen/gen.py [cache-dir]
"""
import gzip
import json
import os
import re
import shutil
import struct
import subprocess
import sys
import urllib.request

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
CACHE = sys.argv[1] if len(sys.argv) > 1 else os.path.join(REPO, ".cache", "minecraft-data")
BASE = "https://raw.githubusercontent.com/PrismarineJS/minecraft-data/master/data/"

# Release protocols without their own minecraft-data entry borrow a neighbour
# whose packet IDs are identical.
BORROW = {108: "1.9.2", 485: "1.14.1"}

PACKETS = {
    # field name: (state, direction, [candidate minecraft-data packet names])
    "Login":          ("play", "toClient", ["login"]),
    "Position":       ("play", "toClient", ["position"]),
    "SpawnPosition":  ("play", "toClient", ["spawn_position"]),
    "KeepAlive":      ("play", "toClient", ["keep_alive"]),
    "Chat":           ("play", "toClient", ["chat"]),
    "SystemChat":     ("play", "toClient", ["system_chat"]),
    "Abilities":      ("play", "toClient", ["abilities"]),
    "GameEvent":      ("play", "toClient", ["game_state_change"]),
    "CenterChunk":    ("play", "toClient", ["update_view_position"]),
    "ChunkData":      ("play", "toClient", ["map_chunk"]),
    "Commands":       ("play", "toClient", ["declare_commands"]),
    "Disconnect":     ("play", "toClient", ["kick_disconnect"]),
    "PluginMessage":  ("play", "toClient", ["custom_payload"]),
    "InKeepAlive":    ("play", "toServer", ["keep_alive"]),
    "InChat":         ("play", "toServer", ["chat", "chat_message"]),
    "InChatCommand":  ("play", "toServer", ["chat_command"]),
    "InTeleportConfirm": ("play", "toServer", ["teleport_confirm"]),
    "InPluginMessage": ("play", "toServer", ["custom_payload"]),
    "CfgRegistryData": ("configuration", "toClient", ["registry_data"]),
    "CfgFeatureFlags": ("configuration", "toClient", ["feature_flags"]),
    "CfgFinish":       ("configuration", "toClient", ["finish_configuration"]),
    "CfgKeepAlive":    ("configuration", "toClient", ["keep_alive"]),
    "CfgPluginMessage": ("configuration", "toClient", ["custom_payload"]),
    "CfgDisconnect":   ("configuration", "toClient", ["disconnect"]),
    "InCfgFinish":     ("configuration", "toServer", ["finish_configuration"]),
    "InCfgKeepAlive":  ("configuration", "toServer", ["keep_alive"]),
    "InCfgPluginMessage": ("configuration", "toServer", ["custom_payload"]),
}


def fetch(path):
    local = os.path.join(CACHE, path.replace("/", "_"))
    if not os.path.exists(local):
        os.makedirs(CACHE, exist_ok=True)
        with urllib.request.urlopen(BASE + path) as r, open(local, "wb") as f:
            f.write(r.read())
    with open(local, "rb") as f:
        return json.load(f)


# --- NBT (prismarine-nbt JSON -> binary) -----------------------------------

TAG = {"end": 0, "byte": 1, "short": 2, "int": 3, "long": 4, "float": 5, "double": 6,
       "byteArray": 7, "string": 8, "list": 9, "compound": 10, "intArray": 11, "longArray": 12}


def mutf8(s):
    out = bytearray()
    for ch in s:
        c = ord(ch)
        if 0 < c < 0x80:
            out.append(c)
        elif c < 0x800:
            out += bytes([0xC0 | (c >> 6), 0x80 | (c & 0x3F)])
        elif c < 0x10000:
            out += bytes([0xE0 | (c >> 12), 0x80 | ((c >> 6) & 0x3F), 0x80 | (c & 0x3F)])
        else:
            c -= 0x10000
            for sur in (0xD800 + (c >> 10), 0xDC00 + (c & 0x3FF)):
                out += bytes([0xE0 | (sur >> 12), 0x80 | ((sur >> 6) & 0x3F), 0x80 | (sur & 0x3F)])
    return struct.pack(">H", len(out)) + bytes(out)


def long_bytes(v):
    if isinstance(v, list):  # [high, low] int32 pair
        return struct.pack(">II", v[0] & 0xFFFFFFFF, v[1] & 0xFFFFFFFF)
    return struct.pack(">q", int(v))


def payload(t, v):
    if t == "byte":
        return struct.pack(">b", v)
    if t == "short":
        return struct.pack(">h", v)
    if t == "int":
        return struct.pack(">i", v)
    if t == "long":
        return long_bytes(v)
    if t == "float":
        return struct.pack(">f", v)
    if t == "double":
        return struct.pack(">d", v)
    if t == "string":
        return mutf8(v)
    if t == "byteArray":
        return struct.pack(">i", len(v)) + bytes(b & 0xFF for b in v)
    if t == "intArray":
        return struct.pack(">i", len(v)) + b"".join(struct.pack(">i", x) for x in v)
    if t == "longArray":
        return struct.pack(">i", len(v)) + b"".join(long_bytes(x) for x in v)
    if t == "list":
        et, items = v["type"], v["value"]
        return bytes([TAG[et]]) + struct.pack(">i", len(items)) + b"".join(payload(et, x) for x in items)
    if t == "compound":
        out = bytearray()
        for name, child in v.items():
            out.append(TAG[child["type"]])
            out += mutf8(name)
            out += payload(child["type"], child["value"])
        out.append(0)
        return bytes(out)
    raise ValueError("unsupported nbt type " + t)


def skip(t, b, i):
    """Walk an encoded payload to verify it is well-formed; returns end offset."""
    fixed = {"byte": 1, "short": 2, "int": 4, "long": 8, "float": 4, "double": 8}
    if t in fixed:
        return i + fixed[t]
    if t == "string":
        return i + 2 + struct.unpack_from(">H", b, i)[0]
    if t in ("byteArray", "intArray", "longArray"):
        n = struct.unpack_from(">i", b, i)[0]
        return i + 4 + n * {"byteArray": 1, "intArray": 4, "longArray": 8}[t]
    rev = {v: k for k, v in TAG.items()}
    if t == "list":
        et, n = rev[b[i]], struct.unpack_from(">i", b, i + 1)[0]
        i += 5
        for _ in range(n):
            i = skip(et, b, i)
        return i
    if t == "compound":
        while True:
            tag = b[i]
            i += 1
            if tag == 0:
                return i
            i = skip("string", b, i)
            i = skip(rev[tag], b, i)
    raise ValueError(t)


def write_nbt(name, node):
    assert node["type"] == "compound", name
    data = payload("compound", node["value"])
    assert skip("compound", data, 0) == len(data), name
    path = os.path.join(REPO, "internal", "limbo", "data", name + ".nbt.gz")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with gzip.GzipFile(path, "wb", mtime=0) as f:
        f.write(data)
    return len(data)


# --- main --------------------------------------------------------------------

def release_protocols(pv, hi=765):
    rel = {}
    for p in pv:
        mv = p["minecraftVersion"]
        if not re.fullmatch(r"1\.\d+(\.\d+)?", mv):
            continue
        parts = [int(x) for x in mv.split(".")] + [0]
        # Netty-era releases only: pre-1.7 protocol numbers overlap.
        if parts[:3] >= [1, 7, 6] and 5 <= p["version"] <= hi:
            rel.setdefault(p["version"], []).append(mv)
    return {k: sorted(v, key=lambda s: [int(x) for x in s.split(".")]) for k, v in sorted(rel.items())}


# Packets the server jar's command handler reads/writes, as minecraft-data names.
JAR_PACKETS = {
    "inChat":          ("play", "toServer", ["chat_message", "chat"]),
    "inChatCommand":   ("play", "toServer", ["chat_command"]),
    "inChatCommandSigned": ("play", "toServer", ["chat_command_signed"]),
    "inChatSession":   ("play", "toServer", ["chat_session_update"]),  # unused, reserved
    "systemChat":      ("play", "toClient", ["system_chat"]),
    "chat":            ("play", "toClient", ["chat"]),
    "disconnect":      ("play", "toClient", ["kick_disconnect"]),
    "transfer":        ("play", "toClient", ["transfer"]),
    "storeCookie":     ("play", "toClient", ["store_cookie"]),
}


def packet_ids(p, spec):
    """Resolve the packet IDs in spec against one protocol.json, -1 if absent."""
    ids = {}
    for field, (state, direction, pnames) in spec.items():
        ids[field] = -1
        if state not in p or direction not in p[state]:
            continue
        mapping = p[state][direction]["types"]["packet"][1][0]["type"][1]["mappings"]
        inv = {name: int(pid, 16) for pid, name in mapping.items()}
        for n in pnames:
            if n in inv:
                ids[field] = inv[n]
                break
    return ids


def emit_jar_packets(data_paths, all_releases):
    """Write plugins/universal/.../packets.json for every Netty-era release."""
    table = {}
    for proto, names in all_releases.items():
        src = BORROW.get(proto)
        candidates = ([src] if src else []) + names
        entry = next((data_paths[n] for n in candidates if n in data_paths and "protocol" in data_paths[n]), None)
        if proto == 5:
            entry = data_paths["1.7"]
        if entry is None:
            continue
        p = fetch(entry["protocol"] + "/protocol.json")
        ids = packet_ids(p, JAR_PACKETS)
        ids["_versions"] = names
        table[str(proto)] = ids
    path = os.path.join(REPO, "plugins", "universal", "src", "main", "resources", "packets.json")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", newline="\n") as f:
        json.dump(table, f, indent=1, sort_keys=True)
        f.write("\n")
    print(f"jar packets.json: {len(table)} protocols (max {max(int(k) for k in table)})")


def main():
    data_paths = fetch("dataPaths.json")["pc"]
    releases = release_protocols(fetch("pc/common/protocolVersions.json"))
    rows, codecs, dims = [], {}, {}
    written = {}
    for proto, names in releases.items():
        src = BORROW.get(proto)
        candidates = ([src] if src else []) + names
        entry = next((data_paths[n] for n in candidates if n in data_paths and "protocol" in data_paths[n]), None)
        if proto == 5:
            entry = data_paths["1.7"]
        if entry is None:
            raise SystemExit(f"no minecraft-data for protocol {proto} {names}")
        p = fetch(entry["protocol"] + "/protocol.json")
        ids = {}
        for field, (state, direction, pnames) in PACKETS.items():
            ids[field] = -1
            if state not in p:
                continue
            mapping = p[state][direction]["types"]["packet"][1][0]["type"][1]["mappings"]
            inv = {name: int(pid, 16) for pid, name in mapping.items()}
            for n in pnames:
                if n in inv:
                    ids[field] = inv[n]
                    break
        rows.append((proto, names, entry["protocol"], ids))

        lp = entry.get("loginPacket")
        if lp and proto >= 735:
            key = lp.split("/")[1]
            if key not in written:
                login = fetch(lp + "/loginPacket.json")
                written[key] = write_nbt("codec_" + key, login["dimensionCodec"])
                if isinstance(login.get("dimension"), dict) and login["dimension"].get("type") == "compound":
                    written[key + ".dim"] = write_nbt("dim_" + key, login["dimension"])
            codecs[proto] = "codec_" + key
            if key + ".dim" in written:
                dims[proto] = "dim_" + key

    out = ["// Code generated by tools/limbogen/gen.py from PrismarineJS minecraft-data (MIT). DO NOT EDIT.", "",
           "package limbo", "", "// packetIDs holds the packet IDs the limbo uses; -1 means absent in that version.",
           "type packetIDs struct {"]
    for field in PACKETS:
        out.append(f"\t{field} int32")
    out += ["}", "", "// packetTable is keyed by protocol version.", "var packetTable = map[int32]packetIDs{"]
    for proto, names, src, ids in rows:
        vals = ", ".join(f"{k}: {v}" for k, v in ids.items())
        out.append(f"\t{proto}: {{{vals}}}, // {', '.join(names)} ({src})")
    out += ["}", "", "// codecFiles maps protocol versions to embedded registry codec files.", "var codecFiles = map[int32]string{"]
    for proto, name in codecs.items():
        out.append(f'\t{proto}: "data/{name}.nbt.gz",')
    out += ["}", "", "// dimensionFiles maps protocol versions to embedded dimension-type elements (1.16.2-1.18.2 Join Game).",
            "var dimensionFiles = map[int32]string{"]
    for proto, name in dims.items():
        out.append(f'\t{proto}: "data/{name}.nbt.gz",')
    out.append("}")
    gen_path = os.path.join(REPO, "internal", "limbo", "zz_packets_gen.go")
    with open(gen_path, "w", newline="\n") as f:
        f.write("\n".join(out) + "\n")
    gofmt = shutil.which("gofmt")
    if gofmt:
        subprocess.run([gofmt, "-w", gen_path], check=True)
    else:
        print("warning: gofmt not found; run gofmt -w on", gen_path)

    print(f"protocols: {len(rows)}")
    for k, n in written.items():
        print(f"  nbt {k}: {n} bytes raw")

    # The jar handles every version including 1.20.5+ (no upper cap).
    emit_jar_packets(data_paths, release_protocols(fetch("pc/common/protocolVersions.json"), hi=10_000))


if __name__ == "__main__":
    main()
