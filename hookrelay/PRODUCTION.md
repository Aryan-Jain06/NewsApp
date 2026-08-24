# PRODUCTION — making HookRelay production-ready, for free

A concrete, ordered checklist. Every item says **what to do**, **why it matters**,
and where code is needed, **the code**. Nothing here costs money.

Work top to bottom. The blockers genuinely block; the rest can ship after you go
live.

- [Cost: zero](#cost-zero)
- [Stage 0 — Blockers (do not go live without these)](#stage-0--blockers-do-not-go-live-without-these)
- [Stage 1 — Infrastructure](#stage-1--infrastructure)
- [Stage 2 — Don't get taken down](#stage-2--dont-get-taken-down)
- [Stage 3 — See what's happening](#stage-3--see-whats-happening)
- [Stage 4 — Don't run out of disk](#stage-4--dont-run-out-of-disk)
- [Stage 5 — Survive losing the machine](#stage-5--survive-losing-the-machine)
- [Stage 6 — Nice to have](#stage-6--nice-to-have)
- [Final pre-launch checklist](#final-pre-launch-checklist)

---

## Cost: zero

| Need | Free option | Limit that actually bites |
|---|---|---|
| Server | **Oracle Cloud Always Free** — 4 ARM cores, 24 GB RAM, 200 GB disk | None. Free indefinitely, no card expiry trick. This is the one to use. |
| Postgres | Self-host on that server, or **Neon** free tier | Neon: 0.5 GB storage, scales to zero (cold starts). |
| Redis | Self-host on that server, or **Upstash** free tier | Upstash: 500k commands/month ≈ 150k deliveries. |
| TLS | **Caddy** + Let's Encrypt | None. Fully automatic. |
| CI/CD | **GitHub Actions** (free for public repos) | 2,000 min/month on private repos. |
| Deploy tooling | **Coolify** or **Kamal** (both open source) | None. |
| Metrics/dashboards | **Prometheus + Grafana** self-hosted, or **Grafana Cloud** free | Grafana Cloud: 10k series, 14-day retention. |
| Uptime alerts | **Uptime Kuma** self-hosted | None. |
| Backup storage | **Cloudflare R2** free tier | 10 GB. Plenty for `pg_dump`s. |
| Error tracking | **GlitchTip** self-hosted (Sentry-compatible) | None. |

**Total: $0.** A domain is ~$10/year and optional — you can run on the IP.

> Everything fits on the single Oracle free VM, including Postgres and Redis.
> One machine, `docker compose up -d`, done.

---

## Stage 0 — Blockers (do not go live without these)

### 0.1 Block SSRF on endpoint URLs 🔴

**Why:** Right now a tenant can register `http://169.254.169.254/latest/meta-data/`
(cloud credentials), `http://127.0.0.1:6379/` (your Redis), or
`http://10.0.0.5:5432/` and use HookRelay as an authenticated proxy into your own
network. HookRelay will dutifully sign and send requests there for them. **This is
the one item I would refuse to ship without.**

**Why URL validation is not enough:** checking the hostname string fails, because
`evil.example.com` can have a DNS A record pointing at `169.254.169.254`. The
check must happen *after* DNS resolution, on the actual IP being dialled.

Create `backend/internal/workers/safedial.go`:

```go
package workers

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"
)

// blockedNets are ranges a webhook must never be able to reach: loopback,
// link-local (cloud metadata lives at 169.254.169.254), and RFC1918 private
// space. A tenant-supplied URL must not become a proxy into our own network.
var blockedNets = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"::1/128", "fc00::/7", "fe80::/10", "::/128",
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

func ipBlocked(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// safeDialer refuses to connect to internal addresses. The check runs in
// Control, which fires after DNS resolution with the concrete address being
// dialled — so a public hostname that resolves to a private IP is still caught,
// and there is no resolve-then-connect race to exploit.
func safeDialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("parse dial address %q: %w", address, err)
			}
			ip := net.ParseIP(host)
			if ipBlocked(ip) {
				return fmt.Errorf("refusing to deliver to internal address %s", ip)
			}
			return nil
		},
	}
}
```

Then in `NewDeliverer` (`backend/internal/workers/deliverer.go`), replace the
`Transport` with:

```go
Transport: &http.Transport{
	DialContext:         safeDialer(5 * time.Second).DialContext,
	MaxIdleConns:        512,
	MaxIdleConnsPerHost: 32,
	IdleConnTimeout:     90 * time.Second,
},
```

**Keep an escape hatch for local development**, or `docker compose` breaks (the
test receiver is on a private address). Gate it on an env var — default to *safe*:

```go
// ALLOW_PRIVATE_ENDPOINTS=true is for local development only, where the test
// receiver is on a private address. Never set it in production.
if os.Getenv("ALLOW_PRIVATE_ENDPOINTS") != "true" {
	transport.DialContext = safeDialer(5 * time.Second).DialContext
}
```

Add `ALLOW_PRIVATE_ENDPOINTS=true` to the `worker` service in
`docker-compose.yml`, and never to production.

**This code is tested, not sketched.** It was compiled and run against 12
addresses — `169.254.169.254`, `127.0.0.1`, `10.0.0.5`, `172.16.0.1`,
`192.168.1.1`, `::1`, `fd00::1`, `0.0.0.0`, `100.64.0.1` all blocked;
`1.1.1.1`, `93.184.216.34`, `2606:4700::1111` all allowed — plus a real
`http.Client` dial to the metadata address, which was refused with
`refusing to deliver to internal address 169.254.169.254`.

**Verify it in your own deployment:**

```bash
# Should end up dead with "refusing to deliver to internal address"
curl -X POST $API/endpoints -H "Authorization: Bearer $KEY" \
  -d '{"url":"http://169.254.169.254/","description":"ssrf","event_types":["ssrf.test"]}'
curl -X POST $API/events -H "Authorization: Bearer $KEY" \
  -d '{"event_type":"ssrf.test","payload":{}}'
```

Add a unit test for `ipBlocked` while you are there — it is a pure function and
the table above is the test case.

### 0.2 Set a real `JWT_SECRET` 🔴

**Why:** the default is `dev-only-change-me`, in a public repo. Anyone can mint a
token for any tenant.

```bash
openssl rand -base64 48
```

Put it in `.env` (already git-ignored). Rotating it logs everyone out of the
dashboard, which is the point. It does **not** affect API keys or webhook signing
secrets.

### 0.3 Lock down CORS 🔴

**Why:** `CORS_ALLOW_ORIGIN=*` lets any website make authenticated dashboard
calls from a victim's browser.

```bash
CORS_ALLOW_ORIGIN=https://hookrelay.yourdomain.com
```

### 0.4 Terminate TLS 🔴

**Why:** API keys and signing secrets travel in headers and bodies. Plain HTTP
publishes them to every hop. HookRelay speaks HTTP on purpose and expects a proxy
in front.

`Caddyfile` — this is the whole thing, certificates included:

```
api.yourdomain.com {
	reverse_proxy localhost:8080
}

hookrelay.yourdomain.com {
	reverse_proxy localhost:3000
}
```

```bash
docker run -d --name caddy --network host -v $PWD/Caddyfile:/etc/caddy/Caddyfile \
  -v caddy_data:/data caddy:2-alpine
```

Caddy gets and renews Let's Encrypt certificates automatically. Free, no config.

Then bind the app ports to localhost only, so nothing bypasses Caddy. In
`docker-compose.yml`:

```yaml
ports:
  - "127.0.0.1:8080:8080"   # was "8080:8080"
```

Do the same for `3000`, and **delete the `ports:` block entirely** from
`postgres`, `redis` and `receiver` — they only need to be reachable inside the
compose network, and the `receiver` should not be deployed to production at all.

### 0.5 Remove the test receiver from production 🔴

**Why:** `/receiver` is a deliberately broken service with an unauthenticated
`/_control` endpoint. It exists to test HookRelay. It has no business in
production.

Create a `docker-compose.prod.yml` overlay:

```yaml
services:
  receiver:
    deploy:
      replicas: 0
```

Deploy with `docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d`,
or simply delete the `receiver` service from your production compose file.

---

## Stage 1 — Infrastructure

### 1.1 Provision the server

Oracle Cloud → **Always Free** → Ampere A1 instance, 4 OCPU / 24 GB RAM, Ubuntu
22.04. Open ports 80 and 443 only in the security list.

```bash
sudo apt update && sudo apt install -y docker.io docker-compose-v2 git
sudo usermod -aG docker $USER   # log out and back in
```

### 1.2 Decide: self-host or managed Postgres and Redis

**Self-hosted (recommended on the free VM)** — already what `docker compose` does.
You own backups (Stage 5).

**Managed** — Neon for Postgres, Upstash for Redis. Two things to get right:

- **Neon:** use the **direct** (non-pooled) URL for migrations. pgBouncer in
  transaction mode does not support the advisory locks golang-migrate takes, so
  migrations hang on the pooled URL.
- **Upstash:** use the `rediss://` (TLS) URL, and check the command budget —
  roughly three commands per delivery, so 500k/month ≈ 150k deliveries. Raise
  `SCHEDULER_INTERVAL` to `5s` to stretch it.

### 1.3 Set the Redis config that actually matters

```
appendonly yes
appendfsync everysec
maxmemory-policy noeviction
```

The compose file already sets `appendonly`. **`noeviction` is the one that bites.**
If the policy is any `allkeys-*` value, Redis will evict your delivery stream to
free memory and queued work silently disappears. `noeviction` makes Redis reject
writes instead — which surfaces as a loud error HookRelay logs and recovers from
via the scheduler.

```bash
redis-cli config get maxmemory-policy   # must be "noeviction"
```

On Upstash and Redis Cloud, set it in the console.

Update the compose command:

```yaml
command: ["redis-server", "--appendonly", "yes", "--maxmemory-policy", "noeviction"]
```

### 1.4 Set the worker for your load

```bash
WORKER_COUNT=50            # concurrent deliveries per worker process
DELIVERY_TIMEOUT=10s
BREAKER_THRESHOLD=20
BREAKER_COOLDOWN=5m
DELIVERY_MAX_AGE=24h       # must stay above the ~8h retry window
```

Leave `RETRY_SCHEDULE` empty in production so the real 8-hour backoff applies.
Only compress it for tests.

**Watch Postgres connections.** Each process opens up to 25. One API + one worker
= 50. Free-tier Postgres often caps at 20–100, so past ~3 worker replicas either
lower `MaxConns` in `internal/db/db.go` or put pgBouncer in front.

### 1.5 Deploy on every push

Add the SSH deploy job from
[DEPLOYMENT.md §7](DEPLOYMENT.md#7-continuous-deployment--argo-cd-and-free-equivalents),
or install **Coolify** (open source, free, web UI, handles Postgres/Redis/workers
and TLS) on the same VM. Both are free.

Migrations run automatically when the API starts, and golang-migrate takes an
advisory lock, so several replicas starting at once is safe. Deploy order that is
never wrong: **API first** (it migrates), then workers, then frontend.

---

## Stage 2 — Don't get taken down

### 2.1 Rate-limit ingestion 🟠

**Why:** `POST /events` has no per-tenant limit. One buggy loop in a customer's
code fills your database and your queue.

Create `backend/internal/handlers/ratelimit.go`:

```go
package handlers

import (
	"net/http"
	"sync"
	"time"

	"github.com/aryan-jain06/hookrelay/backend/internal/httpx"
	"golang.org/x/time/rate"
)

// tenantLimiter holds one token bucket per tenant. Keeping it in process memory
// means each replica enforces the limit independently — with N replicas the
// effective limit is N times higher. That is an acceptable approximation for
// abuse protection; move the counter to Redis if you need an exact global limit.
type tenantLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*rate.Limiter
	lastSeen map[string]time.Time
	perSec   rate.Limit
	burst    int
}

func newTenantLimiter(perSec float64, burst int) *tenantLimiter {
	l := &tenantLimiter{
		buckets:  map[string]*rate.Limiter{},
		lastSeen: map[string]time.Time{},
		perSec:   rate.Limit(perSec),
		burst:    burst,
	}
	go l.evictLoop()
	return l
}

func (l *tenantLimiter) get(key string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.buckets[key]
	if !ok {
		lim = rate.NewLimiter(l.perSec, l.burst)
		l.buckets[key] = lim
	}
	l.lastSeen[key] = time.Now()
	return lim
}

// evictLoop drops idle buckets so the map cannot grow without bound.
func (l *tenantLimiter) evictLoop() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-30 * time.Minute)
		l.mu.Lock()
		for k, seen := range l.lastSeen {
			if seen.Before(cutoff) {
				delete(l.buckets, k)
				delete(l.lastSeen, k)
			}
		}
		l.mu.Unlock()
	}
}

// RateLimitPerTenant throttles authenticated requests per tenant. Mount it
// inside the authenticated group so a tenant is already resolved.
func RateLimitPerTenant(perSec float64, burst int) func(http.Handler) http.Handler {
	limiter := newTenantLimiter(perSec, burst)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant := TenantFrom(r.Context())
			if tenant == nil {
				next.ServeHTTP(w, r)
				return
			}
			if !limiter.get(tenant.ID.String()).Allow() {
				w.Header().Set("Retry-After", "1")
				httpx.Error(w, r, httpx.Errorf(http.StatusTooManyRequests,
					"rate_limited", "too many requests for this tenant; slow down"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

Wire it in `backend/internal/handlers/router.go`, inside the authenticated group:

```go
r.Group(func(r chi.Router) {
	r.Use(d.AuthMW)
	r.Use(RateLimitPerTenant(200, 400))   // 200 req/s sustained, 400 burst
	// ...
})
```

```bash
cd backend && go get golang.org/x/time && go mod tidy
```

### 2.2 Rate-limit login 🟠

**Why:** `/auth/login` is unauthenticated and bcrypt-backed. Unlimited attempts
make password guessing free.

Limit by IP in front of the API — cheapest correct option, no code:

```
api.yourdomain.com {
	@login path /auth/login /auth/register
	rate_limit @login {
		zone login {
			key    {remote_host}
			events 10
			window 1m
		}
	}
	reverse_proxy localhost:8080
}
```

(Needs the `caddy-ratelimit` module — free. Or use `nginx limit_req`.)

### 2.3 Cap payload size per tenant 🟡

`MaxPayloadBytes` is a global 256 KiB constant in
`backend/internal/services/ingest.go`. If you need per-tenant quotas, add a
`max_payload_bytes` column to `tenants` and check it in `Publish`. Not urgent
unless you have untrusted tenants.

---

## Stage 3 — See what's happening

### 3.1 Add a `/metrics` endpoint 🟠

**Why:** there is no metrics endpoint today. You cannot operate what you cannot
see, and this is the gap I would close first after the blockers.

```bash
cd backend && go get github.com/prometheus/client_golang && go mod tidy
```

Create `backend/internal/metrics/metrics.go`:

```go
// Package metrics holds the Prometheus collectors shared by the API and worker.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Attempts is the one to alert on: a rising failure or skipped rate means
	// subscribers are breaking.
	Attempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hookrelay_delivery_attempts_total",
		Help: "Delivery attempts by outcome (success, failure, skipped).",
	}, []string{"outcome"})

	AttemptDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "hookrelay_attempt_duration_seconds",
		Help:    "How long the subscriber took to answer.",
		Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"outcome"})

	DeliveriesSettled = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hookrelay_deliveries_settled_total",
		Help: "Deliveries reaching a terminal state (succeeded, dead).",
	}, []string{"status"})

	// StreamDepth and PendingEntries are gauges refreshed by a background loop.
	StreamDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hookrelay_stream_depth",
		Help: "Entries in the Redis delivery stream.",
	})
	PendingEntries = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "hookrelay_stream_pending_entries",
		Help: "Unacknowledged entries in the consumer group (rising = workers dying).",
	})
	Reclaimed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hookrelay_reclaimed_total",
		Help: "Work items recovered by the reaper, by path.",
	}, []string{"path"})
)
```

Expose it on a **separate internal port** so it is not public. In both
`cmd/api/main.go` and `cmd/worker/main.go`:

```go
// Metrics listen on their own port so they are never exposed through the public
// ingress alongside the API.
go func() {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: ":9100", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("metrics server", "error", err)
	}
}()
```

Then increment them where outcomes are decided — `recordSuccess`,
`recordFailure`, `skip` and `expire` in `deliverer.go`, and the two reclaim paths
in `pool.go`.

### 3.2 Scrape it

`prometheus.yml`:

```yaml
global:
  scrape_interval: 15s
