package repos

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aryan-jain06/hookrelay/backend/internal/models"
	"github.com/google/uuid"
)

// EventRepo reads and writes published events.
type EventRepo struct{ q Querier }

const eventCols = `id, tenant_id, event_type, payload, idempotency_key, created_at`

// Create inserts an event with a caller-supplied ULID.
func (r *EventRepo) Create(ctx context.Context, id string, tenantID uuid.UUID, eventType string, payload json.RawMessage, idempotencyKey *string) (*models.Event, error) {
	const q = `
		INSERT INTO events (id, tenant_id, event_type, payload, idempotency_key)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING ` + eventCols
	var e models.Event
	err := r.q.QueryRow(ctx, q, id, tenantID, eventType, payload, idempotencyKey).
		Scan(&e.ID, &e.TenantID, &e.EventType, &e.Payload, &e.IdempotencyKey, &e.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create event: %w", mapErr(err))
	}
	return &e, nil
}

// ByIdempotencyKey finds a tenant's earlier event published under the same key.
func (r *EventRepo) ByIdempotencyKey(ctx context.Context, tenantID uuid.UUID, key string) (*models.Event, error) {
	const q = `SELECT ` + eventCols + ` FROM events WHERE tenant_id = $1 AND idempotency_key = $2`
	var e models.Event
	err := r.q.QueryRow(ctx, q, tenantID, key).
		Scan(&e.ID, &e.TenantID, &e.EventType, &e.Payload, &e.IdempotencyKey, &e.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("event by idempotency key: %w", mapErr(err))
	}
	return &e, nil
}

// ByID loads one event scoped to a tenant.
func (r *EventRepo) ByID(ctx context.Context, tenantID uuid.UUID, id string) (*models.Event, error) {
	const q = `SELECT ` + eventCols + ` FROM events WHERE tenant_id = $1 AND id = $2`
	var e models.Event
	err := r.q.QueryRow(ctx, q, tenantID, id).
		Scan(&e.ID, &e.TenantID, &e.EventType, &e.Payload, &e.IdempotencyKey, &e.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("event by id: %w", mapErr(err))
	}
	return &e, nil
}

// EventFilter narrows a List query.
type EventFilter struct {
	EventType string
	Limit     int
	// Cursor is a ULID; results are strictly older than it. ULIDs sort
	// lexicographically by time, so this is a stable keyset pagination.
	Cursor string
}

// List returns a page of a tenant's events, newest first.
func (r *EventRepo) List(ctx context.Context, tenantID uuid.UUID, f EventFilter) ([]*models.Event, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	const q = `
		SELECT ` + eventCols + `
		FROM events
		WHERE tenant_id = $1
		  AND ($2 = '' OR event_type = $2)
		  AND ($3 = '' OR id < $3)
		ORDER BY id DESC
		LIMIT $4`
	rows, err := r.q.Query(ctx, q, tenantID, f.EventType, f.Cursor, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	out := []*models.Event{}
	for rows.Next() {
		var e models.Event
		if err := rows.Scan(&e.ID, &e.TenantID, &e.EventType, &e.Payload, &e.IdempotencyKey, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
