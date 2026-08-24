#!/usr/bin/env bash
# Chaos test: SIGKILL the delivery worker while attempts are in flight, restart
# it, and prove no delivery was lost.
#
# This is the XAUTOCLAIM proof. A hard kill leaves stream entries unacknowledged
# in the consumer group's pending list and rows stranded in 'delivering'. The
# restarted worker's reaper must recover both, so every delivery still reaches a
# terminal state.
#
# Usage:
#   scripts/chaos.sh [event_count]
#
# Environment:
#   API_URL, RECEIVER_URL
#   WORKER_KILL_CMD   how to hard-kill the worker  (default: docker compose)
#   WORKER_START_CMD  how to bring it back
#   SETTLE_TIMEOUT    seconds to wait for the backlog to drain (default 300)
set -uo pipefail

API=${API_URL:-http://localhost:8080}
RCV=${RECEIVER_URL:-http://localhost:9090}
COUNT=${1:-200}
SETTLE_TIMEOUT=${SETTLE_TIMEOUT:-300}
KILL_CMD=${WORKER_KILL_CMD:-"docker compose kill -s SIGKILL worker"}
START_CMD=${WORKER_START_CMD:-"docker compose up -d worker"}
JSON='Content-Type: application/json'

green() { printf '\033[32m%s\033[0m' "$1"; }
red()   { printf '\033[31m%s\033[0m' "$1"; }
section() { printf '\n\033[1m%s\033[0m\n' "$1"; }

printf '\033[1mHookRelay chaos test — kill a worker mid-delivery\033[0m\n'
printf 'api=%s events=%s\n' "$API" "$COUNT"

curl -sS -f "$API/healthz" >/dev/null || { echo "API not reachable at $API"; exit 1; }
curl -sS -f "$RCV/healthz" >/dev/null || { echo "receiver not reachable at $RCV"; exit 1; }

EMAIL="chaos-$(date +%s)-$RANDOM@hookrelay.test"
KEY=$(curl -sS -X POST "$API/auth/register" -H "$JSON" \
  -d "{\"name\":\"Chaos Run\",\"email\":\"$EMAIL\",\"password\":\"chaos-password-123\"}" | jq -r .api_key)
[ "$KEY" != "null" ] || { echo "registration failed"; exit 1; }
api() { curl -sS -H "Authorization: Bearer $KEY" "$@"; }

# A slow endpoint guarantees attempts are genuinely in flight when we pull the
# plug, rather than the kill landing between deliveries.
api -X POST "$API/endpoints" -H "$JSON" \
  -d "{\"url\":\"$RCV/slow?ms=1500\",\"description\":\"chaos-target\",\"event_types\":[\"chaos.test\"]}" >/dev/null

section "1. Publishing $COUNT events"
for i in $(seq 1 "$COUNT"); do
  api -X POST "$API/events" -H "$JSON" -d "{\"event_type\":\"chaos.test\",\"payload\":{\"n\":$i}}" >/dev/null &
  # Keep a bounded number of concurrent publishes in flight.
  if [ $((i % 20)) -eq 0 ]; then wait; fi
done
wait
INGESTED=$(api "$API/deliveries?status=&limit=1" | jq -r '[.counts[]]|add')
printf '  ingested deliveries: %s\n' "$INGESTED"

section "2. Killing the worker while attempts are in flight"
sleep 3
INFLIGHT=$(api "$API/deliveries?limit=1" | jq -r '.counts.delivering // 0')
printf '  deliveries in state "delivering" at kill time: %s\n' "$INFLIGHT"
printf '  running: %s\n' "$KILL_CMD"
eval "$KILL_CMD" || { echo "kill command failed"; exit 1; }
sleep 2
STRANDED=$(api "$API/deliveries?limit=1" | jq -r '.counts.delivering // 0')
printf '  deliveries stranded in "delivering" after the kill: %s\n' "$STRANDED"

section "3. Restarting the worker"
printf '  running: %s\n' "$START_CMD"
eval "$START_CMD" || { echo "start command failed"; exit 1; }

section "4. Waiting for the backlog to settle"
deadline=$((SECONDS + SETTLE_TIMEOUT))
while [ $SECONDS -lt $deadline ]; do
  C=$(api "$API/deliveries?limit=1" | jq -r '.counts')
  OPEN=$(echo "$C" | jq -r '((.pending//0) + (.failed//0) + (.delivering//0))')
  DONE=$(echo "$C" | jq -r '((.succeeded//0) + (.dead//0))')
  printf '\r  open=%-6s settled=%-6s elapsed=%ss   ' "$OPEN" "$DONE" "$((SECONDS))"
  [ "$OPEN" = "0" ] && break
  sleep 3
done
printf '\n'

section "Result"
C=$(api "$API/deliveries?limit=1" | jq -r '.counts')
TOTAL=$(echo "$C" | jq -r '[.[]]|add')
SUCC=$(echo "$C"  | jq -r '.succeeded//0')
DEAD=$(echo "$C"  | jq -r '.dead//0')
OPEN=$(echo "$C"  | jq -r '((.pending//0)+(.failed//0)+(.delivering//0))')
LOST=$((INGESTED - TOTAL))

printf '  ingested        %s\n' "$INGESTED"
printf '  accounted for   %s  (succeeded=%s dead=%s still open=%s)\n' "$TOTAL" "$SUCC" "$DEAD" "$OPEN"
printf '  lost            %s\n' "$LOST"

FAIL=0
[ "$LOST" -eq 0 ] || { printf '  %s %s deliveries vanished from the database\n' "$(red FAIL)" "$LOST"; FAIL=1; }
[ "$OPEN" -eq 0 ] || { printf '  %s %s deliveries never reached a terminal state\n' "$(red FAIL)" "$OPEN"; FAIL=1; }
[ "$SUCC" -eq "$INGESTED" ] || {
  printf '  %s only %s of %s deliveries succeeded against a healthy endpoint\n' "$(red FAIL)" "$SUCC" "$INGESTED"; FAIL=1; }

if [ "$FAIL" -eq 0 ]; then
  printf '\n  %s  a hard worker kill lost nothing: all %s deliveries succeeded\n\n' "$(green 'ZERO LOSS')" "$SUCC"
  exit 0
fi
printf '\n  %s  chaos test failed\n\n' "$(red 'FAILED')"
exit 1
