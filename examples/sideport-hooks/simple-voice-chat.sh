#!/bin/sh
# Side port hook for Simple Voice Chat: points voice_host at the public
# address the gateway assigned, so clients send voice to the gateway.
#
#   vecta.properties:  sidePorts=voice:udp:24454
#                      sidePortHook=./hooks/simple-voice-chat.sh
#
# Runs in the server directory. Variables: docs/side-ports.md.
set -eu

case "$VECTA_SIDEPORT_STATE" in
  assigned) ;;
  released) value="" ;;
  *) echo "voice chat not reachable through the gateway: $VECTA_SIDEPORT_ERROR"; exit 0 ;;
esac

if [ "$VECTA_SIDEPORT_STATE" = assigned ]; then
  # Only a hostname/IP and a port go into the file.
  if ! printf '%s' "$VECTA_SIDEPORT_HOST" | grep -Eq '^[A-Za-z0-9.:-]+$' ||
     ! printf '%s' "$VECTA_SIDEPORT_PORT" | grep -Eq '^[0-9]+$'; then
    echo "refusing unexpected address '$VECTA_SIDEPORT_HOST:$VECTA_SIDEPORT_PORT'" >&2
    exit 1
  fi
  case "$VECTA_SIDEPORT_HOST" in
    *:*) value="[$VECTA_SIDEPORT_HOST]:$VECTA_SIDEPORT_PORT" ;; # IPv6 literal
    *) value="$VECTA_SIDEPORT_HOST:$VECTA_SIDEPORT_PORT" ;;
  esac
fi

file=${VOICECHAT_CONFIG:-}
if [ -z "$file" ]; then
  for f in config/voicechat/voicechat-server.properties plugins/voicechat/voicechat-server.properties; do
    if [ -f "$f" ]; then file=$f; break; fi
  done
fi
if [ -z "$file" ]; then
  # First start: the mod creates the file with its defaults and keeps our key.
  if [ -d plugins ] && [ ! -d mods ]; then file=plugins/voicechat/voicechat-server.properties
  else file=config/voicechat/voicechat-server.properties; fi
  mkdir -p "$(dirname "$file")"
  : > "$file"
fi

tmp="$file.vecta-tmp"
if grep -q '^voice_host=' "$file"; then
  sed "s|^voice_host=.*|voice_host=$value|" "$file" > "$tmp"
else
  { cat "$file"; echo "voice_host=$value"; } > "$tmp"
fi
mv "$tmp" "$file"
echo "set voice_host=$value in $file"

if [ "${VECTA_SERVER_STARTED:-}" = 1 ]; then
  echo "restart the server so Simple Voice Chat picks up the new voice_host"
fi
