# EXPLANATION

A plain-language walkthrough of how HookRelay works, why each piece is built the
way it is, what the alternatives were, and the hard questions this architecture
invites.

---

## Part 1 — What problem this actually solves

You run an API. Your customers want to know when things happen: an order was
created, a payment failed, a document finished processing. So you POST to a URL
they gave you.

That works right up until it doesn't:

- Their server is down for a deploy. Your POST fails. The event is gone.
- Their server is slow. Your request thread blocks for 30 seconds.
- Their server returns 200 but you sent the same event twice, and now they
  charged the customer twice.
- Someone spoofs a request to their webhook URL, and they have no way to tell it
  was not you.
- A whole day of events fails and nobody notices until the customer complains.

HookRelay is the layer that absorbs all of that. You hand it an event; it takes
responsibility for getting that event to every subscriber, keeps trying for
hours, and gives you a dashboard showing exactly what happened to every single
attempt. If it truly cannot deliver, it parks the event where a human can see it
and replay it later.

The promise is narrow and precise: **at-least-once delivery, never silent
loss.** Every event either reaches the subscriber, or ends up visible in a
dead-letter queue. It never simply disappears.

---

## Part 2 — The components, one at a time

### The data model

Six tables. The shape of them *is* the design.

**`tenants`** — one row per customer of HookRelay. Holds an API key hash for
producers and an email/password for the dashboard.

The API key is stored as a plain **SHA-256** digest, not bcrypt. That looks
wrong at first glance, so: bcrypt is designed to be slow, which is right for
human passwords (low entropy, guessable, worth brute-forcing). An API key here
is 32 random bytes — 256 bits of entropy. Nobody is brute-forcing that, ever.
What we *do* need is to authenticate a request in one indexed lookup, which a
salted hash cannot give you because you would have to try every row. So:
SHA-256 for the key, bcrypt for the password. Different threat models, different
tools.

**`endpoints`** — a subscriber URL owned by a tenant, with its signing secret,
an `active` flag, and the circuit breaker's state (`consecutive_failures`,
`circuit_opened_until`).

The breaker state lives in the table, not in worker memory. That matters: with
several worker processes, in-memory state would give each one its own opinion of
whether an endpoint is healthy, and a restart would forget everything. In the
table, every worker shares one view and it survives restarts.

**`subscriptions`** — `(endpoint_id, event_type)`. A join table, because an
endpoint subscribes to many event types and an event type has many subscribers.
The literal `"*"` subscribes to everything.

**`events`** — immutable. A ULID primary key, the event type, and the payload as
`JSONB`.

ULID rather than UUID because ULIDs sort lexicographically by creation time.
That gives ordered listing and keyset pagination (`WHERE id < $cursor ORDER BY
id DESC`) with no separate timestamp index, and no random-insert index churn.

`JSONB` rather than `TEXT` because it validates the JSON on write and lets you
query inside payloads later. The cost is that `JSONB` does not preserve key
order or whitespace — which is exactly why the *signature* is computed over a
freshly serialised canonical body rather than the bytes the producer sent.

**`deliveries`** — the heart of it. One row per `(event, endpoint)` pair. This
is where the state machine lives: `status`, `attempt_count`, `next_attempt_at`,
`last_status_code`, `last_error`.

A `UNIQUE (event_id, endpoint_id)` constraint makes fan-out idempotent. Combined
with `ON CONFLICT DO NOTHING`, a retried fan-out cannot create duplicate rows,
so a crash between insert and enqueue is safe to replay.

`tenant_id` is denormalised onto this table. Strictly it is derivable through
`event_id`, but every dashboard query is tenant-scoped and paying for a join on
the hottest table to re-derive something we already know is a bad trade.

**`delivery_attempts`** — one row per HTTP attempt: status code, latency, error,
outcome, timestamp. Append-only. This is what makes the dashboard's timeline
possible, and it is the difference between "your webhook failed" and "attempt 3
of 8 got a 503 after 1,204 ms, next retry in 8 minutes".

The uniqueness guard here is subtle and worth spelling out:

```sql
CREATE UNIQUE INDEX delivery_attempts_no_idx
    ON delivery_attempts (delivery_id, attempt_no)
    WHERE outcome <> 'skipped';
```

