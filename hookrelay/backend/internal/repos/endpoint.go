package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/aryan-jain06/hookrelay/backend/internal/models"
	"github.com/google/uuid"
)

// EndpointRepo reads and writes endpoints and their event-type subscriptions.
type EndpointRepo struct{ q Querier }

const endpointCols = `
	e.id, e.tenant_id, e.url, e.secret, e.previous_secret, e.previous_secret_expires_at,
	e.description, e.active, e.consecutive_failures, e.circuit_opened_until,
	e.created_at, e.updated_at`

// scanEndpoint reads a row selected with endpointCols.
func scanEndpoint(row interface{ Scan(...any) error }) (*models.Endpoint, error) {
	var (
		e       models.Endpoint
		prevSec *string
	)
	err := row.Scan(&e.ID, &e.TenantID, &e.URL, &e.Secret, &prevSec, &e.PreviousSecretExpiresAt,
		&e.Description, &e.Active, &e.ConsecutiveFailures, &e.CircuitOpenedUntil,
		&e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if prevSec != nil {
		e.PreviousSecret = *prevSec
	}
	return &e, nil
}

// Create inserts an endpoint and its subscriptions.
func (r *EndpointRepo) Create(ctx context.Context, tenantID uuid.UUID, url, description, secret string, active bool, eventTypes []string) (*models.Endpoint, error) {
	// The shared column list is qualified with an "e." alias, so the INSERT is
	// wrapped in a CTE named e to reuse it verbatim.
	const q = `
		WITH e AS (
			INSERT INTO endpoints (tenant_id, url, secret, description, active)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING *
		) SELECT ` + endpointCols + ` FROM e`
	e, err := scanEndpoint(r.q.QueryRow(ctx, q, tenantID, url, secret, description, active))
	if err != nil {
		return nil, fmt.Errorf("create endpoint: %w", mapErr(err))
	}
	if err := r.ReplaceSubscriptions(ctx, e.ID, eventTypes); err != nil {
		return nil, err
	}
	e.EventTypes = eventTypes
	return e, nil
}

// ByID loads one endpoint scoped to a tenant, including its event types.
func (r *EndpointRepo) ByID(ctx context.Context, tenantID, id uuid.UUID) (*models.Endpoint, error) {
	row := r.q.QueryRow(ctx, `SELECT `+endpointCols+` FROM endpoints e WHERE e.id = $1 AND e.tenant_id = $2`, id, tenantID)
	e, err := scanEndpoint(row)
	if err != nil {
		return nil, fmt.Errorf("endpoint by id: %w", mapErr(err))
	}
	types, err := r.EventTypes(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	e.EventTypes = types
	return e, nil
}

// ByIDUnscoped loads an endpoint without a tenant filter; used by delivery workers.
func (r *EndpointRepo) ByIDUnscoped(ctx context.Context, id uuid.UUID) (*models.Endpoint, error) {
	row := r.q.QueryRow(ctx, `SELECT `+endpointCols+` FROM endpoints e WHERE e.id = $1`, id)
	e, err := scanEndpoint(row)
	if err != nil {
		return nil, fmt.Errorf("endpoint by id: %w", mapErr(err))
	}
	return e, nil
}

// List returns a tenant's endpoints, newest first, with their event types.
func (r *EndpointRepo) List(ctx context.Context, tenantID uuid.UUID) ([]*models.Endpoint, error) {
	rows, err := r.q.Query(ctx, `SELECT `+endpointCols+` FROM endpoints e WHERE e.tenant_id = $1 ORDER BY e.created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	defer rows.Close()

	var out []*models.Endpoint
	byID := map[uuid.UUID]*models.Endpoint{}
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, fmt.Errorf("scan endpoint: %w", err)
		}
		e.EventTypes = []string{}
		out = append(out, e)
		byID[e.ID] = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate endpoints: %w", err)
	}

	// One extra round trip fills in every endpoint's subscriptions.
	subRows, err := r.q.Query(ctx, `
		SELECT s.endpoint_id, s.event_type
		FROM subscriptions s
		JOIN endpoints e ON e.id = s.endpoint_id
		WHERE e.tenant_id = $1
		ORDER BY s.event_type`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	defer subRows.Close()
	for subRows.Next() {
		var (
			id uuid.UUID
			et string
		)
		if err := subRows.Scan(&id, &et); err != nil {
			return nil, fmt.Errorf("scan subscription: %w", err)
		}
		if e, ok := byID[id]; ok {
			e.EventTypes = append(e.EventTypes, et)
		}
	}
	return out, subRows.Err()
}

// Update applies a partial update. Nil fields are left untouched.
func (r *EndpointRepo) Update(ctx context.Context, tenantID, id uuid.UUID, url, description *string, active *bool) (*models.Endpoint, error) {
	row := r.q.QueryRow(ctx, `
		WITH e AS (
			UPDATE endpoints SET
				url         = COALESCE($3, url),
				description = COALESCE($4, description),
				active      = COALESCE($5, active),
				updated_at  = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING *
		) SELECT `+endpointCols+` FROM e`, id, tenantID, url, description, active)
	e, err := scanEndpoint(row)
	if err != nil {
		return nil, fmt.Errorf("update endpoint: %w", mapErr(err))
	}
	return e, nil
}

// Delete removes an endpoint (cascading to its subscriptions and deliveries).
func (r *EndpointRepo) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	tag, err := r.q.Exec(ctx, `DELETE FROM endpoints WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	if err != nil {
		return fmt.Errorf("delete endpoint: %w", mapErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RotateSecret installs newSecret and keeps the old one valid until graceUntil.
func (r *EndpointRepo) RotateSecret(ctx context.Context, tenantID, id uuid.UUID, newSecret string, graceUntil time.Time) (*models.Endpoint, error) {
	row := r.q.QueryRow(ctx, `
		WITH e AS (
			UPDATE endpoints SET
				previous_secret            = secret,
				previous_secret_expires_at = $4,
				secret                     = $3,
				updated_at                 = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING *
		) SELECT `+endpointCols+` FROM e`, id, tenantID, newSecret, graceUntil)
	e, err := scanEndpoint(row)
	if err != nil {
		return nil, fmt.Errorf("rotate secret: %w", mapErr(err))
	}
	return e, nil
}

// EventTypes lists an endpoint's subscribed event types.
func (r *EndpointRepo) EventTypes(ctx context.Context, endpointID uuid.UUID) ([]string, error) {
	rows, err := r.q.Query(ctx, `SELECT event_type FROM subscriptions WHERE endpoint_id = $1 ORDER BY event_type`, endpointID)
	if err != nil {
		return nil, fmt.Errorf("list event types: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var et string
		if err := rows.Scan(&et); err != nil {
			return nil, fmt.Errorf("scan event type: %w", err)
		}
		out = append(out, et)
	}
	return out, rows.Err()
}

// ReplaceSubscriptions makes the endpoint's subscriptions exactly eventTypes.
func (r *EndpointRepo) ReplaceSubscriptions(ctx context.Context, endpointID uuid.UUID, eventTypes []string) error {
	if _, err := r.q.Exec(ctx, `DELETE FROM subscriptions WHERE endpoint_id = $1`, endpointID); err != nil {
		return fmt.Errorf("clear subscriptions: %w", err)
	}
	if len(eventTypes) == 0 {
		return nil
	}
	_, err := r.q.Exec(ctx, `
		INSERT INTO subscriptions (endpoint_id, event_type)
		SELECT $1, unnest($2::text[])
		ON CONFLICT DO NOTHING`, endpointID, eventTypes)
	if err != nil {
		return fmt.Errorf("insert subscriptions: %w", err)
	}
	return nil
}

// MatchingEndpoints returns the active endpoints a tenant has subscribed to
// eventType. A subscription of "*" matches every event type.
func (r *EndpointRepo) MatchingEndpoints(ctx context.Context, tenantID uuid.UUID, eventType string) ([]*models.Endpoint, error) {
	rows, err := r.q.Query(ctx, `
		SELECT DISTINCT `+endpointCols+`
		FROM endpoints e
		JOIN subscriptions s ON s.endpoint_id = e.id
		WHERE e.tenant_id = $1
		  AND e.active
		  AND (s.event_type = $2 OR s.event_type = '*')
		ORDER BY e.created_at`, tenantID, eventType)
	if err != nil {
		return nil, fmt.Errorf("match endpoints: %w", err)
	}
	defer rows.Close()
	var out []*models.Endpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, fmt.Errorf("scan endpoint: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordSuccess clears the failure streak and closes the breaker.
func (r *EndpointRepo) RecordSuccess(ctx context.Context, endpointID uuid.UUID) error {
	_, err := r.q.Exec(ctx, `
		UPDATE endpoints
		SET consecutive_failures = 0, circuit_opened_until = NULL, updated_at = now()
		WHERE id = $1`, endpointID)
	if err != nil {
		return fmt.Errorf("record endpoint success: %w", err)
	}
	return nil
}

// RecordFailure increments the failure streak and, once it reaches threshold,
// opens the breaker for cooldown. The whole decision happens in one statement so
// concurrent workers cannot race past the threshold.
func (r *EndpointRepo) RecordFailure(ctx context.Context, endpointID uuid.UUID, threshold int, cooldown time.Duration) (failures int, openUntil *time.Time, err error) {
	err = r.q.QueryRow(ctx, `
		UPDATE endpoints SET
			consecutive_failures = consecutive_failures + 1,
			circuit_opened_until = CASE
				WHEN consecutive_failures + 1 >= $2 THEN now() + $3::interval
				ELSE circuit_opened_until
			END,
			updated_at = now()
		WHERE id = $1
		RETURNING consecutive_failures, circuit_opened_until`,
		endpointID, threshold, cooldown.String()).Scan(&failures, &openUntil)
	if err != nil {
		return 0, nil, fmt.Errorf("record endpoint failure: %w", mapErr(err))
	}
	return failures, openUntil, nil
}

// ResetBreaker force-closes the breaker; used by replay so a user-triggered
// retry is never silently skipped.
func (r *EndpointRepo) ResetBreaker(ctx context.Context, endpointID uuid.UUID) error {
	_, err := r.q.Exec(ctx, `
		UPDATE endpoints
		SET consecutive_failures = 0, circuit_opened_until = NULL, updated_at = now()
		WHERE id = $1`, endpointID)
	if err != nil {
		return fmt.Errorf("reset breaker: %w", err)
	}
	return nil
}
