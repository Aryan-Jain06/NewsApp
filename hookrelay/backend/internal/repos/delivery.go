package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/aryan-jain06/hookrelay/backend/internal/models"
	"github.com/google/uuid"
)

// DeliveryRepo owns the deliveries and delivery_attempts tables — the delivery
// state machine lives in these queries.
type DeliveryRepo struct{ q Querier }

const deliveryCols = `
	d.id, d.event_id, d.endpoint_id, d.tenant_id, d.status, d.attempt_count,
	d.next_attempt_at, d.last_status_code, d.last_error,
	d.created_at, d.updated_at, d.completed_at`

func scanDelivery(row interface{ Scan(...any) error }) (*models.Delivery, error) {
	var d models.Delivery
	err := row.Scan(&d.ID, &d.EventID, &d.EndpointID, &d.TenantID, &d.Status, &d.AttemptCount,
		&d.NextAttemptAt, &d.LastStatusCode, &d.LastError,
		&d.CreatedAt, &d.UpdatedAt, &d.CompletedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// CreateFanout inserts one pending delivery per endpoint for an event and
// returns them. The UNIQUE (event_id, endpoint_id) constraint plus DO NOTHING
// makes a retried fan-out idempotent, so a crash between insert and enqueue can
// be replayed safely.
func (r *DeliveryRepo) CreateFanout(ctx context.Context, eventID string, tenantID uuid.UUID, endpointIDs []uuid.UUID, dueAt time.Time) ([]*models.Delivery, error) {
	if len(endpointIDs) == 0 {
		return nil, nil
	}
	const q = `
		WITH d AS (
			INSERT INTO deliveries (event_id, endpoint_id, tenant_id, status, next_attempt_at)
			SELECT $1, unnest($2::uuid[]), $3, 'pending', $4
			ON CONFLICT (event_id, endpoint_id) DO NOTHING
			RETURNING *
		) SELECT ` + deliveryCols + ` FROM d`
	rows, err := r.q.Query(ctx, q, eventID, endpointIDs, tenantID, dueAt)
	if err != nil {
		return nil, fmt.Errorf("create fanout: %w", err)
	}
	defer rows.Close()
	var out []*models.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ByID loads a delivery without a tenant filter; used by workers.
func (r *DeliveryRepo) ByID(ctx context.Context, id uuid.UUID) (*models.Delivery, error) {
	d, err := scanDelivery(r.q.QueryRow(ctx, `SELECT `+deliveryCols+` FROM deliveries d WHERE d.id = $1`, id))
	if err != nil {
		return nil, fmt.Errorf("delivery by id: %w", mapErr(err))
	}
	return d, nil
}

// ByIDForTenant loads a delivery scoped to a tenant.
func (r *DeliveryRepo) ByIDForTenant(ctx context.Context, tenantID, id uuid.UUID) (*models.Delivery, error) {
	d, err := scanDelivery(r.q.QueryRow(ctx,
		`SELECT `+deliveryCols+` FROM deliveries d WHERE d.id = $1 AND d.tenant_id = $2`, id, tenantID))
	if err != nil {
		return nil, fmt.Errorf("delivery by id: %w", mapErr(err))
	}
	return d, nil
}

// ClaimForDelivery transitions a delivery to 'delivering' only if it is still
// 'pending'. Returning no row means the delivery is already terminal, already
// held by another worker, or waiting out a backoff as 'failed' — which is how a
// duplicate or reclaimed stream entry is made harmless instead of causing an
// early retry.
func (r *DeliveryRepo) ClaimForDelivery(ctx context.Context, id uuid.UUID) (*models.Delivery, error) {
	const q = `
		WITH d AS (
			UPDATE deliveries
			SET status = 'delivering', updated_at = now()
			WHERE id = $1 AND status = 'pending'
			RETURNING *
		) SELECT ` + deliveryCols + ` FROM d`
	d, err := scanDelivery(r.q.QueryRow(ctx, q, id))
	if err != nil {
		return nil, fmt.Errorf("claim delivery: %w", mapErr(err))
	}
	return d, nil
}

// ReleaseClaim puts a 'delivering' row back to 'pending' without burning an
// attempt. Used when a worker cannot proceed (endpoint vanished, shutdown).
func (r *DeliveryRepo) ReleaseClaim(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time) error {
	_, err := r.q.Exec(ctx, `
		UPDATE deliveries
		SET status = 'pending', next_attempt_at = $2, updated_at = now()
		WHERE id = $1 AND status = 'delivering'`, id, nextAttemptAt)
	if err != nil {
		return fmt.Errorf("release claim: %w", err)
	}
	return nil
}

// RecordAttempt writes one delivery_attempts row and moves the parent delivery
// to its next state in a single statement pair. Both happen in the caller's
// transaction so a worker crash can never leave an attempt unaccounted for.
func (r *DeliveryRepo) RecordAttempt(ctx context.Context, a models.Attempt) error {
	// The uniqueness guard is a *partial* index, so the conflict target has to
	// repeat its predicate for Postgres to infer it. Skipped attempts fall
	// outside the index and are therefore always inserted.
	const q = `
		INSERT INTO delivery_attempts (delivery_id, attempt_no, status_code, response_ms, error, outcome)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (delivery_id, attempt_no) WHERE outcome <> 'skipped' DO NOTHING`
	_, err := r.q.Exec(ctx, q, a.DeliveryID, a.AttemptNo, a.StatusCode, a.ResponseMS, a.Error, a.Outcome)
	if err != nil {
		return fmt.Errorf("record attempt: %w", err)
	}
	return nil
}

// MarkSucceeded closes out a delivery after a 2xx.
func (r *DeliveryRepo) MarkSucceeded(ctx context.Context, id uuid.UUID, attemptCount, statusCode int) error {
	_, err := r.q.Exec(ctx, `
		UPDATE deliveries SET
			status = 'succeeded', attempt_count = $2, last_status_code = $3,
			last_error = NULL, next_attempt_at = NULL,
			completed_at = now(), updated_at = now()
		WHERE id = $1`, id, attemptCount, statusCode)
	if err != nil {
		return fmt.Errorf("mark succeeded: %w", err)
	}
	return nil
}

// MarkFailed records a failed attempt and schedules the retry.
func (r *DeliveryRepo) MarkFailed(ctx context.Context, id uuid.UUID, attemptCount int, statusCode *int, errMsg string, nextAttemptAt time.Time) error {
	_, err := r.q.Exec(ctx, `
		UPDATE deliveries SET
			status = 'failed', attempt_count = $2, last_status_code = $3,
			last_error = $4, next_attempt_at = $5, updated_at = now()
		WHERE id = $1`, id, attemptCount, statusCode, errMsg, nextAttemptAt)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	return nil
}

// MarkDead parks a delivery in the dead-letter queue.
func (r *DeliveryRepo) MarkDead(ctx context.Context, id uuid.UUID, attemptCount int, statusCode *int, errMsg string) error {
	_, err := r.q.Exec(ctx, `
		UPDATE deliveries SET
			status = 'dead', attempt_count = $2, last_status_code = $3,
			last_error = $4, next_attempt_at = NULL,
			completed_at = now(), updated_at = now()
		WHERE id = $1`, id, attemptCount, statusCode, errMsg)
	if err != nil {
		return fmt.Errorf("mark dead: %w", err)
	}
	return nil
}

// Defer reschedules a delivery without consuming an attempt. Used when an
// endpoint's circuit breaker is open.
func (r *DeliveryRepo) Defer(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time) error {
	_, err := r.q.Exec(ctx, `
		UPDATE deliveries SET
			status = 'pending', next_attempt_at = $2, updated_at = now()
		WHERE id = $1`, id, nextAttemptAt)
	if err != nil {
		return fmt.Errorf("defer delivery: %w", err)
	}
	return nil
}

// DueForRetry promotes up to limit deliveries whose retry time has arrived to
// 'pending' and returns them for enqueueing.
//
// Rather than clearing next_attempt_at, it pushes it forward by lease. That is
// what closes the "committed but never enqueued" hole: if the XADD that follows
// fails, or the process dies between this UPDATE and the XADD, the row is still
// due once the lease expires and the next tick picks it up again. FOR UPDATE
// SKIP LOCKED lets several schedulers run without handing the same row out twice.
func (r *DeliveryRepo) DueForRetry(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]uuid.UUID, error) {
	const q = `
		WITH due AS (
			SELECT id FROM deliveries
			WHERE status IN ('pending', 'failed')
			  AND next_attempt_at IS NOT NULL
			  AND next_attempt_at <= $1
			ORDER BY next_attempt_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE deliveries d
		SET status = 'pending', next_attempt_at = $1 + $3::interval, updated_at = now()
		FROM due
		WHERE d.id = due.id
		RETURNING d.id`
	rows, err := r.q.Query(ctx, q, now, limit, lease.String())
	if err != nil {
		return nil, fmt.Errorf("due for retry: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan due id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReclaimStale rescues deliveries stuck in 'delivering' — the signature of a
// worker that died mid-attempt — by making them due again immediately.
func (r *DeliveryRepo) ReclaimStale(ctx context.Context, olderThan time.Time, limit int, lease time.Duration) ([]uuid.UUID, error) {
	const q = `
		WITH stale AS (
			SELECT id FROM deliveries
			WHERE status = 'delivering' AND updated_at < $1
			ORDER BY updated_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE deliveries d
		SET status = 'pending', next_attempt_at = $3, updated_at = now()
		FROM stale
		WHERE d.id = stale.id
		RETURNING d.id`
	rows, err := r.q.Query(ctx, q, olderThan, limit, time.Now().Add(lease))
	if err != nil {
		return nil, fmt.Errorf("reclaim stale: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stale id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ResetForReplay clears a delivery's history so it starts over from attempt 0.
// Returns ErrNotFound if the delivery does not belong to the tenant.
func (r *DeliveryRepo) ResetForReplay(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID, lease time.Duration) ([]uuid.UUID, error) {
	const q = `
		UPDATE deliveries SET
			status = 'pending', attempt_count = 0, next_attempt_at = now() + $3::interval,
			last_status_code = NULL, last_error = NULL,
			completed_at = NULL, updated_at = now()
		WHERE tenant_id = $1 AND id = ANY($2::uuid[])
		RETURNING id`
	rows, err := r.q.Query(ctx, q, tenantID, ids, lease.String())
	if err != nil {
		return nil, fmt.Errorf("reset for replay: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan replay id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResetForReplayByEvent resets every delivery of one event.
func (r *DeliveryRepo) ResetForReplayByEvent(ctx context.Context, tenantID uuid.UUID, eventID string, lease time.Duration) ([]uuid.UUID, error) {
	const q = `
		UPDATE deliveries SET
			status = 'pending', attempt_count = 0, next_attempt_at = now() + $3::interval,
			last_status_code = NULL, last_error = NULL,
			completed_at = NULL, updated_at = now()
		WHERE tenant_id = $1 AND event_id = $2
		RETURNING id`
	rows, err := r.q.Query(ctx, q, tenantID, eventID, lease.String())
	if err != nil {
		return nil, fmt.Errorf("reset event for replay: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan replay id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeliveryFilter narrows a delivery list query.
type DeliveryFilter struct {
	Status     string
	EventID    string
	EndpointID *uuid.UUID
	Limit      int
	Offset     int
}

// List returns deliveries for a tenant joined with endpoint URL and event type.
func (r *DeliveryRepo) List(ctx context.Context, tenantID uuid.UUID, f DeliveryFilter) ([]*models.Delivery, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	const q = `
		SELECT ` + deliveryCols + `, e.url, ev.event_type
		FROM deliveries d
		JOIN endpoints e ON e.id = d.endpoint_id
		JOIN events ev   ON ev.id = d.event_id
		WHERE d.tenant_id = $1
		  AND ($2 = '' OR d.status::text = $2)
		  AND ($3 = '' OR d.event_id = $3)
		  AND ($4::uuid IS NULL OR d.endpoint_id = $4)
		ORDER BY d.created_at DESC, d.id
		LIMIT $5 OFFSET $6`
	rows, err := r.q.Query(ctx, q, tenantID, f.Status, f.EventID, f.EndpointID, f.Limit, f.Offset)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	defer rows.Close()
	out := []*models.Delivery{}
	for rows.Next() {
		var d models.Delivery
		err := rows.Scan(&d.ID, &d.EventID, &d.EndpointID, &d.TenantID, &d.Status, &d.AttemptCount,
			&d.NextAttemptAt, &d.LastStatusCode, &d.LastError,
			&d.CreatedAt, &d.UpdatedAt, &d.CompletedAt, &d.EndpointURL, &d.EventType)
		if err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// CountByStatus returns a tenant's delivery counts keyed by status.
func (r *DeliveryRepo) CountByStatus(ctx context.Context, tenantID uuid.UUID) (map[string]int, error) {
	rows, err := r.q.Query(ctx, `
		SELECT status::text, count(*) FROM deliveries WHERE tenant_id = $1 GROUP BY status`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("count by status: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var (
			s string
			n int
		)
		if err := rows.Scan(&s, &n); err != nil {
			return nil, fmt.Errorf("scan status count: %w", err)
		}
		out[s] = n
	}
	return out, rows.Err()
}

// AttemptsFor loads every attempt for the given deliveries, oldest first.
func (r *DeliveryRepo) AttemptsFor(ctx context.Context, deliveryIDs []uuid.UUID) (map[uuid.UUID][]models.Attempt, error) {
	out := map[uuid.UUID][]models.Attempt{}
	if len(deliveryIDs) == 0 {
		return out, nil
	}
	const q = `
		SELECT id, delivery_id, attempt_no, status_code, response_ms, error, outcome, attempted_at
		FROM delivery_attempts
		WHERE delivery_id = ANY($1::uuid[])
		ORDER BY delivery_id, attempt_no`
	rows, err := r.q.Query(ctx, q, deliveryIDs)
	if err != nil {
		return nil, fmt.Errorf("load attempts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a models.Attempt
		if err := rows.Scan(&a.ID, &a.DeliveryID, &a.AttemptNo, &a.StatusCode, &a.ResponseMS,
			&a.Error, &a.Outcome, &a.AttemptedAt); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		out[a.DeliveryID] = append(out[a.DeliveryID], a)
	}
	return out, rows.Err()
}

// WorkItem is everything a worker needs to make one attempt, fetched in a single
// round trip after the delivery has been claimed.
type WorkItem struct {
	Delivery *models.Delivery
	Event    *models.Event
	Endpoint *models.Endpoint
}

// LoadWorkItem joins a claimed delivery with its event and endpoint.
func (r *DeliveryRepo) LoadWorkItem(ctx context.Context, deliveryID uuid.UUID) (*WorkItem, error) {
	const q = `
		SELECT ` + deliveryCols + `,
		       ev.id, ev.tenant_id, ev.event_type, ev.payload, ev.created_at,
		       e.id, e.tenant_id, e.url, e.secret, e.previous_secret, e.previous_secret_expires_at,
		       e.description, e.active, e.consecutive_failures, e.circuit_opened_until,
		       e.created_at, e.updated_at
		FROM deliveries d
		JOIN events ev  ON ev.id = d.event_id
		JOIN endpoints e ON e.id = d.endpoint_id
		WHERE d.id = $1`
	var (
		d       models.Delivery
		ev      models.Event
		ep      models.Endpoint
		prevSec *string
	)
	err := r.q.QueryRow(ctx, q, deliveryID).Scan(
		&d.ID, &d.EventID, &d.EndpointID, &d.TenantID, &d.Status, &d.AttemptCount,
		&d.NextAttemptAt, &d.LastStatusCode, &d.LastError, &d.CreatedAt, &d.UpdatedAt, &d.CompletedAt,
		&ev.ID, &ev.TenantID, &ev.EventType, &ev.Payload, &ev.CreatedAt,
		&ep.ID, &ep.TenantID, &ep.URL, &ep.Secret, &prevSec, &ep.PreviousSecretExpiresAt,
		&ep.Description, &ep.Active, &ep.ConsecutiveFailures, &ep.CircuitOpenedUntil,
		&ep.CreatedAt, &ep.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("load work item: %w", mapErr(err))
	}
	if prevSec != nil {
		ep.PreviousSecret = *prevSec
	}
	return &WorkItem{Delivery: &d, Event: &ev, Endpoint: &ep}, nil
}

// CountStuck reports deliveries that are neither settled nor scheduled — the
// load test's zero-loss assertion. A healthy system always returns 0 once the
// queue has drained.
func (r *DeliveryRepo) CountStuck(ctx context.Context) (int, error) {
	const q = `
		SELECT count(*) FROM deliveries
		WHERE status IN ('pending', 'failed', 'delivering')
		  AND (next_attempt_at IS NULL OR next_attempt_at < now() - interval '5 minutes')`
	var n int
	if err := r.q.QueryRow(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("count stuck: %w", err)
	}
	return n, nil
}