A **partial** unique index. Real attempts are deduplicated by number, so a
duplicate stream delivery cannot double-count one try. Skipped attempts (circuit
breaker open) are append-only, because an endpoint can be skipped many times at
the same attempt number — the skip does not advance the counter. Getting this
wrong is what a first implementation does; the symptom is either lost skip
history or a constraint violation that strands deliveries.

> A real bug from building this: the `ON CONFLICT (delivery_id, attempt_no)`
> clause silently did not match the partial index, because Postgres requires the
> index predicate to be repeated in the conflict target. Every attempt failed to
> record with SQLSTATE 42P10. The instructive part is what happened next:
> **nothing was lost.** Workers left the stream entries unacknowledged, the
> reaper kept reclaiming them, and once the query was fixed all the stranded
> deliveries completed. The bug exercised the at-least-once machinery by
> accident and it held.

### Ingestion: `POST /events`

1. Validate the event type and payload (256 KiB cap, must be valid JSON).
2. If an `Idempotency-Key` was sent, look for an earlier event with that key. If
   found, return the original and **do nothing else**.
3. In one transaction: insert the event, find every active endpoint subscribed to
   this event type, and insert one `pending` delivery per endpoint.
4. Commit.
5. **Then** `XADD` the delivery IDs to the Redis Stream.
6. Return `202` with the event ID.

The ordering in steps 4 and 5 is the whole trick. Everything is durable in
Postgres before Redis is told anything. If the process dies between commit and
enqueue, the deliveries are already `pending` with a `next_attempt_at`, and the
scheduler picks them up on its next tick. The enqueue is an *optimisation* that
makes delivery fast; it is not what makes delivery reliable.

Note what happens with no subscribers: the event is still recorded. That matters
for debugging, because "why did nothing fire?" is a question you can only answer
if you can see the event that had nowhere to go.

### The queue: Redis Streams

The stream carries **delivery IDs and nothing else**. Not the payload, not the
URL, not the secret.

Consequences of that:

- Redis memory is never the constraint. A million queued deliveries is a few
  tens of MB.
- Losing Redis loses no data. Postgres has everything; the scheduler rebuilds
  the queue.
- A delivery always reads the *current* endpoint configuration. Change the URL
  or rotate the secret and in-flight retries pick it up.

A consumer group (`delivery_workers`) gives us three things a plain list could
not: each entry goes to exactly one consumer, unacknowledged entries stay in the
group's pending-entries list (PEL), and `XAUTOCLAIM` lets a live worker take
over a dead worker's entries.

### The worker process

Four kinds of goroutine, one lifecycle, one graceful shutdown.

**Reader** — `XREADGROUP` with a 2 s block, pushing entries into an internal
channel sized to the worker count. That bounded channel is deliberate
backpressure: a slow pool stops the reader rather than piling work in memory.

**Delivery workers** (`WORKER_COUNT` of them) — the actual work:

1. `ClaimForDelivery`: a conditional `UPDATE` flipping `pending → delivering`.
   If no row comes back, someone else has it or it is already settled — ack the
   entry and drop it. **This is what makes duplicate stream entries harmless.**
2. Load the delivery, event and endpoint in one joined query.
3. Check the absolute deadline (`DELIVERY_MAX_AGE`). Past it, dead-letter with a
   reason.
4. Check the circuit breaker and the `active` flag. If either says no, record a
   `skipped` attempt and defer — without burning an attempt.
5. Build the canonical body, sign it, POST it with a 10 s timeout.
6. In one transaction: record the attempt, transition the delivery
   (`succeeded` / `failed` with `next_attempt_at` / `dead`), and update the
   endpoint's failure streak.
7. **Then** `XACK`.

Step 7 after step 6, always. If the process dies in between, the entry stays
pending and gets reclaimed. If `Deliver` returns an error, the worker
deliberately does *not* ack — leaving the entry for the reaper is the correct
response to "I could not record what happened".

**Scheduler** — every second, promote deliveries whose `next_attempt_at` has
passed from `failed` to `pending` and enqueue them. `FOR UPDATE SKIP LOCKED`
means several schedulers can run concurrently without handing the same row out
twice.

The lease is the non-obvious part. Rather than clearing `next_attempt_at` on
promotion, the scheduler pushes it forward 60 seconds. If the `XADD` then fails,
or the process dies before it, the row is due again after the lease and the next
tick retries. Clearing the timestamp instead would leave a `pending` row that no
query ever selects again — a silent, permanent loss. This was the single
subtlest correctness issue in the whole build.

