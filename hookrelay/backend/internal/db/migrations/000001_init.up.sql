-- HookRelay core schema.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE tenants (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    -- SHA-256 of the raw API key: we need a constant-time O(1) lookup by key,
    -- which a per-row salted hash (bcrypt) cannot give us.
    api_key_hash   TEXT NOT NULL UNIQUE,
    api_key_prefix TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE endpoints (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    url         TEXT NOT NULL,
    secret      TEXT NOT NULL,
    -- Rotation keeps the superseded secret valid until its expiry so in-flight
    -- receivers can accept either signature.
    previous_secret            TEXT,
    previous_secret_expires_at TIMESTAMPTZ,
    description TEXT NOT NULL DEFAULT '',
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    -- Per-endpoint circuit breaker state.
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    circuit_opened_until TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX endpoints_tenant_idx ON endpoints (tenant_id);

CREATE TABLE subscriptions (
    endpoint_id UUID NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    event_type  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (endpoint_id, event_type)
);
CREATE INDEX subscriptions_event_type_idx ON subscriptions (event_type);

CREATE TABLE events (
    id              TEXT PRIMARY KEY,               -- ULID: sortable by creation time
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_type      TEXT NOT NULL,
    payload         JSONB NOT NULL,
    idempotency_key TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX events_tenant_created_idx ON events (tenant_id, created_at DESC);
CREATE UNIQUE INDEX events_tenant_idempotency_key_idx
    ON events (tenant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE TYPE delivery_status AS ENUM (
    'pending',     -- queued, waiting for a worker
    'delivering',  -- a worker holds it and the HTTP request is in flight
    'succeeded',   -- endpoint answered 2xx
    'failed',      -- attempt failed, retry scheduled at next_attempt_at
    'dead'         -- retry budget exhausted; parked in the dead-letter queue
);

CREATE TABLE deliveries (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id         TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    endpoint_id      UUID NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    -- Denormalised so dashboard queries stay tenant-scoped without extra joins.
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    status           delivery_status NOT NULL DEFAULT 'pending',
    attempt_count    INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ,
    last_status_code INTEGER,
    last_error       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ,
    UNIQUE (event_id, endpoint_id)
);
CREATE INDEX deliveries_event_idx  ON deliveries (event_id);
CREATE INDEX deliveries_tenant_status_idx ON deliveries (tenant_id, status, created_at DESC);
CREATE INDEX deliveries_endpoint_idx ON deliveries (endpoint_id, created_at DESC);
-- Drives the scheduler: find rows whose retry is due.
CREATE INDEX deliveries_due_idx ON deliveries (next_attempt_at)
    WHERE status IN ('pending', 'failed');

CREATE TABLE delivery_attempts (
    id           BIGSERIAL PRIMARY KEY,
    delivery_id  UUID NOT NULL REFERENCES deliveries(id) ON DELETE CASCADE,
    attempt_no   INTEGER NOT NULL,
    status_code  INTEGER,
    response_ms  INTEGER,
    error        TEXT,
    -- success | failure | skipped (skipped = circuit breaker was open)
    outcome      TEXT NOT NULL,
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Real attempts are deduplicated by number so a duplicate stream delivery cannot
-- double-count one try. Skipped attempts (circuit breaker open) are append-only:
-- an endpoint can be skipped many times at the same attempt number.
CREATE UNIQUE INDEX delivery_attempts_no_idx
    ON delivery_attempts (delivery_id, attempt_no)
    WHERE outcome <> 'skipped';
CREATE INDEX delivery_attempts_delivery_idx ON delivery_attempts (delivery_id, attempt_no);
CREATE INDEX delivery_attempts_attempted_at_idx ON delivery_attempts (attempted_at DESC);