scrape_configs:
  - job_name: hookrelay
    static_configs:
      - targets: ['api:9100', 'worker:9100']
```

Add Prometheus + Grafana to compose (both free), or ship to **Grafana Cloud**'s
free tier (10k series) with Grafana Agent if you would rather not run them.

Also free and zero-code: **postgres_exporter** and **redis_exporter** give you
useful infrastructure dashboards immediately.

### 3.3 Alert on the four things that matter 🟠

| Alert | Condition | Meaning |
|---|---|---|
| **Dead letters growing** | `increase(hookrelay_deliveries_settled_total{status="dead"}[15m]) > 0` | Deliveries are being permanently abandoned. Page someone. |
| **Queue not draining** | `hookrelay_stream_depth` rising for 10m | Worker is down or cannot keep up. |
| **Workers dying** | `hookrelay_stream_pending_entries > 100` for 5m | Workers claim work and die before acknowledging. |
| **Readiness failing** | `/readyz` non-200 for 2m | Postgres or Redis is unreachable. |

Without Prometheus, the API's own endpoints work fine for a cron-based check:

```bash
curl -sH "Authorization: Bearer $KEY" $API/deliveries?limit=1 | jq .counts
```

Point **Uptime Kuma** (free, self-hosted) at `/readyz` for basic alerting in
about five minutes.

### 3.4 Ship the logs somewhere

Logs are already structured JSON on stdout via `log/slog`, so anything works:
**Loki** + Grafana (free), **Vector** (free), or `docker compose logs`. Set
`LOG_LEVEL=info` in production — `debug` logs every attempt.

---

## Stage 4 — Don't run out of disk

Both of these are unbounded growth today. Neither is urgent on day one; both
become outages eventually.

### 4.1 Trim the Redis stream 🟠

**Why:** acknowledged entries stay in the stream forever. `queue.Trim` exists and
nothing calls it on a schedule. Left alone, Redis fills up and — with
`noeviction` set correctly — starts rejecting writes.

Simplest fix, a cron on the host:

```cron
*/30 * * * * docker compose -f /srv/hookrelay/docker-compose.yml exec -T redis \
  redis-cli XTRIM deliveries_stream MAXLEN '~' 1000000