**Reaper** — every 15 s, two complementary rescues:

1. `ReclaimStale`: rows stuck in `delivering` longer than `REAPER_MIN_IDLE`
   belong to a worker that died mid-attempt. Return them to `pending` and
   re-enqueue, because their original stream entry may already be acked.
2. `XAUTOCLAIM`: stream entries idle longer than `REAPER_MIN_IDLE` are
   transferred to this consumer and fed back through the same channel the reader
   uses.

Both are needed. The first covers "the row is stranded but the entry is gone";
the second covers "the entry is stranded in the PEL". The chaos test shows both
firing: `count=50` stale rows and `count=101` reclaimed entries.

### Signing

Body:

```json
{"id":"<event ULID>","event_type":"order.created","timestamp":1750000000,"data":{...}}
```

Signed material:

```
{id}.{timestamp}.{body}
```

Header: `X-HookRelay-Signature: v1=<hex HMAC-SHA256>`, or several
space-separated signatures during a rotation.

Signing `id` and `timestamp` alongside the body — rather than the body alone —
is what makes each request individually verifiable. A body-only signature stays
valid forever, so a captured request could be replayed verbatim at any point in
the future and the receiver could not tell. With the timestamp inside the HMAC,
a receiver rejects anything outside a tolerance window; with the id inside, it
can deduplicate. Neither is possible if those values are only in headers an
attacker can rewrite.

The `v1=` prefix is version negotiation: when SHA-256 needs replacing, `v2=`
signatures can be sent alongside `v1=` and receivers migrate at their own pace.
Exactly the same mechanism as the rotation grace window.

Rotation: `previous_secret` plus `previous_secret_expires_at`. For 24 hours both
signatures are sent, so a receiver holding either secret verifies. Without the
overlap, rotating a secret would mean dropping every in-flight delivery — so in
practice nobody would ever rotate.

### The circuit breaker

Twenty consecutive failures pauses an endpoint for five minutes.

The purpose is worth being precise about: **it protects the worker pool, not the
receiver.** A hard-down endpoint would otherwise consume a worker for the full
10 s timeout on every queued delivery. With 10,000 queued deliveries and 50
workers, that is over half an hour of the entire pool doing nothing but waiting
on one broken subscriber, while healthy endpoints queue behind it.

The load test measured exactly this: the breaker turned roughly 30,000 would-be
timeouts into cheap skips. Without it, that run would not have finished.

Two decisions inside it:

- **Skips do not consume the retry budget.** The endpoint being paused is not
  the delivery's fault, so it would be wrong to spend a retry on it.
- **One success closes the breaker completely.** No half-open probing state. The
  endpoint has demonstrably recovered; holding it half-open would only delay
  legitimate traffic for no information gain.

The first decision creates a problem, and it is worth showing the reasoning
rather than hiding it. If skips never advance the counter, a delivery for an
endpoint that stays down forever gets deferred every five minutes, *forever*. It
never dies, so it never reaches the dead-letter queue, and the queue grows
without bound. The fix is `DELIVERY_MAX_AGE` — an absolute deadline,
independent of attempts, that guarantees termination. This gap only became
visible when the load test ran a permanently-failing endpoint at scale, which is
a decent argument for load-testing the unhappy path.

### The dashboard

Next.js App Router, entirely client-side. Every page is a client component
polling the API every 5 seconds.

No server-side rendering of data, no Next.js API routes, no BFF layer. The
dashboard is just another API client authenticating with a bearer token — the
same API a producer uses. That keeps exactly one authorisation surface. Adding a
Next.js server layer would mean a second place where "can this tenant see this
delivery?" gets decided, and two answers to that question is how data leaks
happen.

The JWT lives in `localStorage`. An `httpOnly` cookie would be better against
XSS, but cookies need a server on the same origin to set them, which would mean
the BFF layer just rejected. Given the alternative, this is the honest trade for
an internal operations dashboard, and it is called out in the hardening
checklist rather than glossed over.

Polling every 5 s rather than SSE or WebSockets: 5 s is fast enough to watch a
load test, one `GET` is trivially cacheable and debuggable, and it survives any
proxy. SSE would be genuinely better for the event detail page and is the
obvious next improvement.

---

## Part 3 — Alternatives considered and rejected

### Kafka instead of Redis Streams

