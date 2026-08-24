# HookRelay

Reliable webhook delivery as a standalone service. Producers publish events to
HookRelay; HookRelay guarantees they reach every subscribed endpoint — signed
with HMAC, retried with exponential backoff for hours, and parked in a
dead-letter queue you can inspect and replay if an endpoint never comes back.

**The promise: at-least-once delivery, never silent loss.**

Verified against a real stack: 10,000 events fanned out to 30,000 deliveries
with **zero lost**, including a hard `SIGKILL` of the worker mid-flight.

```
┌── Producer ──┐        ┌──────── HookRelay ────────┐        ┌── Subscribers ──┐
│ POST /events │ ─────► │ persist → fan out → queue │ ─────► │ signed POST     │
└──────────────┘        │      delivery workers     │        │ 2xx = delivered │
                        └───────────────────────────┘        └─────────────────┘
```

---

## Contents

- [Quick start](#quick-start)
- [Architecture](#architecture)
- [Delivery state machine](#delivery-state-machine)
- [Retry schedule](#retry-schedule)
- [Verifying signatures (for subscribers)](#verifying-signatures-for-subscribers)
- [API reference](#api-reference)
- [Dashboard](#dashboard)
- [Configuration](#configuration)
- [Testing](#testing)
- [Load and chaos test results](#load-and-chaos-test-results)
- [Repository layout](#repository-layout)
- [Further reading](#further-reading)

---

## Quick start

Requires Docker. Nothing else, and nothing to pay for.

```bash
git clone <this repo>
cd hookrelay
docker compose up --build
```

| Service | URL |
|---|---|
| Dashboard | <http://localhost:3000> |
| API | <http://localhost:8080> |
| Test receiver | <http://localhost:9090> |

### Send your first webhook

```bash
API=http://localhost:8080

# 1. Register a tenant. The API key is shown once — copy it.
KEY=$(curl -s -X POST $API/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme","email":"dev@acme.test","password":"supersecret123"}' \
  | jq -r .api_key)

# 2. Subscribe an endpoint to an event type. Note the whsec_ signing secret.
curl -s -X POST $API/endpoints \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"url":"http://receiver:9090/ok","description":"orders","event_types":["order.created"]}' | jq

# 3. Publish an event.
curl -s -X POST $API/events \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"event_type":"order.created","payload":{"order_id":"ord_1","amount_cents":4999}}' | jq
# → 202 {"event_id":"01J...","deliveries":1,...}

# 4. Watch the delivery timeline.
curl -s $API/events/<event_id> -H "Authorization: Bearer $KEY" | jq
```

Then open <http://localhost:3000>, sign in with the same email and password,
and watch the attempts land.

---

## Architecture

```mermaid
flowchart LR
    subgraph Producers
        P[Your application]
    end

    subgraph HookRelay
        API["API<br/>(chi)"]
        PG[("PostgreSQL<br/>source of truth")]
        RS[("Redis Stream<br/>deliveries_stream")]

        subgraph W["Worker process"]
            RD["Reader<br/>XREADGROUP"]
            POOL["Delivery workers<br/>sign + POST + record"]
            SCH["Scheduler<br/>due retries → XADD"]
            REAP["Reaper<br/>XAUTOCLAIM + stale claims"]
        end
    end

    subgraph Subscribers
        E1[endpoint A]
        E2[endpoint B]
        E3[endpoint C]
    end

    DASH["Dashboard<br/>(Next.js)"]

    P -->|"POST /events<br/>Idempotency-Key"| API
    API -->|"1. persist event<br/>2. fan out deliveries"| PG
    API -->|"3. XADD delivery ids"| RS

    RS --> RD --> POOL
    POOL -->|"attempt, then XACK"| RS
    POOL -->|"record attempt<br/>+ state transition"| PG
    POOL -->|"HMAC-signed POST"| E1
    POOL --> E2
    POOL --> E3

    SCH -->|"next_attempt_at <= now"| PG
    SCH --> RS
    REAP -->|"reclaim abandoned work"| RS
    REAP --> PG

    DASH -->|"poll every 5s"| API
```

### The load-bearing idea

**Postgres is the source of truth; Redis is only a work pointer.**

The stream carries delivery IDs, never payloads. That single decision buys a
lot:

- Losing Redis entirely loses no data. The scheduler finds every `pending` or
  `failed` delivery in Postgres and re-enqueues it.
- Ingestion commits to Postgres *before* enqueueing. If the process dies in
  between, the rows are already durable and the scheduler picks them up.
- Stream entries stay tiny, so Redis memory is never the constraint.

### Why a delivery can never be silently dropped

Three mechanisms overlap, so no single failure loses work:

1. **`XACK` comes last.** A worker acknowledges a stream entry only after the
   attempt and its state transition are committed to Postgres, in one
   transaction. Crash before the ack and the entry stays in the consumer
   group's pending-entries list.
2. **`XAUTOCLAIM` rescues the pending list.** The reaper transfers entries idle
   longer than `REAPER_MIN_IDLE` to a live consumer, which retries them.
3. **A lease on `next_attempt_at` covers the enqueue itself.** When the
   scheduler promotes a delivery it pushes `next_attempt_at` forward by 60 s
   rather than clearing it. If the `XADD` then fails — or the process dies
   before it — the row is due again after the lease and the next tick retries.
   Without this, a lost enqueue would orphan a committed delivery forever.

The cost of this design is duplicates, not loss: a delivery may be attempted
more than once. That is the deliberate trade — see
[EXPLANATION.md](EXPLANATION.md) on why at-least-once and not exactly-once.

---

## Delivery state machine

```mermaid
stateDiagram-v2
    [*] --> pending: fan-out creates one row per subscribed endpoint

    pending --> delivering: worker claims it (status = pending only)
    delivering --> succeeded: endpoint answered 2xx
    delivering --> failed: non-2xx, timeout or connection error, retries left
    delivering --> dead: retry budget exhausted
    delivering --> dead: exceeded DELIVERY_MAX_AGE
    delivering --> pending: circuit breaker open → skipped, deferred, no attempt burned
    delivering --> pending: worker died → reaper reclaims

    failed --> pending: scheduler promotes it once next_attempt_at passes

    succeeded --> [*]
    dead --> pending: replay resets attempt_count to 0

    note right of failed
        Only the scheduler promotes failed → pending.
        A duplicate stream entry for a failed delivery
        finds a non-claimable row and is dropped, so a
        reclaim cannot cause an early retry.
    end note

    note right of dead
        The dead-letter queue. Visible at
        GET /deliveries?status=dead and on /dlq.
    end note
```

Claiming is what makes duplicates harmless. `ClaimForDelivery` flips
`pending → delivering` in a single conditional `UPDATE`; if no row comes back,
another worker already owns it or it is already settled, and the entry is acked
and dropped.

---

## Retry schedule

Each delivery gets one initial attempt plus seven retries, spread over roughly
eight hours, with ±20% jitter on every delay.

| Attempt | Delay before it | Jitter range | Cumulative (no jitter) |
|--------:|----------------:|-------------:|-----------------------:|
| 1 | — (immediate) | — | 0s |
| 2 | 5s | 4s – 6s | 5s |
| 3 | 30s | 24s – 36s | 35s |
| 4 | 2m | 1m36s – 2m24s | 2m35s |
| 5 | 10m | 8m – 12m | 12m35s |
| 6 | 30m | 24m – 36m | 42m35s |
| 7 | 2h | 1h36m – 2h24m | 2h42m35s |
| 8 | 5h | 4h – 6h | **7h42m35s** |

After attempt 8 fails, the delivery is marked `dead`.

**Why jitter.** Deliveries that fail together — a subscriber restarting, a
network blip — would otherwise retry in lockstep and hit the recovering endpoint
as one synchronised burst. ±20% smears the herd out.

**Overriding it.** `RETRY_SCHEDULE` takes a comma-separated duration list. The
test suite uses `RETRY_SCHEDULE=1s,1s,2s,2s,3s,3s,4s` so the dead-letter path
completes in seconds rather than hours.

### Circuit breaker

An endpoint that fails **20 consecutive times** is paused for **5 minutes**.
While paused, its deliveries are recorded as `skipped` attempts and deferred —
they do **not** consume the retry budget, because the endpoint being paused is
not the delivery's fault.

The breaker protects the *worker pool*, not the receiver: without it, one hard
down endpoint would tie up every worker for the full 10 s timeout on every
queued delivery, starving healthy endpoints behind it. In the load test below,
the breaker converted ~30,000 would-be timeouts into cheap skips.

Because skips do not consume the budget, `DELIVERY_MAX_AGE` (default 24 h) is
the absolute deadline that guarantees termination. Without it, deliveries for an
endpoint that stays down would defer forever instead of dead-lettering.

---

## Verifying signatures (for subscribers)

Every delivery carries these headers:

| Header | Example |
|---|---|
| `X-HookRelay-Id` | `01J8Z4M0RQ...` — the event ULID, stable across retries |
| `X-HookRelay-Timestamp` | `1750000000` — unix seconds |
| `X-HookRelay-Signature` | `v1=3f2a...` (space-separated list during rotation) |
| `X-HookRelay-Event-Type` | `order.created` |
| `X-HookRelay-Attempt` | `3` |

The signature is `HMAC-SHA256` over the exact string:

```
{id}.{timestamp}.{raw request body}
```

**Verify the timestamp before the signature**, and compare in constant time.

### Go

```go
func verify(secret, id, sigHeader, tsHeader string, body []byte) error {
	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return errors.New("bad timestamp header")
	}
	// Reject stale requests: the signature stays valid forever, the timestamp
	// is what makes a captured request unreplayable.
	if d := time.Since(time.Unix(ts, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return errors.New("timestamp outside tolerance window")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s.%d.", id, ts)
	mac.Write(body)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))

	// During a secret rotation the header carries several signatures.
	for _, got := range strings.Fields(sigHeader) {
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return nil
		}
	}
	return errors.New("signature mismatch")
}
```

### Node.js

```js
const crypto = require("node:crypto");

function verify(secret, req, rawBody) {
  const id = req.headers["x-hookrelay-id"];
  const ts = Number(req.headers["x-hookrelay-timestamp"]);
  const header = req.headers["x-hookrelay-signature"] || "";
  if (!id || !Number.isFinite(ts)) return false;

  // Freshness first.
  if (Math.abs(Date.now() / 1000 - ts) > 300) return false;

  const want =
    "v1=" +
    crypto.createHmac("sha256", secret)
          .update(`${id}.${ts}.`)
          .update(rawBody)          // the raw bytes, NOT a re-serialised object
          .digest("hex");

  const wantBuf = Buffer.from(want);
  return header.split(/\s+/).some((got) => {
    const gotBuf = Buffer.from(got);
    return gotBuf.length === wantBuf.length && crypto.timingSafeEqual(gotBuf, wantBuf);
  });
}
```

### Python

```python
import hashlib, hmac, time

def verify(secret: str, headers, raw_body: bytes) -> bool:
    event_id = headers.get("X-HookRelay-Id", "")
    try:
        ts = int(headers.get("X-HookRelay-Timestamp", ""))
    except ValueError:
        return False
    if not event_id or abs(time.time() - ts) > 300:
        return False

    mac = hmac.new(secret.encode(), digestmod=hashlib.sha256)
    mac.update(f"{event_id}.{ts}.".encode())
    mac.update(raw_body)
    want = "v1=" + mac.hexdigest()

    return any(hmac.compare_digest(got, want)
               for got in headers.get("X-HookRelay-Signature", "").split())
```

> **Sign the raw bytes.** Re-serialising the parsed JSON changes key order and
> whitespace, and the signature will not match. Capture the body before any
> JSON middleware touches it.

### Secret rotation

`POST /endpoints/{id}/rotate-secret` installs a new secret and keeps the old one
valid for **24 hours**. During that window every request carries *both*
signatures, space separated, so a receiver holding either secret verifies
successfully. Roll your receivers at any point in the window.

### Idempotency on the receiving side

Delivery is at-least-once, so a receiver **must** be idempotent. Deduplicate on
`X-HookRelay-Id` — it is the event's ULID and is identical across every retry of
that event.

---

## API reference

All authenticated routes take `Authorization: Bearer <credential>`, where the
credential is either a tenant **API key** (`hrk_…`, for producers) or a
**dashboard JWT**. Both work everywhere.

### Auth

| Method | Path | Description |
|---|---|---|
| `POST` | `/auth/register` | Create a tenant. Returns the API key **once**. |
| `POST` | `/auth/login` | Email + password → JWT. |
| `GET` | `/auth/me` | The authenticated tenant. |

### Endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/endpoints` | Create. Auto-generates a `whsec_` secret, returned once. |
| `GET` | `/endpoints` | List (secrets omitted). |
| `GET` | `/endpoints/{id}` | Detail. `?reveal_secret=true` includes the secret. |
| `PATCH` | `/endpoints/{id}` | Partial update, including `event_types`. |
| `DELETE` | `/endpoints/{id}` | Delete, cascading to deliveries. |
| `POST` | `/endpoints/{id}/rotate-secret` | Rotate with a 24 h grace window. |
| `GET` | `/endpoints/{id}/stats` | Success rate, p95 latency. `?window_hours=24`. |

Subscribe to `"*"` to receive every event type.

### Events

| Method | Path | Description |
|---|---|---|
| `POST` | `/events` | Publish. Honours `Idempotency-Key`. `202`, or `200` on a duplicate. |
| `GET` | `/events` | List with per-event delivery summaries. `?event_type=&limit=&cursor=` |
| `GET` | `/events/{id}` | **Every delivery with every attempt.** |
| `POST` | `/events/{id}/replay` | Reset and re-enqueue all of the event's deliveries. |

### Deliveries and the dead-letter queue

| Method | Path | Description |
|---|---|---|
| `GET` | `/deliveries` | `?status=dead` is the DLQ. Also `endpoint_id`, `event_id`, `limit`, `offset`, `include_attempts`. |
| `GET` | `/deliveries/{id}` | One delivery with its attempts. |
| `POST` | `/deliveries/{id}/replay` | Reset attempts and re-enqueue. |
| `POST` | `/deliveries/replay` | Bulk: `{"delivery_ids":[…]}` or `{"status":"dead","limit":1000}`. |

Replay clears the endpoint's circuit breaker too, so an operator-triggered
retry is never silently skipped by a breaker still open from the original
failure.

### Stats and health

| Method | Path | Description |
|---|---|---|
| `GET` | `/stats/overview` | Headline counts, success rate, p95. |
| `GET` | `/stats/timeseries` | Bucketed attempts for the charts. `?window_hours=1&bucket_seconds=60` |
| `GET` | `/healthz` | Liveness. |
| `GET` | `/readyz` | Readiness — checks Postgres and Redis, 503 if either is down. |

---

## Dashboard

Dark, desktop-first, polls every 5 s.

| Page | What it is for |
|---|---|
| `/login` | Sign in, or register a tenant and copy its API key. |
| `/` | Overview: counts, success rate, p95, and three charts — deliveries/min, success rate, p95 latency. |
| `/endpoints` | All endpoints with subscriptions, breaker state and failure streaks. |
| `/endpoints/[id]` | Config, signing secret (reveal + rotate), 24 h success rate, recent deliveries. |
| `/events` | Every published event with its per-status delivery breakdown. |
| `/events/[id]` | **The debugging page.** One timeline per endpoint: every attempt with number, status code, latency and error, a live countdown to the next retry, and per-endpoint replay. |
| `/dlq` | Dead deliveries with attempt history, individual and bulk replay. |

---

## Configuration

Defaults are in `backend/internal/config/config.go` and work out of the box with
`docker compose up`. The full table, plus the three that will bite you, is in
[DEPLOYMENT.md §4](DEPLOYMENT.md#4-environment-variables-and-secrets).

The ones you are most likely to change:

| Variable | Default | Purpose |
|---|---|---|
| `JWT_SECRET` | `dev-only-change-me` | **Change this.** Signs dashboard JWTs. |
| `WORKER_COUNT` | `8` | Concurrent deliveries per worker process. |
| `DELIVERY_TIMEOUT` | `10s` | Per-attempt HTTP timeout. |
| `RETRY_SCHEDULE` | *(8-hour default)* | Comma-separated backoff override. |
| `BREAKER_THRESHOLD` / `BREAKER_COOLDOWN` | `20` / `5m` | Circuit breaker. |
| `DELIVERY_MAX_AGE` | `24h` | Absolute deadline; keep it above the retry window. |
| `NEXT_PUBLIC_API_URL` | `http://localhost:8080` | Baked into the frontend **at build time**. |

---

## Testing

### Unit tests

```bash
cd backend && go test ./... -v
```

Covers signature generation and verification (including that the id, timestamp,
body and secret are each actually part of the HMAC, and that a replayed stale
request is rejected), the retry schedule and its jitter bounds, and the circuit
breaker's threshold, cooldown and reset semantics.

### End-to-end verification

Needs the stack running with a compressed retry schedule so the dead-letter path
finishes in seconds:

```bash
RETRY_SCHEDULE=1s,1s,2s,2s,3s,3s,4s docker compose up -d
scripts/verify.sh
```

29 assertions across ten areas: fan-out, `Idempotency-Key`, signature accept and
reject, rotation with dual signatures, retries beating a flaky endpoint, the
dead-letter queue, replay after fixing a receiver, event-level replay, and the
circuit breaker opening with skipped attempts recorded.

### Load test

```bash
cd loadtest
go run ./cmd/loadtest -events 10000 -concurrency 50
```

Ingests 10,000 events against three endpoints of deliberately different health,
waits for the pipeline to drain, then asserts every delivery is accounted for.
A k6 equivalent for the ingestion half is in `loadtest/k6/ingest.js`.

### Chaos test

```bash
scripts/chaos.sh 1000
```

`SIGKILL`s the worker while attempts are in flight, restarts it, and asserts
zero loss. Override `WORKER_KILL_CMD` / `WORKER_START_CMD` for non-Docker setups.

---

## Load and chaos test results

Measured on a single 4-core container running Postgres 16, Redis 7, the API and
one worker process with `WORKER_COUNT=50`. The retry schedule was compressed to
`2s,4s,6s,8s,10s,12s,15s` with `DELIVERY_TIMEOUT=2s` and
`DELIVERY_MAX_AGE=180s` so the run finished in minutes instead of eight hours.

### 10,000 events → 30,000 deliveries

Three endpoints: `/ok` (always 200), `/flaky?rate=0.3` (30% 500s),
`/slow?ms=3000` (always exceeds the 2 s timeout).

**Ingestion**

| Metric | Value |
|---|---|
| Events published | 10,000 / 10,000, 0 failed |
| Wall clock | 11.5s |
| Throughput | **868 events/sec** |
| Latency p50 | 27ms |
| Latency p95 | **189ms** |
| Latency p99 | 289ms |
| Deliveries created | 30,000 (exactly 10,000 × 3) |

**Delivery accounting — the point of the exercise**

| Metric | Value |
|---|---|
| Created by ingestion | 30,000 |
| Found in the database | 30,000 |
| Succeeded | 19,999 |
| Dead-lettered | 10,001 |
| Still open (pending/failed/delivering) | **0** |
| **Lost** | **0** |

`10,000` of the dead deliveries are the `/slow` endpoint, which never answers
inside the timeout — correct behaviour. The one extra is a `/flaky` delivery
that lost eight coin flips in a row: at a 30% failure rate that is
`0.3^8 × 10,000 ≈ 0.66` expected, so exactly the tail you would predict.

**Healthy endpoint (`/ok`)**

| Metric | Value |
|---|---|
| Deliveries | 10,000 |
| Success rate | 100.00% |
| Attempt latency p95 | 2ms |
| End-to-end p50 (publish → delivered) | 13.3s |
| End-to-end p95 (publish → delivered) | **13.7s** |

**Attempt outcomes across all endpoints**

| Outcome | Count |
|---|---|
| succeeded | 19,999 |
| failed | 14,492 |
| skipped (breaker open, no HTTP call) | 30,431 |
| **total attempts** | **64,922** |

**Assertions — all passed**

```
PASS ingested 10000 of 10000 events
PASS zero lost deliveries (created 30000, accounted for 30000)
PASS queue drained within 8m0s
PASS no delivery stuck in a non-terminal state (0 open)
PASS every delivery settled (30000 of 30000)
PASS healthy endpoint dead-lettered nothing (0 dead)
```

### Reading the end-to-end number honestly

13.7 s p95 for an endpoint that answers in 2 ms is **head-of-line blocking, not
slowness**. All 30,000 deliveries share one Redis stream, and every `/slow`
attempt occupies a worker for the full 2 s timeout, so healthy deliveries queue
behind unhealthy ones. The circuit breaker limits the damage — it is why the run
finished at all — but it only engages after 20 consecutive failures.

With only the healthy endpoint under load, end-to-end latency is milliseconds.
The fix for the mixed case is per-tenant or per-endpoint stream sharding so one
bad subscriber cannot delay everyone else; it is listed as a known gap in
[DEPLOYMENT.md §11](DEPLOYMENT.md#11-production-hardening-checklist) and
discussed in [EXPLANATION.md](EXPLANATION.md). It is a real limitation of this
implementation, not a measurement artefact.

### Chaos: `SIGKILL` the worker mid-delivery

`scripts/chaos.sh 1000` publishes 1,000 events against an endpoint that takes
1.5 s to answer, waits until attempts are genuinely in flight, then hard-kills
the worker with `SIGKILL` — no graceful drain, no chance to `XACK` — and
restarts it.

| Phase | Observation |
|---|---|
| At kill time | 50 deliveries in state `delivering` (one per worker goroutine) |
| Immediately after the kill | the same 50 stranded in `delivering`, their stream entries unacked in the consumer group's PEL |
| Recovery | reaper logged `reclaimed deliveries abandoned mid-attempt count=50`, then `reclaimed pending stream entries count=101` |
| Drained after | 43s |
| **Result** | **1,000 / 1,000 succeeded, 0 dead, 0 still open, 0 lost, 0 worker errors** |

Both recovery paths fired, which is the design working as intended:

1. **`ReclaimStale`** found the 50 rows stuck in `delivering` past
   `REAPER_MIN_IDLE` and returned them to `pending`, re-enqueueing them —
   necessary because their original stream entry may already have been acked.
2. **`XAUTOCLAIM`** transferred the unacknowledged stream entries from the dead
   consumer to the live one, so nothing was left orphaned in the pending list.

This is the concrete proof behind the at-least-once claim. A worker can die at
the worst possible moment — after claiming a delivery, after sending the HTTP
request, before recording the result — and the delivery still completes.

---

## Repository layout

```
hookrelay/
├── docker-compose.yml       postgres, redis, api, worker, receiver, frontend
├── README.md                this file
├── EXPLANATION.md           design walkthrough + interview questions
├── DEPLOYMENT.md            everything to do after the code exists
├── backend/                 Go 1.24+
│   ├── cmd/api/             HTTP API, runs migrations on startup
│   ├── cmd/worker/          delivery pool, scheduler, reaper
│   └── internal/
│       ├── config/          environment configuration
│       ├── db/              pgx pool + embedded migrations
│       ├── handlers/        chi routes, middleware, auth
│       ├── httpx/           JSON responses and error mapping
│       ├── models/          domain types
│       ├── queue/           Redis Streams wrapper
│       ├── repos/           SQL data access, the delivery state machine
│       ├── services/        signing, backoff, breaker, ingestion, auth
│       └── workers/         deliverer + pool/scheduler/reaper
├── frontend/                Next.js 16, TypeScript, Tailwind, recharts
├── receiver/                deliberately misbehaving test endpoint
├── .github/workflows/ci.yml  build, vet, gofmt, tests, verify.sh, chaos.sh
├── Makefile                 make up / check / verify / chaos / loadtest
├── loadtest/
│   ├── cmd/loadtest/        Go load test with the zero-loss assertions
│   └── k6/ingest.js         k6 equivalent for the ingestion half
└── scripts/
    ├── verify.sh            29-assertion end-to-end check
    └── chaos.sh             kill the worker mid-flight, prove zero loss
```

---

## Further reading

- **[EXPLANATION.md](EXPLANATION.md)** — every component and design decision in
  plain language, the alternatives considered and rejected, and the ten hardest
  questions this architecture invites, answered properly.
- **[DEPLOYMENT.md](DEPLOYMENT.md)** — hosting, Redis and Postgres setup,
  secrets, free GitOps/CD options, monitoring, backups, and an honest hardening
  checklist of what this codebase does not do yet.
