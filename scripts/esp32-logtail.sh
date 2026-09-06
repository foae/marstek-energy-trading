#!/usr/bin/env bash
# Tail the ESPHome bridge's debug log stream (SSE /events) into a timestamped file,
# dropping the routine Modbus chatter, so an RS485 link freeze can be diagnosed from
# the device side after the fact. Reconnects when the device reboots.
#
# Usage: scripts/esp32-logtail.sh <device-url> [output-file]
#   nohup scripts/esp32-logtail.sh http://battery-bridge.local ~/esp32-events.log >/dev/null 2>&1 &
set -u
if (( $# < 1 )); then
  printf 'usage: %s <device-url> [output-file]\n' "$0" >&2
  exit 2
fi
URL=${1%/}
OUT=${2:-$HOME/esp32-events.log}
ESC=$(printf '\033')
# Stamp in the trader's timezone so device and service logs line up.
export TZ=${LOGTAIL_TZ:-Europe/Amsterdam}

log() { printf '%s %s\n' "$(date '+%F %T %Z')" "$*" >> "$OUT"; }

log "[logtail] starting against $URL"
while :; do
  # --speed-time/--speed-limit: drop a connection that goes silent for 5 minutes
  # (ESPHome pings the stream regularly; silence means the device is gone).
  curl -sN --speed-time 300 --speed-limit 1 "$URL/events" 2>/dev/null \
    | grep --line-buffered -E '^data: .\[' \
    | grep --line-buffered -vE 'requests pending, refused|Poll refused by hub|New select value' \
    | while IFS= read -r line; do
        line=${line#data: }
        line=$(printf '%s' "$line" | sed -E "s/${ESC}\[[0-9;]*m//g")
        log "$line"
      done
  log "[logtail] stream ended, reconnecting in 10s"
  sleep 10
done