**Rejected.** Kafka's real strengths — partitioned ordered logs, long retention,
replay from arbitrary offsets, multiple independent consumer groups over the
same data — are things HookRelay does not need. Retention lives in Postgres.
Replay is a database operation, not an offset seek. There is one consumer group.

What we *do* need is per-message acknowledgement and redelivery of individual
failed messages. Kafka does not do that: it tracks a per-partition offset, so a
single slow or failing message blocks its partition, and you end up building
retry topics and a dead-letter topic just to get back to per-message semantics.
Redis Streams gives per-entry acks and `XAUTOCLAIM` natively.

And the operational cost is not close. Redis is one container with one config
line that matters. Kafka is a cluster, a schema for topic layout, and a
partition-count decision you cannot easily change later.

If HookRelay needed millions of events per second across many independent
consumers, the answer flips. At this scale it would be all cost and no benefit.

### Postgres alone, with `SELECT … FOR UPDATE SKIP LOCKED`

**Genuinely close, and defensible.** One less moving part, transactional
enqueue, no lost-enqueue window at all.

Rejected for two reasons. First, polling. Without a queue you either poll
constantly (burning database CPU at idle) or accept latency equal to your poll
interval. `LISTEN`/`NOTIFY` helps but is fire-and-forget: a notification
delivered while no worker is listening is simply gone, so you still need the
poll as a backstop.

Second, and more importantly, this is a project *about* queue semantics. Using
Redis Streams means the PEL, `XACK` ordering and `XAUTOCLAIM` recovery are all
explicit and visible, rather than hidden inside a `SKIP LOCKED` query. The
scheduler is in effect the Postgres-only design, kept as the durable fallback —
so the system has both.

### RabbitMQ

**Rejected.** Good at what it does. But its delayed-message support needs a
plugin, and per-message backoff schedules are awkward — the usual pattern is a
chain of dead-letter exchanges with different TTLs, one per retry stage. A
`next_attempt_at` column plus a scheduler is dramatically simpler to reason
about and to inspect: `SELECT * FROM deliveries WHERE next_attempt_at < now()`
answers "what is about to retry?" with no broker introspection at all.

### SQS / Cloud Tasks

**Rejected on the free/open-source constraint,** but the design comparison is
still interesting. SQS caps visibility timeout extension at 12 hours and message
retention at 14 days, and its per-message delay is capped at 15 minutes — so the
2 h and 5 h retry stages would need a scheduler anyway. Cloud Tasks handles long
delays well and would genuinely simplify the worker, at the cost of vendor lock
and no self-hosting.

### Exactly-once delivery

**Rejected as impossible,** which is the honest answer rather than a
cop-out — see the interview questions below.

### Storing payloads in Redis

**Rejected.** Faster (no Postgres read on the delivery path) but it makes Redis
stateful in a way that matters, multiplies memory by payload size, and means a
retry sends a stale snapshot of the endpoint config. The extra query is one
indexed primary-key lookup.

### bcrypt for API keys

**Rejected.** Covered above: it would force a full table scan per
authentication.

### An in-memory circuit breaker

**Rejected.** Covered above: it fragments across processes and forgets on
restart.

---

## Part 4 — The ten hardest questions this raises

### 1. Why at-least-once and not exactly-once?

Because exactly-once delivery over a network is not achievable, and claiming it
would be a lie.

The core problem: a sender POSTs, and the connection drops before the response
arrives. The sender cannot distinguish "the receiver never got it" from "the
receiver processed it and the acknowledgement was lost". Those two states look
identical from outside. So the sender must choose:

- **Retry** → the receiver might process it twice. That is at-least-once.
- **Do not retry** → the receiver might never get it. That is at-most-once.

There is no third option, and no amount of protocol design creates one, because
the ambiguity is in the network, not the code. This is the Two Generals problem
with different clothes.

What systems that advertise "exactly-once" actually provide is at-least-once
delivery plus idempotent processing, with the deduplication key managed for you.
Kafka's exactly-once semantics work because the consumer's offset commit and its
output write land in the same transaction — which requires the output to be
*inside Kafka*. An arbitrary HTTP endpoint on someone else's infrastructure
cannot participate in that transaction.

So HookRelay does the honest version: guarantee at-least-once, make the
deduplication key obvious and stable, and document loudly that receivers must be
idempotent. `X-HookRelay-Id` is the event's ULID and is byte-identical across
every retry. A receiver storing processed IDs gets effectively-exactly-once, and
it gets it in the only place where the required transaction actually exists — its
own database.

