#!/usr/bin/env bash
# Real-client scenarios (run on the Docker host with the e2e stack up).
# Vanilla clients cannot type commands, so limbo scenarios verify that each
# version reaches the menu; choosing is covered by clients.cjs.
#   bash scenarios.sh [parallelism] [name-filter-regex]
set -u
P=${PROJECT:-anymcp-test}
PAR=${1:-3}
FILTER=${2:-.}
OUT=/tmp/anymcp-hmc
mkdir -p "$OUT"

# name version host expectation(extended regex, no spaces) timeout mods mode env
#   mods: comma-separated Modrinth slugs or "-"
#   mode: once | reconnect (join twice with the same account: pick, then play)
#   expectation: regex on the client log, or server:<service>:<regex> to grep
#   that backend's log instead ({user} is replaced by the player name). Modded
#   joins are checked server-side: client logs don't reliably show them.
#   env:  comma-separated VAR=value for hmc-run.sh (XVFB=1, ASSETS=real) or "-"
# Lines starting with # are skipped. H3: the 1.16.5 client stalls during
# startup under HeadlessMC (its image-decoding stub stays active even under
# Xvfb) and never connects; the 1.16.5 limbo is covered by clients.cjs (L4).
SCENARIOS='
H1 1.8.9            play.test               \[CHAT\].*Choose.a.server 150 -                    once      -
H2 1.12.2           lobby.play.test         \[CHAT\].*Choose.a.server 150 -                    once      -
#H3 1.16.5          lobby.play.test         \[CHAT\].*Choose.a.server 420 -                    once      XVFB=1
H4 1.20.1           play.test               \[CHAT\].*Choose.a.server 180 -                    once      -
H5 1.20.4           lobby.play.test         \[CHAT\].*Choose.a.server 180 -                    once      -
H6 1.12.2           play.test               joined.the.game           150 -                    once      -
H7 1.8.9            paper-legacy.play.test  joined.the.game           150 -                    once      -
M1 neoforge:1.21.1  play.test               server:mc-neoforge:{user}.joined.the.game 300 jei                  once      -
M2 fabric:1.21.1    play.test               server:mc-fabric:{user}.joined.the.game 300 fabric-api,appleskin once      -
M3 forge:1.20.1     play.test               server:mc-forge:{user}.joined.the.game 300 waystones,balm       reconnect -
'

launch() { # name ver host timeout mods user log env
  local mods=$5 envs=()
  [ "$mods" = "-" ] && mods=""
  if [ "$8" != "-" ]; then
    IFS=',' read -ra kv <<< "$8"
    for e in "${kv[@]}"; do envs+=(-e "$e"); done
  fi
  docker run --rm --name "$P-hmc-$1" --label anymcp-test=true --network "${P}_default" \
    -e MODS="$mods" "${envs[@]}" -v anymcp-test-hmc-cache:/cache anymcp-test/hmc:2.10.0 \
    "$2" "$6" "$3" 25565 "$4" >"$7" 2>&1
}

run() {
  local name=$1 ver=$2 host=$3 expect=$4 timeout=$5 mods=$6 mode=$7 env=$8
  local tag=${ver//[:.]/}
  local user="R${name}${tag}"
  user=${user:0:16}
  local log="$OUT/$name.log"
  if [ "$mode" = "reconnect" ]; then
    # Short first run: the client idles on the disconnect screen afterwards,
    # and the remembered route must still be valid when it reconnects.
    launch "$name" "$ver" "$host" 120 "$mods" "$user" "$OUT/$name-pick.log" "$env"
  fi
  launch "$name" "$ver" "$host" "$timeout" "$mods" "$user" "$log" "$env"
  local haystack=$log
  if [[ "$expect" == server:* ]]; then
    local svc=${expect#server:}
    svc=${svc%%:*}
    expect=${expect#server:$svc:}
    haystack="$OUT/$name-server.log"
    docker logs --since 30m "$P-$svc-1" >"$haystack" 2>&1
  fi
  expect=${expect//\{user\}/$user}
  if grep -Eq "$expect" "$haystack"; then
    echo "PASS $name $ver -> $host: $(grep -Eo "$expect.{0,60}" "$haystack" | head -1)"
  else
    echo "FAIL $name $ver -> $host (expected /$expect/); last lines:"
    grep -hE "hmc-run|Connecting to|disconnect|\[CHAT\]|Exception|Kicked|lost connection" \
      $( [ "$mode" = "reconnect" ] && echo "$OUT/$name-pick.log" ) "$log" | tail -8 | sed 's/^/    /'
  fi
}

while read -r name ver host expect timeout mods mode env; do
  [ -z "${name:-}" ] && continue
  [[ "$name" == \#* ]] && continue
  [[ "$name" =~ $FILTER ]] || continue
  run "$name" "$ver" "$host" "$expect" "$timeout" "$mods" "$mode" "${env:--}" &
  while [ "$(jobs -rp | wc -l)" -ge "$PAR" ]; do sleep 2; done
done <<< "$SCENARIOS"
wait
echo "logs in $OUT"
