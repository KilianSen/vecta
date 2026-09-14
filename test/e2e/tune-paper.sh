#!/bin/sh
# Run on the Docker host once the stack is up. Idempotent: servers are only
# restarted when a setting actually changed. Waits for each server to have
# written its config files (fresh servers need a minute), and skips a server
# with a warning instead of aborting if they never appear.
# - Every player reaches backends from the gateway's IP, so Paper's per-IP login
#   throttle must be off (or PROXY protocol on) or rapid joins get rejected.
# - paper-a receives PROXY protocol v2 from the gateway (proxyProtocol: true).
set -u
P=${PROJECT:-anymcp-test}
WAIT=${TUNE_WAIT_SECONDS:-180}
restart=""

wait_for_file() { # container path
  i=0
  while [ "$i" -lt "$WAIT" ]; do
    docker exec "$1" test -f "$2" 2>/dev/null && return 0
    sleep 3
    i=$((i + 3))
  done
  echo "warning: $1 has no $2 after ${WAIT}s; skipped" >&2
  return 1
}

add_restart() {
  case " $restart " in *" $1 "*) ;; *) restart="$restart $1" ;; esac
}

for svc in mc-paper-a mc-paper-via mc-paper-legacy mc-paper-plugin; do
  c="$P-$svc-1"
  docker inspect "$c" >/dev/null 2>&1 || { echo "warning: $c not found; skipped" >&2; continue; }
  wait_for_file "$c" /data/bukkit.yml || continue
  if ! docker exec "$c" grep -q "connection-throttle: -1" /data/bukkit.yml; then
    docker exec "$c" sed -i 's/connection-throttle: .*/connection-throttle: -1/' /data/bukkit.yml && add_restart "$c"
  fi
done

c="$P-mc-paper-a-1"
if wait_for_file "$c" /data/config/paper-global.yml; then
  if ! docker exec "$c" grep -q "proxy-protocol: true" /data/config/paper-global.yml; then
    docker exec "$c" sed -i 's/proxy-protocol: false/proxy-protocol: true/' /data/config/paper-global.yml && add_restart "$c"
  fi
fi

if [ -n "$restart" ]; then
  docker restart $restart >/dev/null
  echo "restarted:$restart"
else
  echo "paper settings already applied"
fi