### 2. Why Redis Streams over Kafka here?

Four reasons, in order of weight.

**Per-message acknowledgement.** This is the decisive one. Webhook delivery
means individual messages fail independently and need individual retries at
individual times. Redis Streams tracks per-entry state in the PEL. Kafka tracks
one offset per partition — a single failing message either blocks its partition
or gets skipped, and recovering per-message semantics means building retry
topics and a DLQ topic on top.

**Retention is not our problem.** Kafka's headline feature is a durable,
replayable log. HookRelay already has one, in Postgres, with richer query
support. Paying Kafka's operational cost for a feature we duplicate is
backwards.

**One consumer group.** Kafka shines when many independent consumers read the
same stream at their own pace. There is exactly one consumer here.

**Operational weight.** Redis is one container; the only config that matters is
`appendonly yes` and `maxmemory-policy noeviction`. Kafka is a cluster, a
partition-count decision that is painful to change, and a whole discipline.

The honest counter-argument: at very high volume with multiple tenants, Kafka's
partitioning would give per-tenant isolation that this design lacks — see
question 9. That is a real trade, not a dismissal.

### 3. How does XAUTOCLAIM prevent loss?

By making "a worker claimed this and vanished" a recoverable state instead of a
terminal one.

When a consumer group entry is read with `XREADGROUP`, Redis moves it into the
group's **pending-entries list** with the consumer's name and a delivery
timestamp. It stays there until acknowledged. `XACK` is the only thing that
removes it.

So if a worker is `SIGKILL`ed mid-delivery, the entry does not vanish and it
does not silently redeliver either — it sits in the PEL, owned by a consumer
that no longer exists, with a steadily growing idle time.

`XAUTOCLAIM` scans the PEL for entries idle longer than a threshold and
transfers ownership to a live consumer, returning them so they can be
reprocessed. The reaper runs it every 15 seconds with a 60-second idle
threshold.

The load-bearing detail is that **`XACK` happens after the attempt is committed
to Postgres, never before.** That ordering is what makes the PEL meaningful: an
entry still pending is, by construction, an attempt whose outcome was never
recorded. If we acked first and then recorded, a crash in between would lose the
delivery with no trace.

There is a second, independent recovery path, because `XAUTOCLAIM` alone is not
sufficient. Consider: a worker acks the entry, then dies before its transaction
commits. Now the row is stuck in `delivering` and there is no stream entry to
reclaim. `ReclaimStale` covers that by finding `delivering` rows older than the
threshold and re-enqueueing them. The chaos test shows both firing — 50 stale
rows and 101 reclaimed entries — which is why it recovers 1,000/1,000.

Cost: duplicates. An entry reclaimed after 60 seconds may be delivered a second
time when the original worker was merely slow. Hence the conditional claim, and
hence at-least-once.

### 4. Why sign timestamp + id, not just the body?

Because a signature over the body alone is valid forever, and a signature that
never expires is a replay attack waiting to happen.

Suppose the signed material is just the body. An attacker who observes one
legitimate request — via a logging proxy, a misconfigured CDN, a leaked APM
trace — now holds a `(body, signature)` pair that verifies correctly for the
rest of time. They can resend it whenever they like. The receiver has no basis
to refuse: the signature is genuinely valid.

Binding the timestamp into the HMAC fixes it. The receiver checks that the
timestamp is within a tolerance window (5 minutes here) *and* that the signature
covers that timestamp. A captured request is unusable after five minutes, and
the attacker cannot move the window because changing the timestamp invalidates
the signature.

The id serves a different purpose: deduplication. Because delivery is
at-least-once, receivers need a stable key, and it must be signed or an attacker
could rewrite it to bypass the receiver's dedup store and force reprocessing.

Why not just put them in headers? Because unsigned headers are attacker
controlled. Anything a security decision depends on has to be inside the signed
material. A header outside the HMAC is a suggestion, not evidence.

Format detail: the separator matters. Concatenating `id + timestamp + body`
without delimiters would let `("ab", "1")` and `("a", "b1")` produce identical
signed strings. The `.` delimiter, with neither id nor timestamp able to contain
one, makes the encoding unambiguous.

### 5. What happens if Postgres goes down? What if Redis does?