```

Better, in `pool.go` — add a fourth background loop alongside the reaper:

```go
// trim keeps acknowledged history from growing without bound. Approximate
// trimming is much cheaper than exact and the precision does not matter here.
func (p *Pool) trim(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := p.queue.Trim(ctx, 1_000_000); err != nil {
			slog.Error("stream trim failed", "error", err)
		}
	}
}
```

Monitor with `redis-cli XLEN deliveries_stream`.

### 4.2 Prune attempt history 🟠

**Why:** `delivery_attempts` grows by one row per HTTP attempt and nothing deletes
them. It is the fastest-growing table by a wide margin.

Nightly cron:

```sql
DELETE FROM delivery_attempts
WHERE attempted_at < now() - interval '30 days'
  AND delivery_id IN (
    SELECT id FROM deliveries
    WHERE status IN ('succeeded', 'dead')
      AND completed_at < now() - interval '30 days'
  );
```

Only settled deliveries are touched, so nothing in flight loses its history.

At real volume, use **pg_partman** (free) to partition by month and drop old
partitions instantly instead of paying for a big `DELETE`.

Also consider dropping succeeded deliveries and their events after 90 days;
dead ones are the ones worth keeping.

---

## Stage 5 — Survive losing the machine

**Only Postgres needs backing up.** Redis holds work pointers; the scheduler
rebuilds the queue from Postgres. Even a total Redis wipe costs latency, not data.

### 5.1 Nightly backups to free object storage

```bash
#!/usr/bin/env bash
# /srv/hookrelay/backup.sh — run nightly from cron
set -euo pipefail
STAMP=$(date +%F-%H%M)
OUT="/tmp/hookrelay-$STAMP.dump"

