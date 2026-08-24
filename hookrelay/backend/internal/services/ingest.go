package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aryan-jain06/hookrelay/backend/internal/models"
	"github.com/aryan-jain06/hookrelay/backend/internal/queue"
	"github.com/aryan-jain06/hookrelay/backend/internal/repos"
	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
)

// MaxPayloadBytes caps a single event payload.
const MaxPayloadBytes = 256 << 10 // 256 KiB

// IngestService turns a published event into durable per-endpoint deliveries and
// puts them on the queue.
type IngestService struct {
	store *repos.Store
	queue *queue.Queue
}

// NewIngestService builds an IngestService.
func NewIngestService(store *repos.Store, q *queue.Queue) *IngestService {
	return &IngestService{store: store, queue: q}
}

// PublishInput is one event submitted by a producer.
type PublishInput struct {
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
}

// PublishResult describes what ingestion did.
type PublishResult struct {
	Event *models.Event `json:"event"`
	// DeliveryIDs are the deliveries created by this call (empty on an
	// idempotent replay, since the original call already created them).
	DeliveryIDs []uuid.UUID `json:"delivery_ids"`
	// Duplicate is true when an Idempotency-Key matched an earlier event.
	Duplicate bool `json:"duplicate"`
	// Enqueued counts the stream entries written.
	Enqueued int `json:"enqueued"`
}

// Publish validates, persists and fans an event out.
//
// The event row, the fan-out rows and the queue writes are ordered so that no
// step can lose work: everything is committed to Postgres first, and only then
// enqueued. If the process dies between commit and enqueue, the deliveries are
// still 'pending' in the database and the scheduler picks them up, so the
// database — not Redis — is the durable record.
func (s *IngestService) Publish(ctx context.Context, tenantID uuid.UUID, in PublishInput, idempotencyKey string) (*PublishResult, error) {
	eventType := strings.ToLower(strings.TrimSpace(in.EventType))
	if eventType == "" {
		return nil, validationErrorf("event_type is required")
	}
	if eventType == "*" {
		return nil, validationErrorf("event_type %q is reserved for subscriptions", "*")
	}
	if err := ValidateEventType(eventType); err != nil {
		return nil, err
	}
	if len(in.Payload) == 0 {
		return nil, validationErrorf("payload is required")
	}
	if len(in.Payload) > MaxPayloadBytes {
		return nil, validationErrorf("payload is %d bytes, limit is %d", len(in.Payload), MaxPayloadBytes)
	}
	if !json.Valid(in.Payload) {
		return nil, validationErrorf("payload is not valid JSON")
	}

	idempotencyKey = strings.TrimSpace(idempotencyKey)
	var keyPtr *string
	if idempotencyKey != "" {
		if len(idempotencyKey) > 255 {
			return nil, validationErrorf("Idempotency-Key exceeds 255 characters")
		}
		keyPtr = &idempotencyKey
	}

	// Fast path for a repeated key: return the original event and do not fan out.
	if keyPtr != nil {
		existing, err := s.store.Events.ByIdempotencyKey(ctx, tenantID, idempotencyKey)
		if err == nil {
			return &PublishResult{Event: existing, Duplicate: true}, nil
		}
		if !errors.Is(err, repos.ErrNotFound) {
			return nil, err
		}
	}

	var (
		event      *models.Event
		deliveries []*models.Delivery
	)
	err := s.store.InTx(ctx, func(tx *repos.TxStore) error {
		eventID := ulid.Make().String()
		ev, err := tx.Events.Create(ctx, eventID, tenantID, eventType, in.Payload, keyPtr)
		if err != nil {
			// Two concurrent requests with the same key: the loser reads the
			// winner's row instead of creating a second event.
			if errors.Is(err, repos.ErrConflict) && keyPtr != nil {
				return errIdempotentRace
			}
			return err
		}
		event = ev

		endpoints, err := tx.Endpoints.MatchingEndpoints(ctx, tenantID, eventType)
		if err != nil {
			return err
		}
		if len(endpoints) == 0 {
			// No subscriber: the event is still recorded, which matters for
			// debugging "why did nothing fire?".
			return nil
		}
		ids := make([]uuid.UUID, 0, len(endpoints))
		for _, e := range endpoints {
			ids = append(ids, e.ID)
		}
		deliveries, err = tx.Deliveries.CreateFanout(ctx, ev.ID, tenantID, ids, time.Now().Add(EnqueueLease))
		return err
	})
	switch {
	case errors.Is(err, errIdempotentRace):
		existing, lookupErr := s.store.Events.ByIdempotencyKey(ctx, tenantID, idempotencyKey)
		if lookupErr != nil {
			return nil, fmt.Errorf("resolve idempotency race: %w", lookupErr)
		}
		return &PublishResult{Event: existing, Duplicate: true}, nil
	case err != nil:
		return nil, err
	}

	result := &PublishResult{Event: event}
	if len(deliveries) == 0 {
		return result, nil
	}

	ids := make([]uuid.UUID, 0, len(deliveries))
	for _, d := range deliveries {
		ids = append(ids, d.ID)
	}
	result.DeliveryIDs = ids

	entryIDs, err := s.queue.EnqueueMany(ctx, ids)
	if err != nil {
		// Not fatal: the rows are committed as 'pending' with next_attempt_at
		// set, so the scheduler will enqueue them on its next tick.
		slog.ErrorContext(ctx, "enqueue after fan-out failed; scheduler will recover",
			"event_id", event.ID, "deliveries", len(ids), "error", err)
		return result, nil
	}
	result.Enqueued = len(entryIDs)
	return result, nil
}