**Redis down** — degraded, no loss. Ingestion still commits events and
deliveries (the `XADD` failure is logged and swallowed on purpose). Workers
cannot read. When Redis returns, `EnsureGroup` recreates the stream and group
with `MKSTREAM`, and the scheduler finds every `pending`/`failed` delivery whose
`next_attempt_at` has passed and re-enqueues it. Lost: delivery latency. Lost
data: none. Even a total Redis wipe is survivable, which is precisely why the
stream carries only IDs.

**Postgres down** — hard failure, and correctly so. Ingestion returns 500 rather
than accepting an event it cannot durably store; accepting it would be lying to
the producer. Workers cannot claim or record, so they leave entries
unacknowledged, which is the right response — those entries are reclaimable.
`/readyz` returns 503 so a load balancer takes the instance out. On recovery,
everything resumes from committed state.

The asymmetry is deliberate. One store is the truth and must be available for
correctness; the other is a performance cache for work pointers.

### 6. Why is the retry schedule 5s → 5h rather than pure exponential backoff?

Pure exponential backoff (`base × 2^n`) is tuned for contention — many clients
competing for one resource, needing to spread out fast. Webhook retries have a
different shape: the endpoint is *down*, and the question is "how long until a
human fixes it?"

The chosen schedule is exponential-ish but hand-tuned around real failure
durations:

- **5s, 30s** — transient. A dropped connection, a pod restarting, a brief GC
  pause. Most failures are resolved here, and retrying fast means the subscriber
  never notices.
- **2m, 10m** — a deploy. Long enough for a rolling update to finish.
- **30m, 2h** — an incident. Someone has been paged and is working on it.
- **5h** — the last chance. Total window ≈ 7h42m, so an endpoint that breaks at
  5pm and gets fixed the next morning still receives its events.

Pure doubling from 5 s would need 13 attempts to span 8 hours, most of them
clustered pointlessly in the first two minutes. This schedule spends its attempts
where failures actually resolve.

Jitter (±20%) handles the herd problem that exponential backoff is usually
credited with. Deliveries that failed together — one endpoint restarting, one
network blip — would otherwise retry in perfect lockstep and hit the recovering
endpoint with a synchronised burst, quite possibly knocking it over again. ±20%
smears them out.

### 7. How do you prevent a duplicate stream entry from causing a duplicate delivery?

Layered, because no single check is sufficient.

**The conditional claim.** `ClaimForDelivery` only matches `status = 'pending'`:

```sql
UPDATE deliveries SET status = 'delivering'
WHERE id = $1 AND status = 'pending'
RETURNING *
```

If two workers race, one wins and the other gets zero rows, acks and drops. If
the delivery already succeeded, no match. This is the primary defence and it is
atomic without any explicit locking.

**`failed` is not claimable.** This one is easy to get wrong. A `failed`
delivery has a future `next_attempt_at`; only the scheduler promotes it to
`pending`. So a reclaimed stream entry for a delivery that is currently backing
off finds a non-claimable row and is dropped, rather than triggering an early
retry that would burn an attempt ahead of schedule.

**Attempt-number uniqueness.** The partial unique index on
`(delivery_id, attempt_no)` with `ON CONFLICT DO NOTHING` means that even if the
same attempt number is recorded twice, the history stays correct.

**Idempotency at ingestion.** A unique partial index on
`(tenant_id, idempotency_key)` stops duplicate events at the door. Two
concurrent requests with the same key: one inserts, the other catches the
unique violation and returns the winner's event.

What none of this prevents: a worker that successfully POSTs, then dies before
recording. The receiver got it; we do not know that. The reaper reclaims and we
POST again. **That duplicate is unavoidable** — it is question 1 in concrete
form — and it is why receivers must deduplicate on `X-HookRelay-Id`.

### 8. Why does the scheduler take a lease instead of clearing `next_attempt_at`?

Because clearing it creates a permanent silent loss, and this was the subtlest
bug in the build.

The obvious implementation: find deliveries where `next_attempt_at <= now()`,
set `status = 'pending', next_attempt_at = NULL`, and enqueue them.

Now suppose the `XADD` fails — Redis restarted, memory limit hit, network
blip — or the process dies between the `UPDATE` and the `XADD`. The row is now
`pending` with `next_attempt_at = NULL`. The scheduler's query requires
`next_attempt_at IS NOT NULL`, so it will never select that row again. There is
no stream entry, so no worker will ever see it. The delivery is committed,
visible in the dashboard as pending, and permanently stuck. Exactly the silent
loss the whole system exists to prevent.

