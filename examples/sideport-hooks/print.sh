#!/bin/sh
# Side port hook that only reports the assignment, for side ports whose users
# are told the address by hand (Geyser, BlueMap, NuVotifier, ...). It also
# writes sideports.txt in the server directory, e.g. for a MOTD or web page.
set -eu

line="$VECTA_SIDEPORT_NAME ($VECTA_SIDEPORT_PROTOCOL $VECTA_SIDEPORT_BACKEND_PORT): $VECTA_SIDEPORT_STATE"
if [ "$VECTA_SIDEPORT_STATE" = assigned ]; then
  line="$line at $VECTA_SIDEPORT_HOST:$VECTA_SIDEPORT_PORT"
elif [ -n "$VECTA_SIDEPORT_ERROR" ]; then
  line="$line ($VECTA_SIDEPORT_ERROR)"
fi
echo "$line"

touch sideports.txt
grep -v "^$VECTA_SIDEPORT_NAME " sideports.txt > sideports.txt.tmp || true
[ "$VECTA_SIDEPORT_STATE" = released ] || echo "$line" >> sideports.txt.tmp
mv sideports.txt.tmp sideports.txt
