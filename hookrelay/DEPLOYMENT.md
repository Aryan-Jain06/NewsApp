# DEPLOYMENT — what to do once the code exists

Everything below is free or open source. Where a hosted service has a paid tier
I say so explicitly and give a self-hosted alternative, so you never *have* to
pay for anything.

Contents:

1. [Run it locally in one command](#1-run-it-locally-in-one-command)
2. [What you must provide: Postgres and Redis](#2-what-you-must-provide-postgres-and-redis)
3. [Redis settings that actually matter for Streams](#3-redis-settings-that-actually-matter-for-streams)
4. [Environment variables and secrets](#4-environment-variables-and-secrets)
5. [Migrations](#5-migrations)
6. [Where to host it (free options)](#6-where-to-host-it-free-options)
7. [Continuous deployment — Argo CD and free equivalents](#7-continuous-deployment--argo-cd-and-free-equivalents)
8. [Scaling the workers](#8-scaling-the-workers)
9. [Monitoring and alerting](#9-monitoring-and-alerting)
10. [Backups and disaster recovery](#10-backups-and-disaster-recovery)
11. [Production hardening checklist](#11-production-hardening-checklist)
12. [Cost summary](#12-cost-summary)

---

## 1. Run it locally in one command

```bash
cp .env.example .env      # optional; the defaults work as-is
docker compose up --build
```

That starts six containers: `postgres`, `redis`, `api`, `worker`, `receiver`,
`frontend`. Then:

| What | Where |
|---|---|
| Dashboard | <http://localhost:3000> |
| API | <http://localhost:8080> |
| Test receiver | <http://localhost:9090> |

Nothing to install beyond Docker. Nothing to buy.

To verify the whole thing end to end:

```bash
# Compress the retry backoff first, so the dead-letter path finishes in seconds
# rather than the production 8-hour window.
RETRY_SCHEDULE=1s,1s,2s,2s,3s,3s,4s docker compose up -d worker
scripts/verify.sh
```

---

## 2. What you must provide: Postgres and Redis

HookRelay needs exactly two pieces of infrastructure. Both are open source and
both have genuinely free hosting options.

### PostgreSQL 14+

Postgres is the **source of truth**. Every event, delivery and attempt lives
here; Redis only carries pointers. Losing Redis costs you nothing permanent.
Losing Postgres loses data.

| Option | Free? | Notes |
|---|---|---|
| **Self-host** (docker compose, a VM, or Kubernetes) | Free forever | What `docker compose up` already does. You own backups. |
| **Neon** | Free tier: 0.5 GB, scales to zero | Serverless Postgres. Best free managed option. Cold starts add latency to the first request. |
| **Supabase** | Free tier: 500 MB, pauses after 7 days idle | You only need the Postgres, not the rest of the platform. |
| **Railway / Render Postgres** | Free trial credits, then paid | Fine for a demo, not a permanent free home. |
| **Aiven / ElephantSQL** | Small free tiers | Row/size limits are tight. |

Connection string goes in `DATABASE_URL`:

```
postgres://USER:PASSWORD@HOST:5432/hookrelay?sslmode=require
```

Use `sslmode=require` for anything not on localhost. If your provider gives a
pooled and a direct URL (Neon does), point `DATABASE_URL` at the **pooled**
endpoint for the API and the **direct** endpoint for migrations — pgBouncer in
transaction mode does not support the advisory locks golang-migrate takes.

**Sizing:** the pool is capped at 25 connections per process (`internal/db/db.go`).
With one API and one worker that is 50. Free tiers often cap at 20–100 total, so
if you run several worker replicas either lower `MaxConns` or put pgBouncer in
front.

### Redis 6.2+ (7.x recommended)

Redis carries the delivery queue as a **Stream** with a **consumer group**.
HookRelay uses `XADD`, `XREADGROUP`, `XACK` and `XAUTOCLAIM`.

| Option | Free? | Notes |
|---|---|---|
| **Self-host** (docker compose, VM, Kubernetes) | Free forever | Recommended. Redis is tiny and stateless-ish here. |
| **Upstash Redis** | Free tier: 256 MB, 500k commands/month | Serverless, pay-per-command. Supports Streams and consumer groups. Watch the command budget — a busy scheduler burns commands fast. |
| **Redis Cloud (Redis Ltd.)** | Free tier: 30 MB | 30 MB is enough for the stream if you trim it, but it is tight. |
| **Valkey** | Free forever | The BSD-licensed fork of Redis 7.2 after the licence change. Drop-in compatible; use it if the Redis licence matters to you. Change the compose image to `valkey/valkey:8-alpine` and nothing else. |

Connection string goes in `REDIS_URL`:

```
redis://:PASSWORD@HOST:6379/0
rediss://:PASSWORD@HOST:6379/0     # TLS — Upstash and Redis Cloud need this
```

> **A note on the command budget.** The scheduler polls Postgres every second
> and only touches Redis when something is due, so an idle system costs almost
> no Redis commands. Each delivery costs roughly one `XADD` + one `XREADGROUP`
> share + one `XACK`. Upstash's 500k free commands is therefore about 150k
> deliveries per month. Raise `SCHEDULER_INTERVAL` to `5s` if you need to
> stretch it further.

---

## 3. Redis settings that actually matter for Streams

These are the settings people get wrong, and the failure modes are quiet.

### Enable persistence

```
appendonly yes
appendfsync everysec
```

The compose file already does this. Without AOF, a Redis restart loses the
stream **and the consumer group's pending-entries list (PEL)**. HookRelay
survives that — the scheduler re-enqueues anything still `pending`/`failed` in
Postgres — but you take a delivery-latency hit while it catches up. With AOF the
restart is invisible.

### Never let Redis evict the stream

```
maxmemory-policy noeviction
```

This is the one that bites. If `maxmemory-policy` is any `allkeys-*` value,
Redis will happily evict your delivery stream to make room, and queued work
disappears. `noeviction` makes Redis reject writes instead, which surfaces as a
loud `XADD` error that HookRelay logs and recovers from via the scheduler.

Check what you have:

```bash
redis-cli config get maxmemory-policy
```

On Upstash and Redis Cloud, set the eviction policy to `noeviction` in the
console.

### Trim the stream

Acknowledged entries stay in the stream forever unless trimmed. HookRelay
exposes `queue.Trim`, but nothing calls it on a schedule yet — that is a
deliberate, documented gap. Add a cron or a small goroutine:

```bash
# Keep the most recent 1,000,000 entries; approximate trimming is cheap.
redis-cli XTRIM deliveries_stream MAXLEN '~' 1000000
```

Monitor with:

```bash
redis-cli XLEN deliveries_stream                        # total entries
redis-cli XPENDING deliveries_stream delivery_workers   # unacked
redis-cli XINFO GROUPS deliveries_stream                # lag per group
```

A steadily growing `XPENDING` count means workers are claiming work and dying
before acking — check worker logs and `REAPER_MIN_IDLE`.

### Do not put HookRelay's Redis behind a proxy that rewrites keys

Cluster-mode proxies that hash-tag keys can split the stream from the consumer
group. Single-node (or a cluster with the stream pinned to one slot) is what you
want. HookRelay does not need Redis Cluster at any realistic volume.

---

## 4. Environment variables and secrets

Full list, with the defaults from `internal/config/config.go`:

| Variable | Default | What it does |
|---|---|---|
| `DATABASE_URL` | `postgres://hookrelay:hookrelay@localhost:5432/hookrelay?sslmode=disable` | Postgres DSN. |
| `REDIS_URL` | `redis://localhost:6379/0` | Redis DSN. |
| `API_ADDR` | `:8080` | API listen address. |
| `JWT_SECRET` | `dev-only-change-me` | **Must be changed.** Signs dashboard JWTs. |
| `CORS_ALLOW_ORIGIN` | `*` | Set to your dashboard origin in production. |
| `WORKER_COUNT` | `8` | Concurrent delivery goroutines per worker process. |
| `DELIVERY_TIMEOUT` | `10s` | Per-attempt HTTP timeout. |
| `SCHEDULER_INTERVAL` | `1s` | How often due retries are re-enqueued. |
| `SCHEDULER_BATCH_SIZE` | `500` | Max deliveries promoted per tick. |
| `REAPER_INTERVAL` | `15s` | How often abandoned work is reclaimed. |
| `REAPER_MIN_IDLE` | `60s` | How long an entry must be idle before `XAUTOCLAIM` takes it. |
| `BREAKER_THRESHOLD` | `20` | Consecutive failures before an endpoint is paused. |
| `BREAKER_COOLDOWN` | `5m` | How long a tripped endpoint is skipped. |
| `DELIVERY_MAX_AGE` | `24h` | Absolute deadline; see the warning below. |
| `RETRY_SCHEDULE` | *(empty → 5s,30s,2m,10m,30m,2h,5h)* | Comma-separated backoff override. |
| `STREAM_NAME` | `deliveries_stream` | Redis stream key. |
| `CONSUMER_GROUP` | `delivery_workers` | Consumer group name. |
| `CONSUMER_NAME` | `<hostname>-<pid>` | Must be unique per worker process. |
| `LOG_LEVEL` | `info` | `debug` logs every attempt. |
| `NEXT_PUBLIC_API_URL` | `http://localhost:8080` | **Baked into the frontend at build time.** |

### Three that will bite you

**`JWT_SECRET`.** Generate a real one and never commit it:

```bash
openssl rand -base64 48
```

Rotating it invalidates every dashboard session, which is the point. It does
**not** affect API keys or webhook signing secrets.

**`NEXT_PUBLIC_API_URL`.** Next.js inlines `NEXT_PUBLIC_*` at *build* time, not
run time. Changing the env var on a running container does nothing — you must
rebuild the image. The compose file passes it as a build arg for this reason.

**`DELIVERY_MAX_AGE`.** Circuit-breaker skips deliberately do not consume a
delivery's retry budget, so without an absolute deadline a delivery queued for
an endpoint that stays down would be deferred forever. Keep this comfortably
larger than the sum of `RETRY_SCHEDULE` (≈8 h by default). If you shorten the
retry schedule, shorten this too.

### Where to keep secrets

- **docker compose:** a `.env` file, git-ignored (already in `.gitignore`).
- **Kubernetes:** a `Secret`, ideally managed by
  [External Secrets Operator](https://external-secrets.io/) or
  [Sealed Secrets](https://github.com/bitnami-labs/sealed-secrets) — both free
  and open source — so the encrypted form is safe to commit for GitOps.
- **SOPS + age** ([getsops.dev](https://getsops.io/)) is the simplest free
  option if you are not on Kubernetes: encrypted secrets committed to the repo,
  decrypted at deploy time.

Never commit a plaintext `JWT_SECRET`, `DATABASE_URL` with a password, or any
`whsec_` endpoint secret.

---

## 5. Migrations

Migrations are **embedded in the API binary** and run automatically on startup
(`cmd/api/main.go` calls `db.Migrate` before serving). golang-migrate takes a
Postgres advisory lock, so starting several API replicas at once is safe —
exactly one applies the migrations and the rest wait.

That means **you do not need a separate migration step or the golang-migrate
CLI** for a normal deploy. Deploy the API, it migrates itself.

Two cases where you want manual control:

**Large migrations on a live database.** An index build or column rewrite can
hold locks for minutes. Run it out of band first, then deploy:

```bash
go install github.com/golang-migrate/migrate/v4/cmd/migrate@latest
migrate -path backend/internal/db/migrations \
        -database "$DATABASE_URL" up
```

**Rollback.** Every migration has a `.down.sql`:

```bash
migrate -path backend/internal/db/migrations -database "$DATABASE_URL" down 1
```

Note that the *worker* does not migrate — it only connects. Always deploy the
API (or run migrations manually) before rolling out a worker that expects a new
column.

---

## 6. Where to host it (free options)

HookRelay is three deployables: **api**, **worker**, **frontend**. The worker is
the awkward one for free tiers, because it is a long-running background process
with no HTTP port, and many free platforms only host web services or sleep idle
containers.

### The pragmatic free choice: one small VM

A single VM running `docker compose up -d` hosts the whole system, including
Postgres and Redis, and costs nothing on:

- **Oracle Cloud Always Free** — 4 ARM cores / 24 GB RAM, genuinely free
  indefinitely. By far the most generous. This is what I would use.
- **Google Cloud free tier** — one `e2-micro`. Tight but workable.
- **AWS free tier** — `t4g.micro` free for 12 months, then paid.

Point a domain at it, terminate TLS with **Caddy** (free, automatic Let's
Encrypt) in front of the API and frontend.

### Platform-as-a-service

| Platform | Free? | Worker support |
|---|---|---|
| **Fly.io** | Small free allowance; card required | Good — `[processes]` in `fly.toml` runs api and worker from one image. Handles no-HTTP-port processes properly. |
| **Render** | Free web services (they sleep after 15 min idle) | Background workers are **paid only**. Free tier won't run the worker. |
| **Railway** | Trial credits, then paid | Works well, but not permanently free. |
| **Koyeb / Northflank** | Small free tiers | Both can run a worker. Limits are tight. |

A sleeping free web service is fine for the dashboard, and fatal for the worker
— deliveries stop while it sleeps.

### Frontend only

The dashboard is a static-ish Next.js app that talks to the API from the
browser, so it can go anywhere:

- **Vercel** — free Hobby tier, the natural home for Next.js.
- **Cloudflare Pages** / **Netlify** — free, generous.

Set `NEXT_PUBLIC_API_URL` to your public API URL **at build time**, and set the
API's `CORS_ALLOW_ORIGIN` to the dashboard's origin.

### Kubernetes

If you already have a cluster, HookRelay maps onto it cleanly:

- `api` → `Deployment` (2+ replicas) + `Service` + `Ingress`
- `worker` → `Deployment` (N replicas, no Service). Give each pod a unique
  `CONSUMER_NAME`; a `StatefulSet` gets you that for free via the pod ordinal,
  or use `valueFrom: fieldRef: metadata.name`.
- `frontend` → `Deployment` + `Service` + `Ingress`
- Postgres → [CloudNativePG](https://cloudnative-pg.io/) (free, excellent) or a
  managed instance.
- Redis → a single `StatefulSet` with a PVC, or
  [Valkey](https://valkey.io/) via its Helm chart.

Free local/edge clusters: **k3s**, **k0s**, **kind**, **minikube**.

Set the worker's `terminationGracePeriodSeconds` to at least
`DELIVERY_TIMEOUT + 10s` (so ≥ 20 s at defaults) so in-flight attempts finish
during a rolling update. The worker already drains on `SIGTERM`.

---

## 7. Continuous deployment — Argo CD and free equivalents

**Argo CD is itself free and open source** (Apache 2.0) — there is no paid
version to avoid. You only pay if you choose someone's hosted control plane
(Akuity, Codefresh). Self-hosting it on your own cluster costs nothing.

```bash
kubectl create namespace argocd
kubectl apply -n argocd \
  -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
```

So if you want Argo CD, just run Argo CD. Here is how the alternatives compare,
in case you do not have a cluster:

| Tool | Licence | Needs Kubernetes? | Good fit when |
|---|---|---|---|
| **Argo CD** | Apache 2.0, free | Yes | You have a cluster and want a UI showing drift between git and live state. |
| **Flux CD** | Apache 2.0, free | Yes | You prefer no UI, pure controllers, and tight Git/OCI integration. Lighter than Argo. |
| **Kamal** | MIT, free | **No** | Deploying Docker containers to plain VMs over SSH, with zero-downtime swaps. The best Argo-substitute for a single-VM HookRelay. |
| **Coolify** | Apache 2.0, free self-hosted | No | You want a Heroku/Vercel-style UI on your own VM. Handles Postgres, Redis, and background workers. Very good fit here. |
| **Dokploy** | MIT, free | No | Same idea as Coolify, lighter. |
| **Dokku** | MIT, free | No | Mature single-host PaaS; `git push` deploys. |
| **CapRover** | MIT, free | No | Docker Swarm based, web UI. |
| **GitHub Actions** | Free for public repos; 2,000 min/month private | No | Plain push-based CD. Simplest possible thing that works. |
| **Woodpecker CI / Drone** | Free, self-hosted | No | Self-hosted CI if you would rather not use GitHub's runners. |

### Recommended free setup for this project

**Single VM (Oracle Always Free) + Coolify or Kamal.** You get Postgres, Redis,
api, worker and frontend on one box, GitOps-style deploys, TLS, and no bill.

**If you already run Kubernetes: Argo CD + CloudNativePG.** Also free.

### Minimal GitHub Actions CD

For a single-VM deployment, this is genuinely all you need:

```yaml
name: deploy
on:
  push:
    branches: [main]

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      # Verify before shipping.
      - uses: actions/setup-go@v5
        with: { go-version: '1.25' }
      - run: go test ./... && go vet ./...
        working-directory: backend

      - name: Deploy over SSH
        uses: appleboy/ssh-action@v1
        with:
          host: ${{ secrets.DEPLOY_HOST }}
          username: ${{ secrets.DEPLOY_USER }}
          key: ${{ secrets.DEPLOY_SSH_KEY }}
          script: |
            cd /srv/hookrelay
            git pull --ff-only
            docker compose pull
            docker compose up -d --build
            docker image prune -f
```

A CI workflow already ships at `.github/workflows/ci.yml`. On every push and
PR it builds and vets all three Go modules, enforces `gofmt`, runs the unit
tests under `-race`, typechecks and builds the frontend, validates
`docker compose config`, and stands up Postgres + Redis as service containers to
run `scripts/verify.sh` and `scripts/chaos.sh` for real.

### Deployment order that avoids a broken window

1. Run migrations (or deploy the API, which migrates itself).
2. Roll the workers.
3. Roll the frontend.

Because migrations are additive and the API is backward compatible with the
previous worker, any order *works* — but this one never has a worker querying a
column that does not exist yet.

---

## 8. Scaling the workers

Delivery throughput is `worker_replicas × WORKER_COUNT / mean_attempt_seconds`.
With `WORKER_COUNT=50` and a 100 ms endpoint, one process handles ~500
deliveries/sec.

**Scale horizontally by adding replicas.** The Redis consumer group distributes
entries across consumers automatically; no coordination or sharding needed.
Every replica must have a **unique `CONSUMER_NAME`** — two consumers sharing a
name share a PEL, and `XAUTOCLAIM` will fight over entries. The default
(`hostname-pid`) is unique in containers.

What to watch as you scale:

- **Postgres connections.** Each replica opens up to 25. Add pgBouncer past
  ~4 replicas on a small instance.
- **`XPENDING` depth.** Rising = workers cannot keep up, or are dying.
- **The scheduler.** `SCHEDULER_BATCH_SIZE` × (1 s / `SCHEDULER_INTERVAL`) caps
  how many retries per second can be re-enqueued. The default 500/s is plenty
  for normal traffic; raise it if a large backlog drains slowly. Running the
  scheduler in every replica is safe — `FOR UPDATE SKIP LOCKED` means no row is
  handed out twice.

Running more replicas than you need is harmless; idle workers block on
`XREADGROUP` and cost nothing.

---

## 9. Monitoring and alerting

There is **no `/metrics` endpoint yet** — that is the biggest honest gap in the
project. Until there is, the API's own stats endpoints are the source of truth:

```bash
curl -H "Authorization: Bearer $KEY" localhost:8080/stats/overview
curl -H "Authorization: Bearer $KEY" 'localhost:8080/stats/timeseries?window_hours=1&bucket_seconds=60'
curl -H "Authorization: Bearer $KEY" 'localhost:8080/deliveries?status=dead&limit=1'
```

### The four things worth alerting on

| Signal | Query | Why it matters |
|---|---|---|
| **Dead-letter queue growing** | `counts.dead` from `/deliveries?limit=1` | Deliveries are being permanently abandoned. Page someone. |
| **Deliveries stuck open** | `pending + failed + delivering` not falling | The worker is down, or the scheduler is not keeping up. |
| **`XPENDING` rising** | `redis-cli XPENDING deliveries_stream delivery_workers` | Workers claim and die without acking. |
| **A specific endpoint's breaker open** | `circuit_opened_until` on `GET /endpoints/{id}` | One subscriber is down; tell *them*, not yourself. |

### Free stack to put this on a graph

- **Prometheus + Grafana** — both free and self-hostable. Add
  `promhttp.Handler()` on an internal port in `cmd/api` and `cmd/worker` and
  export: deliveries by status, attempt count by outcome, attempt latency
  histogram, stream depth, PEL depth.
- **Grafana Cloud** free tier — 10k series, 14-day retention. Enough for this.
- **Postgres exporter** and **Redis exporter** (both free) cover the
  infrastructure half without any code change, and are the fastest way to get
  useful dashboards today.
- **Uptime Kuma** (free, self-hosted) or **Better Stack** free tier for a simple
  `/readyz` check plus alerting.

`/healthz` is liveness (process up). `/readyz` checks Postgres and Redis and
returns 503 if either is down — point your load balancer at `/readyz`.

Logs are structured JSON on stdout (`log/slog`), so anything that reads
container logs works: **Loki** (free), **Vector** (free), or `docker compose
logs`.

---

## 10. Backups and disaster recovery

**Postgres is the only thing you must back up.** Redis holds pointers; the
scheduler rebuilds the queue from Postgres.

```bash
# Nightly logical backup.
pg_dump --format=custom "$DATABASE_URL" > hookrelay-$(date +%F).dump

# Restore.
pg_restore --clean --if-exists --dbname "$DATABASE_URL" hookrelay-2026-01-01.dump
```

Managed providers (Neon, Supabase) include point-in-time recovery on the free
tier — use it and verify a restore once.

Self-hosting? **pgBackRest** or **WAL-G** (both free) do incremental WAL
archiving to any S3-compatible store. Free object storage: Cloudflare R2 has a
10 GB free tier.

**Test your restore.** An untested backup is a rumour.

### What happens if Redis dies entirely

Nothing is lost. On restart:

1. Deliveries still `pending` or `failed` in Postgres have a `next_attempt_at`.
2. The scheduler sees them as due and re-enqueues them.
3. `EnsureGroup` recreates the stream and consumer group via `MKSTREAM`.

You lose delivery *latency*, not delivery. This is the whole reason the stream
carries only delivery IDs and never the payload.

### Data growth

`delivery_attempts` grows fastest — one row per HTTP attempt. Nothing prunes it
yet. Add a retention job before it hurts:

```sql
-- Keep 30 days of attempt history for settled deliveries.
DELETE FROM delivery_attempts
WHERE attempted_at < now() - interval '30 days'
  AND delivery_id IN (
    SELECT id FROM deliveries
    WHERE status IN ('succeeded', 'dead')
      AND completed_at < now() - interval '30 days'
  );
```

Run it from cron, or use **pg_partman** (free) to partition by month and drop
old partitions instantly.

---

## 11. Production hardening checklist

Things this codebase does **not** do yet, in rough priority order. Each is a
real gap, not a nitpick.

- [ ] **`JWT_SECRET` set to a random value.** Non-negotiable.
- [ ] **`CORS_ALLOW_ORIGIN` set to your dashboard origin**, not `*`.
- [ ] **TLS everywhere.** Terminate at Caddy, an ingress, or the platform. The
      API speaks plain HTTP by design and expects something in front.
- [ ] **Rate limit ingestion.** `POST /events` has no per-tenant rate limit. A
      runaway producer can fill your database. Add a token bucket keyed by
      tenant (`golang.org/x/time/rate`, free) or do it at the ingress.
- [ ] **SSRF protection on endpoint URLs.** A tenant can currently register
      `http://169.254.169.254/` or `http://10.0.0.5:6379/` and use HookRelay as
      a request proxy into your own network. Before production, block private
      and link-local ranges at DNS-resolution time in the deliverer's HTTP
      transport (`DialContext`), not just by parsing the URL string — DNS can
      resolve a public name to a private IP.
- [ ] **Payload size and retention limits per tenant.** `MaxPayloadBytes` is a
      global 256 KiB; there is no per-tenant quota.
- [ ] **`/metrics` endpoint.** See §9.
- [ ] **Stream trimming on a schedule.** See §3.
- [ ] **Attempt-history retention.** See §10.
- [ ] **Structured audit log** for secret rotation and endpoint deletion.
- [ ] **Signing-secret encryption at rest.** Endpoint secrets are stored in
      plaintext so they can be revealed in the dashboard and used for signing.
      Consider envelope encryption with a KMS key if your threat model includes
      database-dump exposure.
- [ ] **Per-tenant worker fairness.** One tenant with a million queued
      deliveries currently starves everyone else, because there is a single
      global stream. Sharding the stream per tenant tier is the fix.

---

## 12. Cost summary

| Component | Free option | Cost |
|---|---|---|
| Compute (api + worker + frontend) | Oracle Cloud Always Free VM | **$0** |
| Postgres | Self-hosted on that VM, or Neon free tier | **$0** |
| Redis | Self-hosted on that VM, or Upstash free tier | **$0** |
| CD | Argo CD / Flux / Kamal / Coolify — all open source | **$0** |
| CI | GitHub Actions (free for public repos) | **$0** |
| Monitoring | Prometheus + Grafana self-hosted, or Grafana Cloud free | **$0** |
| Object storage for backups | Cloudflare R2 free tier (10 GB) | **$0** |
| TLS certificates | Let's Encrypt via Caddy | **$0** |
| Frontend hosting | Vercel Hobby / Cloudflare Pages | **$0** |
| Domain name | — | **~$10/year**, and only if you want a custom domain |

**Total: $0**, or about $10/year if you want a domain. Nothing in this project
requires a paid service.