The lease fixes it: `next_attempt_at = now() + 60s`. If the enqueue succeeds, a
worker picks the delivery up in milliseconds and the lease is irrelevant. If it
fails, the row becomes due again in 60 seconds and the next tick retries. The
same pattern applies at fan-out (`CreateFanout` sets a lease rather than a bare
`now()`, so the scheduler cannot double-enqueue what ingestion just enqueued) and
on replay.

The general principle: **never let a state transition depend on a subsequent
non-transactional operation succeeding.** Either make the operation part of the
transaction, or leave a timer that reconstructs the intent. The load test's
`LOST: 0` line is only trustworthy because of this.

### 9. What breaks first as this scales, and what would you fix?

**Head-of-line blocking, and it is already visible in the measurements.**

The load test shows the healthy endpoint — answering in 2 ms — with an
end-to-end p95 of 13.7 seconds. Nothing was slow about it. All 30,000 deliveries
shared one Redis stream, and every `/slow` attempt occupied a worker for the
full timeout, so healthy deliveries queued behind unhealthy ones. One badly
behaved subscriber degrades everyone.

The circuit breaker limits the blast radius, and it is why the run completed at
all, but it only engages after 20 consecutive failures — and a *slow* endpoint
that eventually returns 200 never trips it while still consuming a worker slot
for seconds at a time.

The fix is stream sharding: partition by tenant, or by an endpoint health class,
with a dedicated worker pool per shard. Slow and failing endpoints get their own
lane; healthy traffic is unaffected. Kafka would give this via partitioning, and
it is the strongest argument for Kafka at scale.

In rough order after that:

1. **`delivery_attempts` growth.** The fastest-growing table, one row per HTTP
   attempt, with nothing pruning it. Partition by month with `pg_partman` and
   drop old partitions.
2. **No ingestion rate limit.** `POST /events` has no per-tenant throttle. One
   runaway producer can fill the database.
3. **Postgres connection count.** 25 per process. Past ~4 worker replicas on a
   small instance you need pgBouncer.
4. **The scheduler's poll.** One query per second per replica is fine at this
   scale and becomes a hot spot with a very large backlog. The partial index on
   `next_attempt_at` keeps it cheap for now.
5. **No observability endpoint.** No `/metrics`. You cannot operate what you
   cannot see, and this is the gap I would close first in practice.

### 10. What is wrong with this system today?

The security gap that matters most: **there is no SSRF protection.** A tenant
can register `http://169.254.169.254/latest/meta-data/` or
`http://10.0.0.5:6379/` as an endpoint and use HookRelay as an authenticated
request proxy into the network it runs in. URL validation only checks the
scheme and that a host is present. The real fix has to happen at
DNS-resolution time in the HTTP transport's `DialContext` — blocking private,
link-local and loopback ranges *after* resolution — because a public hostname
can resolve to a private address, so string-matching the URL is not enough. This
is the one item I would not ship to a real production without.

Also genuinely wrong or missing:

- **Signing secrets are stored in plaintext.** Necessary for the reveal-and-sign
  flow, but a database dump exposes every subscriber's secret. Envelope
  encryption with a KMS key is the fix.
- **No rate limiting** on any endpoint, including `/auth/login`, which makes
  password brute-forcing cheap.
- **JWT in `localStorage`** rather than an `httpOnly` cookie — XSS-exposed. The
  alternative required a server-side session layer that would have duplicated
  the authorisation surface, so it is a deliberate trade, not an oversight.
- **The stream is never trimmed.** `queue.Trim` exists and nothing calls it on a
  schedule. Acknowledged entries accumulate until Redis fills.
- **No `/metrics` endpoint.** See above.
- **Payload size is a global constant,** not a per-tenant quota.
- **No webhook payload encryption at rest.** Payloads sit in plaintext `JSONB`,
  which may not be acceptable for regulated data.

And things that are less wrong than they look:

- The `/slow` endpoint's 10,000 dead-lettered deliveries in the load test are
  correct behaviour, not a bug — the endpoint genuinely never answered in time.
- The single `/flaky` dead-letter is statistically expected
  (`0.3^8 × 10,000 ≈ 0.66`), not a defect.
- The 13.7 s end-to-end p95 is contention from the deliberately hostile test
  mix, not per-delivery overhead. With only healthy traffic it is milliseconds.
