#!/bin/sh
# Run on the Docker host after the stack is healthy once (config files exist).
# - Every player reaches backends from the gateway's IP, so Paper's per-IP login
#   throttle must be off (or PROXY protocol on) or rapid joins get rejected.
# - paper-a receives PROXY protocol v2 from the gateway (proxyProtocol: true).
set -eu
P=${PROJECT:-anymcp-test}
for svc in mc-paper-a mc-paper-via; do
  docker exec "$P-$svc-1" sed -i 's/connection-throttle: .*/connection-throttle: -1/' /data/bukkit.yml
done
docker exec "$P-mc-paper-a-1" sed -i 's/proxy-protocol: false/proxy-protocol: true/' /data/config/paper-global.yml
docker exec "$P-mc-paper-a-1" grep -n "proxy-protocol" /data/config/paper-global.yml
docker exec "$P-mc-paper-via-1" grep -n "connection-throttle" /data/bukkit.yml
docker restart "$P-mc-paper-a-1" "$P-mc-paper-via-1"