// errIdempotentRace is an internal sentinel used to unwind the fan-out
// transaction when a concurrent request won the idempotency key.
var errIdempotentRace = errors.New("idempotency key race")

// ReplayEvent resets every delivery of an event and re-enqueues them.
func (s *IngestService) ReplayEvent(ctx context.Context, tenantID uuid.UUID, eventID string) ([]uuid.UUID, error) {
	if _, err := s.store.Events.ByID(ctx, tenantID, eventID); err != nil {
		return nil, err
	}
	ids, err := s.store.Deliveries.ResetForReplayByEvent(ctx, tenantID, eventID, EnqueueLease)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if _, err := s.queue.EnqueueMany(ctx, ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// ReplayDeliveries resets and re-enqueues specific deliveries. The endpoint's
// circuit breaker is cleared too, so an operator-triggered replay is never
// silently skipped by a breaker that is still open from the original failure.
func (s *IngestService) ReplayDeliveries(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, validationErrorf("at least one delivery id is required")
	}
	if len(ids) > 1000 {
		return nil, validationErrorf("at most 1000 deliveries may be replayed at once, got %d", len(ids))
	}

	reset, err := s.store.Deliveries.ResetForReplay(ctx, tenantID, ids, EnqueueLease)
	if err != nil {
		return nil, err
	}
	if len(reset) == 0 {
		return nil, repos.ErrNotFound
	}

	// Clear breakers for the endpoints involved.
	seen := map[uuid.UUID]struct{}{}
	for _, id := range reset {
		d, err := s.store.Deliveries.ByIDForTenant(ctx, tenantID, id)
		if err != nil {
			continue
		}
		if _, done := seen[d.EndpointID]; done {
			continue
		}
		seen[d.EndpointID] = struct{}{}
		if err := s.store.Endpoints.ResetBreaker(ctx, d.EndpointID); err != nil {
			slog.WarnContext(ctx, "reset breaker on replay failed", "endpoint_id", d.EndpointID, "error", err)
		}
	}

	if _, err := s.queue.EnqueueMany(ctx, reset); err != nil {
		return nil, err
	}
	return reset, nil
}
