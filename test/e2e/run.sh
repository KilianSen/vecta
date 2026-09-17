#!/usr/bin/env bash
# One-command end-to-end run: build, unit tests, deploy the stack to the
# Docker host, wait for all servers, run the protocol-bot suite and the
# real-client (HeadlessMC) scenarios, write results, exit non-zero on failure.
#
#   E2E_HOST=root@docker-host GW_HOST=docker-host bash test/e2e/run.sh
#
# Needs: go, node (npm ci in test/e2e), ssh/scp access to E2E_HOST, and a
# docker CLI whose context points at that host (the bot suite reads backend
# logs through it).
set -euo pipefail

E2E_HOST=${E2E_HOST:-root@10.10.25.155}
export GW_HOST=${GW_HOST:-10.10.25.155}
REMOTE=${REMOTE:-/root/vecta-test}
EXPECT_SERVERS=${EXPECT_SERVERS:-7}
SKIP_HMC=${SKIP_HMC:-}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
E2E=$ROOT/test/e2e
RESULTS=${RESULTS:-$E2E/results}
SSH="ssh -o BatchMode=yes $E2E_HOST"
mkdir -p "$RESULTS"

step() { printf '\n== %s\n' "$*"; }

step "build and unit tests"
(cd "$ROOT" && go vet ./... && go test ./... >"$RESULTS/unit.log" 2>&1) || { cat "$RESULTS/unit.log"; exit 1; }
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$E2E/vecta" ./cmd/vecta)

step "vecta server jar"
$SSH "rm -rf /root/vecta-build/universal && mkdir -p /root/vecta-build/universal $REMOTE/vecta-jar"
tar -C "$ROOT/plugins/universal" --exclude=build -cf - . |
  $SSH "tar --no-same-owner -C /root/vecta-build/universal -xf -"
$SSH "sh /root/vecta-build/universal/build.sh >/root/vecta-build/universal.log 2>&1 || { cat /root/vecta-build/universal.log; exit 1; }; tail -n 2 /root/vecta-build/universal.log"
# Running servers keep the old jar loaded; restart them after compose up if it changed.
JAR_CHANGED=$($SSH "cmp -s /root/vecta-build/universal/build/vecta.jar $REMOTE/vecta-jar/vecta.jar || echo yes")
$SSH "cp /root/vecta-build/universal/build/vecta.jar $REMOTE/vecta-jar/vecta.jar"

step "deploy to $E2E_HOST:$REMOTE"
$SSH "mkdir -p $REMOTE/hmc $REMOTE/hooks"
scp -q "$ROOT"/examples/sideport-hooks/print.sh "$E2E_HOST:$REMOTE/hooks/"
scp -q "$E2E"/{Dockerfile,compose.yaml,gateway.json,tune-paper.sh,vecta} "$E2E_HOST:$REMOTE/"
scp -q "$E2E"/hmc/{Dockerfile,hmc-run.sh,scenarios.sh} "$E2E_HOST:$REMOTE/hmc/"
$SSH "cd $REMOTE && sed -i 's/\r\$//' tune-paper.sh hmc/*.sh hooks/*.sh &&docker build -q -t vecta-test/hmc:2.10.0 hmc >/dev/null && docker compose up -d --build --remove-orphans 2>&1 | tail -n 3"
if [ -n "$JAR_CHANGED" ]; then
  echo "server jar changed; restarting the servers that load it"
  $SSH "cd $REMOTE && docker compose restart mc-paper-legacy mc-paper-via mc-fabric mc-neoforge mc-forge 2>&1 | tail -n 1"
fi

wait_online() {
  for _ in $(seq 1 180); do
    n=$(curl -s --max-time 5 "http://$GW_HOST:38080/api/v1/servers" |
      node -e 'let d="";process.stdin.on("data",c=>d+=c).on("end",()=>{try{console.log(JSON.parse(d).filter(s=>s.online).length)}catch{console.log(0)}})')
    [ "$n" -ge "$EXPECT_SERVERS" ] && { echo "$n servers online"; return 0; }
    sleep 5
  done
  echo "timed out waiting for $EXPECT_SERVERS servers"; return 1
}
# Tune first: paper-a has proxyProtocol enabled, so the gateway sends it a PROXY
# header even on health pings, and it stays offline until tune-paper.sh turns on
# proxy-protocol. tune-paper.sh waits for each server's config files, so it can
# run before the online gate; on a cold stack this avoids a deadlock (servers
# never reach "online" before tuning, which used to run only after).
step "tune paper servers"
$SSH "sh $REMOTE/tune-paper.sh" 2>&1 | tee "$RESULTS/tune.log"
step "wait for servers"
wait_online

step "protocol-bot suite"
(cd "$E2E" && [ -d node_modules ] || npm ci --silent)
set +e
(cd "$E2E" && node clients.cjs) 2>&1 | tee "$RESULTS/bots.log"
bots=${PIPESTATUS[0]}

step "in-game command suite (/hub, /server, /global)"
(cd "$E2E" && node commands.cjs) 2>&1 | tee "$RESULTS/commands.log"
cmds=${PIPESTATUS[0]}

hmc=0
if [ -z "$SKIP_HMC" ]; then
  step "real-client scenarios"
  $SSH "bash $REMOTE/hmc/scenarios.sh 3" 2>&1 | tee "$RESULTS/hmc.log"
  grep -q '^FAIL' "$RESULTS/hmc.log" && hmc=1
fi

step "metrics snapshot"
curl -s -H "Authorization: Bearer ${METRICS_TOKEN:-e2e-metrics}" "http://$GW_HOST:38080/metrics" >"$RESULTS/metrics.txt"
grep -E '^vecta_(routes|lobby_outcomes|connections)_total' "$RESULTS/metrics.txt" | head -20
set -e

step "summary"
echo "bots: $(grep -cE '^PASS' "$RESULTS/bots.log") passed, $(grep -cE '^FAIL' "$RESULTS/bots.log") failed"
echo "commands: $(grep -cE '^PASS' "$RESULTS/commands.log") passed, $(grep -cE '^FAIL' "$RESULTS/commands.log") failed"
[ -z "$SKIP_HMC" ] && echo "real clients: $(grep -c '^PASS' "$RESULTS/hmc.log") passed, $(grep -c '^FAIL' "$RESULTS/hmc.log") failed"
[ "$bots" -eq 0 ] && [ "$cmds" -eq 0 ] && [ "$hmc" -eq 0 ]