pg_dump --format=custom --no-owner "$DATABASE_URL" > "$OUT"

# Cloudflare R2 is S3-compatible with a 10 GB free tier.
aws s3 cp "$OUT" "s3://$R2_BUCKET/hookrelay/" \
  --endpoint-url "https://$R2_ACCOUNT.r2.cloudflarestorage.com"

rm -f "$OUT"
# Keep 30 days locally as a fast-restore path.
find /srv/hookrelay/backups -name '*.dump' -mtime +30 -delete
```

```cron
0 3 * * * /srv/hookrelay/backup.sh >> /var/log/hookrelay-backup.log 2>&1
```

On Neon or Supabase, point-in-time recovery is included on the free tier — use it
instead.

For continuous WAL archiving, **pgBackRest** or **WAL-G** (both free) push
incremental WAL to R2.

### 5.2 Actually test a restore 🟠

An untested backup is a rumour. Once, on a throwaway database:

```bash
createdb hookrelay_restore_test
pg_restore --dbname hookrelay_restore_test --no-owner hookrelay-2026-01-01.dump
psql hookrelay_restore_test -c "SELECT count(*) FROM deliveries;"
dropdb hookrelay_restore_test
```

Put a calendar reminder to repeat it quarterly.

---

## Stage 6 — Nice to have

Real improvements, none of them blocking.

### 6.1 Encrypt signing secrets at rest 🟡

Endpoint `whsec_` secrets are stored in plaintext, because they must be revealed
in the dashboard and used for signing. A database dump therefore exposes every
subscriber's secret. Envelope-encrypt them with a KMS key (or `age` with a
key held only in the environment) and decrypt on use. Worth doing if your threat
model includes database exposure.

### 6.2 Move the JWT to an httpOnly cookie 🟡

The dashboard keeps its JWT in `localStorage`, which XSS can read. An `httpOnly`
cookie is safer but needs a same-origin server to set it — which means adding a
Next.js server layer, which means a second place authorisation is decided. Given
the trade, `localStorage` is deliberate rather than accidental. If you add the
cookie, also add CSRF protection.

### 6.3 Shard the stream per tenant 🟡

**The real scaling limit.** The load test measured a 13.7 s end-to-end p95 for an
endpoint answering in 2 ms — pure head-of-line blocking, because all 30,000
deliveries shared one stream and every slow endpoint attempt held a worker for
the full timeout.

The circuit breaker limits the damage but only engages after 20 consecutive
failures, and a *slow-but-eventually-200* endpoint never trips it at all while
still consuming a worker slot for seconds.

Fix: one stream per tenant (or per health class), with its own worker pool. Slow
subscribers get their own lane. Do this when a noisy tenant first hurts a quiet
one — not before.

### 6.4 Server-sent events on the event detail page 🟢

Polling every 5 s is fine, but SSE would make the timeline feel live. The
obvious next frontend improvement.

### 6.5 Audit log 🟢

Log secret rotations, endpoint deletions and replays with actor and timestamp.
Cheap to add, valuable the first time someone asks "who rotated this?".

### 6.6 Encrypt payloads at rest 🟢

Payloads sit in plaintext `JSONB`. If you handle regulated data, encrypt the
payload column.

---

## Final pre-launch checklist

Copy this into your issue tracker.

**Blockers**

- [ ] SSRF dialler installed; `ALLOW_PRIVATE_ENDPOINTS` **not** set in production
- [ ] Verified: an endpoint pointing at `169.254.169.254` fails to deliver
- [ ] `JWT_SECRET` generated with `openssl rand -base64 48`
- [ ] `CORS_ALLOW_ORIGIN` set to the dashboard origin, not `*`
- [ ] TLS terminating in front of both API and dashboard
- [ ] App ports bound to `127.0.0.1`; no `ports:` on postgres/redis
- [ ] `receiver` service removed from the production compose file

**Infrastructure**

- [ ] `redis-cli config get maxmemory-policy` returns `noeviction`
- [ ] `appendonly yes` confirmed
- [ ] `RETRY_SCHEDULE` unset (production 8-hour backoff)
- [ ] `DELIVERY_MAX_AGE` comfortably above the retry window
- [ ] `LOG_LEVEL=info`
- [ ] Postgres connection count under the provider's cap
- [ ] Migrations verified applied (`\dt` shows 7 tables)
- [ ] `/readyz` returns 200

**Abuse protection**

- [ ] Per-tenant rate limit on the authenticated group
- [ ] IP rate limit on `/auth/login` and `/auth/register`

**Observability**

- [ ] `/metrics` exposed on an internal port and scraped
- [ ] Alert: dead letters increasing
- [ ] Alert: stream depth rising
- [ ] Alert: pending entries high
- [ ] Alert: `/readyz` failing
- [ ] Logs shipped somewhere searchable

**Growth control**

- [ ] Stream trimming scheduled
- [ ] `delivery_attempts` retention job scheduled

**Recovery**

- [ ] Nightly `pg_dump` to off-machine storage
- [ ] **A restore actually performed and verified**

**Confidence**

- [ ] CI green on `main`
- [ ] `scripts/verify.sh` passes against staging
- [ ] `scripts/chaos.sh` passes against staging
- [ ] Load test run at your expected peak volume

---

## What you are signing up for

With Stage 0 done you have a service that will not leak credentials or proxy
requests into your network. With Stages 1–3 you have one you can actually
operate. Stages 4–5 stop it degrading over months.

The honest summary of what remains: **head-of-line blocking is the real
architectural limit** (§6.3), and the plaintext signing secrets (§6.1) are the
remaining security compromise. Everything else on this list is routine
operational work.

Total cost, all stages: **$0.**
