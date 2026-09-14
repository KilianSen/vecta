#!/bin/sh
# Launch one real, headless Minecraft client that joins host:port.
#   [MODS=slug,slug] [ASSETS=real] hmc-run.sh <version> <username> <host> <port> [timeout-seconds]
# <version> is a vanilla version ("1.21.1") or loader-qualified
# ("fabric:1.21.1", "forge:1.20.1", "neoforge:1.21.1"). MODS are Modrinth
# project slugs downloaded for that loader and version into the client's mods.
# Versions and assets are cached in /cache (mount a volume); each client gets
# its own game dir under /work/<username>.
set -eu
VER=$1
NAME=$2
HOST=$3
PORT=$4
TIMEOUT=${5:-300}

GAMEDIR=/work/$NAME
mkdir -p "$GAMEDIR/mods" /cache/.minecraft
cat > "$GAMEDIR/options.txt" <<'EOF'
onboardAccessibility:false
pauseOnLostFocus:false
skipMultiplayerWarning:true
joinedFirstServer:true
narrator:0
EOF

MC=${VER#*:}
MINOR=$(echo "$MC" | sed -E 's/^1\.([0-9]+).*/\1/')
if [ "$MINOR" -ge 20 ]; then
  JOIN="--quickPlayMultiplayer $HOST:$PORT"
else
  JOIN="--server $HOST --port $PORT"
fi

UUID=$(cat /proc/sys/kernel/random/uuid)
# Dummy assets skip the ~500 MB download but can stall some versions while
# building texture atlases (seen on 1.16.5); ASSETS=real downloads the real ones.
DUMMY_ASSETS=true
[ "${ASSETS:-}" = "real" ] && DUMMY_ASSETS=false
JAVAS="/opt/java/java8/bin/java;/opt/java/java17/bin/java;/opt/java/openjdk/bin/java"
cd /headlessmc
hmc() {
  java -Dhmc.mcdir=/cache/.minecraft -Dhmc.offline=true -Dhmc.jline.enabled=false \
    "-Dhmc.java.versions=$JAVAS" \
    -jar headlessmc-launcher-wrapper.jar --command "$@"
}

case "$VER" in
  *:*)
    LOADER=${VER%%:*}
    case "$LOADER" in
      # NeoForge version dirs are named neoforge-<mc minor>.<patch>.<build>.
      neoforge) PATTERN="neoforge-$(echo "$MC" | cut -d. -f2-3)" ;;
      *)        PATTERN="$MC" ;;
    esac
    find_version() { ls /cache/.minecraft/versions 2>/dev/null | grep -i "$LOADER" | grep -F "$PATTERN" | head -1; }
    if [ -z "$(find_version)" ]; then
      echo "[hmc-run] installing $LOADER $MC"
      hmc "$LOADER" "$MC"
    fi
    LAUNCH=$(find_version)
    [ -n "$LAUNCH" ] || { echo "[hmc-run] $LOADER $MC not installed"; exit 2; } ;;
  *)
    LOADER=""
    if [ ! -d "/cache/.minecraft/versions/$MC" ]; then
      echo "[hmc-run] downloading $MC"
      hmc download "$MC"
    fi
    LAUNCH=$MC ;;
esac

if [ -n "${MODS:-}" ]; then
  [ -n "$LOADER" ] || { echo "[hmc-run] MODS need a loader-qualified version"; exit 2; }
  for slug in $(echo "$MODS" | tr ',' ' '); do
    url=$(curl -fsSL -A "vecta-e2e" -G "https://api.modrinth.com/v2/project/$slug/version" \
        --data-urlencode "loaders=[\"$LOADER\"]" --data-urlencode "game_versions=[\"$MC\"]" |
      jq -r '([.[] | select(.version_type == "release")] + .)[0].files | ((map(select(.primary)) + .)[0].url) // empty')
    [ -n "$url" ] || { echo "[hmc-run] no $LOADER $MC build of $slug"; exit 2; }
    file="$GAMEDIR/mods/$(basename "$url" | sed 's/%2B/+/g')"
    curl -fsSL -A "vecta-e2e" -o "$file" "$url"
    echo "[hmc-run] mod $slug -> $(basename "$file")"
  done
fi

# XVFB=1 renders for real (Mesa llvmpipe under Xvfb) instead of stubbing
# LWJGL: needed where the stub stalls (1.16.5) or mods issue real GL calls.
LWJGL=-lwjgl
if [ "${XVFB:-}" = "1" ]; then
  LWJGL=""
  DUMMY_ASSETS=false
fi
cat > "$GAMEDIR/launch.sh" <<EOF
exec java \\
  -Dhmc.gamedir="$GAMEDIR" \\
  -Dhmc.mcdir=/cache/.minecraft \\
  -Dhmc.offline=true \\
  -Dhmc.offline.username="$NAME" \\
  -Dhmc.offline.uuid="$UUID" \\
  -Dhmc.assets.dummy="$DUMMY_ASSETS" \\
  -Dhmc.check.xvfb=true \\
  -Dhmc.rethrow.launch.exceptions=true \\
  -Dhmc.jline.enabled=false \\
  -Dhmc.crash.report.watcher=true \\
  "-Dhmc.java.versions=$JAVAS" \\
  "-Dhmc.gameargs=$JOIN" \\
  "-Dhmc.jvmargs=-Xmx2500M" \\
  -jar headlessmc-launcher-wrapper.jar --command launch "$LAUNCH" $LWJGL -offline
EOF

echo "[hmc-run] launching $LAUNCH as $NAME -> $HOST:$PORT ($JOIN)${XVFB:+ under Xvfb}"
if [ "${XVFB:-}" = "1" ]; then
  export LIBGL_ALWAYS_SOFTWARE=1
  exec timeout "$TIMEOUT" xvfb-run -a -s "-screen 0 1280x720x24" sh "$GAMEDIR/launch.sh"
fi
exec timeout "$TIMEOUT" sh "$GAMEDIR/launch.sh"
