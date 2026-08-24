#!/usr/bin/env bash
# End-to-end verification of a running HookRelay stack.
#
# Proves, against real services: fan-out, idempotency, HMAC signing (including
# rejection of a bad signature and dual signatures during rotation), retries
# against a flaky endpoint, the dead-letter queue, replay after a receiver is
# fixed, and the per-endpoint circuit breaker.
#
# Usage:
#   scripts/verify.sh
#
# Environment:
#   API_URL       default http://localhost:8080
#   RECEIVER_URL  default http://localhost:9090
#   BREAKER_THRESHOLD  must match the worker's (default 20)
#
# The worker must run a compressed retry schedule so the dead-letter path
# completes in seconds rather than hours, e.g.
#   RETRY_SCHEDULE=1s,1s,2s,2s,3s,3s,4s
set -uo pipefail

API=${API_URL:-http://localhost:8080}
RCV=${RECEIVER_URL:-http://localhost:9090}
THRESHOLD=${BREAKER_THRESHOLD:-20}
JSON='Content-Type: application/json'

PASS=0; FAIL=0
green() { printf '\033[32m%s\033[0m' "$1"; }
red()   { printf '\033[31m%s\033[0m' "$1"; }

# check <label> <actual> <expected>
check() {
  if [ "$2" = "$3" ]; then
    printf '  %s %s (%s)\n' "$(green PASS)" "$1" "$2"; PASS=$((PASS+1))
  else
    printf '  %s %s — got %q, want %q\n' "$(red FAIL)" "$1" "$2" "$3"; FAIL=$((FAIL+1))
  fi
}
# check_ge <label> <actual> <min>
check_ge() {
  if [ "$2" -ge "$3" ] 2>/dev/null; then
    printf '  %s %s (%s >= %s)\n' "$(green PASS)" "$1" "$2" "$3"; PASS=$((PASS+1))
  else
    printf '  %s %s — got %q, want >= %q\n' "$(red FAIL)" "$1" "$2" "$3"; FAIL=$((FAIL+1))
  fi
}
section() { printf '\n\033[1m%s\033[0m\n' "$1"; }

api()  { curl -sS -H "Authorization: Bearer $KEY" "$@"; }
# poll_status <event_id> <endpoint_id|-> <wanted status> <timeout secs>
poll_status() {
  local ev=$1 ep=$2 want=$3 timeout=$4
  local deadline=$((SECONDS + timeout))
  local st=""
  while [ $SECONDS -lt $deadline ]; do
    st=$(api "$API/events/$ev" | jq -r --arg ep "$ep" \
      '[.deliveries[] | select($ep=="-" or .endpoint_id==$ep)][0].status // "none"')
    [ "$st" = "$want" ] && { echo "$st"; return 0; }
    sleep 1
  done
  echo "$st"
}
mkep() { api -X POST "$API/endpoints" -H "$JSON" -d "$1"; }
publish() { api -X POST "$API/events" -H "$JSON" -d "$1" | jq -r .event_id; }

printf '\033[1mHookRelay end-to-end verification\033[0m\n'
printf 'api=%s receiver=%s\n' "$API" "$RCV"

curl -sS -f "$API/healthz" >/dev/null || { echo "API not reachable at $API"; exit 1; }
curl -sS -f "$RCV/healthz" >/dev/null || { echo "receiver not reachable at $RCV"; exit 1; }
curl -sS -X POST "$RCV/_reset" >/dev/null
curl -sS -X POST "$RCV/_control" -H "$JSON" -d '{"switch":"fail"}' >/dev/null

# A dedicated tenant keeps every count in this run unambiguous.
EMAIL="verify-$(date +%s)-$RANDOM@hookrelay.test"
REG=$(curl -sS -X POST "$API/auth/register" -H "$JSON" \
  -d "{\"name\":\"Verify Run\",\"email\":\"$EMAIL\",\"password\":\"verify-password-123\"}")
KEY=$(echo "$REG" | jq -r .api_key)
[ "$KEY" != "null" ] || { echo "registration failed: $REG"; exit 1; }

section "1. Fan-out: one event reaches every subscribed endpoint"
for i in 1 2 3; do
  mkep "{\"url\":\"$RCV/ok\",\"description\":\"fanout-$i\",\"event_types\":[\"fanout.test\"]}" >/dev/null
done
mkep "{\"url\":\"$RCV/ok\",\"description\":\"unsubscribed\",\"event_types\":[\"other.type\"]}" >/dev/null
FAN=$(api -X POST "$API/events" -H "$JSON" -d '{"event_type":"fanout.test","payload":{"n":1}}')
check "deliveries created for 3 subscribers" "$(echo "$FAN" | jq -r .deliveries)" "3"
check "stream entries enqueued"              "$(echo "$FAN" | jq -r '.delivery_ids|length')" "3"
FANEV=$(echo "$FAN" | jq -r .event_id)
sleep 3
check "all 3 delivered" \
  "$(api "$API/events/$FANEV" | jq '[.deliveries[]|select(.status=="succeeded")]|length')" "3"
check "each succeeded on the first attempt" \
  "$(api "$API/events/$FANEV" | jq '[.deliveries[]|select(.attempt_count==1)]|length')" "3"

section "2. Idempotency-Key: a replayed publish creates nothing new"
IK="idem-$RANDOM"
A1=$(api -X POST "$API/events" -H "$JSON" -H "Idempotency-Key: $IK" -d '{"event_type":"fanout.test","payload":{"n":2}}')
A2=$(api -X POST "$API/events" -H "$JSON" -H "Idempotency-Key: $IK" -d '{"event_type":"fanout.test","payload":{"n":2}}')
check "same event id returned"      "$(echo "$A1"|jq -r .event_id)" "$(echo "$A2"|jq -r .event_id)"
check "second call flagged duplicate" "$(echo "$A2"|jq -r .duplicate)" "true"
check "second call fanned out nothing" "$(echo "$A2"|jq -r .deliveries)" "0"

section "3. HMAC signature: /verify accepts a correct signature"
VEP=$(mkep "{\"url\":\"$RCV/verify?secret=PLACEHOLDER\",\"description\":\"verify-good\",\"event_types\":[\"sig.good\"]}")
VSEC=$(echo "$VEP"|jq -r .secret); VID=$(echo "$VEP"|jq -r .id)
api -X PATCH "$API/endpoints/$VID" -H "$JSON" -d "{\"url\":\"$RCV/verify?secret=$VSEC\"}" >/dev/null
GOODEV=$(publish '{"event_type":"sig.good","payload":{"signed":true}}')
check "signed delivery succeeded" "$(poll_status "$GOODEV" - succeeded 20)" "succeeded"

section "4. HMAC signature: a wrong secret is rejected with 401"
mkep "{\"url\":\"$RCV/verify?secret=whsec_definitely_not_the_real_secret\",\"description\":\"verify-bad\",\"event_types\":[\"sig.bad\"]}" >/dev/null
BADEV=$(publish '{"event_type":"sig.bad","payload":{"signed":false}}')
check "bad signature ends up dead" "$(poll_status "$BADEV" - dead 60)" "dead"
check "receiver answered 401"      "$(api "$API/events/$BADEV" | jq -r '.deliveries[0].last_status_code')" "401"
RSTAT=$(curl -sS "$RCV/_stats")
check_ge "receiver rejected at least one request" "$(echo "$RSTAT"|jq -r '.routes["/verify"].rejected')" 1
check_ge "receiver accepted at least one request" "$(echo "$RSTAT"|jq -r '.routes["/verify"].accepted')" 1

section "5. Secret rotation: both old and new signatures verify"
ROT=$(api -X POST "$API/endpoints/$VID/rotate-secret")
NEWSEC=$(echo "$ROT"|jq -r .endpoint.secret)
check "previous secret retained" "$(echo "$ROT"|jq -r '.endpoint.previous_secret_expires_at != null')" "true"
# The receiver still holds only the OLD secret; the dual signature header must
# let it verify anyway.
OLDEV=$(publish '{"event_type":"sig.good","payload":{"during":"rotation"}}')
check "old secret still verifies during grace" "$(poll_status "$OLDEV" "$VID" succeeded 20)" "succeeded"
# Now move the receiver to the new secret; that must verify too.
api -X PATCH "$API/endpoints/$VID" -H "$JSON" -d "{\"url\":\"$RCV/verify?secret=$NEWSEC\"}" >/dev/null
NEWEV=$(publish '{"event_type":"sig.good","payload":{"after":"rotation"}}')
check "new secret verifies"        "$(poll_status "$NEWEV" "$VID" succeeded 20)" "succeeded"

section "6. Retries: a flaky endpoint eventually succeeds"
mkep "{\"url\":\"$RCV/flaky?rate=0.7\",\"description\":\"flaky\",\"event_types\":[\"flaky.test\"]}" >/dev/null
FLEV=$(publish '{"event_type":"flaky.test","payload":{"n":1}}')
check "flaky delivery eventually succeeded" "$(poll_status "$FLEV" - succeeded 90)" "succeeded"
FLATT=$(api "$API/events/$FLEV" | jq -r '.deliveries[0].attempt_count')
printf '  ...    it took %s attempt(s)\n' "$FLATT"
check_ge "attempt history recorded" "$(api "$API/events/$FLEV" | jq -r '.deliveries[0].attempts|length')" "$FLATT"

section "7. Dead-letter queue: a permanently failing endpoint is parked"
mkep "{\"url\":\"$RCV/fail?code=500\",\"description\":\"always-fails\",\"event_types\":[\"dead.test\"]}" >/dev/null
DEV=$(publish '{"event_type":"dead.test","payload":{"n":1}}')
check "delivery reached the DLQ" "$(poll_status "$DEV" - dead 90)" "dead"
DD=$(api "$API/events/$DEV")
MAXA=$(echo "$DD"|jq -r '.deliveries[0].attempt_count')
check_ge "every retry was attempted" "$MAXA" 8
check "attempts all recorded" "$(echo "$DD"|jq -r '.deliveries[0].attempts|length')" "$MAXA"
check "shows in GET /deliveries?status=dead" \
  "$(api "$API/deliveries?status=dead" | jq --arg id "$(echo "$DD"|jq -r '.deliveries[0].id')" \
     '[.deliveries[]|select(.id==$id)]|length')" "1"

section "8. Replay: fix the receiver, replay from the DLQ, delivery succeeds"
mkep "{\"url\":\"$RCV/switch\",\"description\":\"broken-then-fixed\",\"event_types\":[\"replay.test\"]}" >/dev/null
REV=$(publish '{"event_type":"replay.test","payload":{"n":1}}')
check "delivery died while receiver was broken" "$(poll_status "$REV" - dead 90)" "dead"
RDID=$(api "$API/events/$REV" | jq -r '.deliveries[0].id')
curl -sS -X POST "$RCV/_control" -H "$JSON" -d '{"switch":"ok"}' >/dev/null
printf '  ...    receiver switched to healthy\n'
check "replay accepted" "$(api -X POST "$API/deliveries/$RDID/replay" | jq -r .replayed)" "1"
check "replayed delivery succeeded" "$(poll_status "$REV" - succeeded 60)" "succeeded"
check "attempt counter reset by replay" \
  "$(api "$API/events/$REV" | jq -r '.deliveries[0].attempt_count')" "1"

section "9. Event-level replay"
EREV=$(publish '{"event_type":"fanout.test","payload":{"n":"event-replay"}}')
sleep 3
check "replays every delivery of the event" "$(api -X POST "$API/events/$EREV/replay" | jq -r .replayed)" "3"

section "10. Circuit breaker: $THRESHOLD consecutive failures pause the endpoint"
BEP=$(mkep "{\"url\":\"$RCV/fail?code=503\",\"description\":\"breaker-victim\",\"event_types\":[\"breaker.test\"]}")
BID=$(echo "$BEP"|jq -r .id)
# Each event costs one attempt against this endpoint, so publish enough to
# exceed the threshold and let the retries pile the failures up.
for _ in $(seq 1 6); do publish '{"event_type":"breaker.test","payload":{"n":1}}' >/dev/null; done
OPENED=false; SKIPPED=0
deadline=$((SECONDS+120))
while [ $SECONDS -lt $deadline ]; do
  EPJSON=$(api "$API/endpoints/$BID")
  CF=$(echo "$EPJSON"|jq -r .consecutive_failures)
  CU=$(echo "$EPJSON"|jq -r '.circuit_opened_until // "null"')
  SKIPPED=$(api "$API/deliveries?endpoint_id=$BID&include_attempts=true" \
    | jq '[.deliveries[].attempts[]?|select(.outcome=="skipped")]|length')
  if [ "$CU" != "null" ]; then OPENED=true; fi
  if [ "$OPENED" = true ] && [ "$SKIPPED" -gt 0 ]; then break; fi
  sleep 2
done
check_ge "failure streak reached the threshold" "$CF" "$THRESHOLD"
check "breaker opened (circuit_opened_until set)" "$OPENED" "true"
check_ge "attempts recorded as skipped while paused" "$SKIPPED" 1

section "Result"
TOTAL=$((PASS+FAIL))
if [ "$FAIL" -eq 0 ]; then
  printf '  %s  %d/%d checks passed\n\n' "$(green 'ALL CHECKS PASSED')" "$PASS" "$TOTAL"
  exit 0
fi
printf '  %s  %d/%d checks passed, %d failed\n\n' "$(red 'FAILURES')" "$PASS" "$TOTAL" "$FAIL"
exit 1
