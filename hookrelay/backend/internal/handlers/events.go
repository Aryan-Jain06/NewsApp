package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/aryan-jain06/hookrelay/backend/internal/httpx"
	"github.com/aryan-jain06/hookrelay/backend/internal/models"
	"github.com/aryan-jain06/hookrelay/backend/internal/repos"
	"github.com/aryan-jain06/hookrelay/backend/internal/services"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// EventHandler serves event ingestion and the event read APIs.
type EventHandler struct {
	ingest *services.IngestService
	store  *repos.Store
}

// NewEventHandler builds an EventHandler.
func NewEventHandler(ingest *services.IngestService, store *repos.Store) *EventHandler {
	return &EventHandler{ingest: ingest, store: store}
}

// Publish handles POST /events. It honours an Idempotency-Key header: a repeated
// key returns the original event and fans nothing out a second time.
func (h *EventHandler) Publish(w http.ResponseWriter, r *http.Request) {
	var in services.PublishInput
	if err := httpx.DecodeJSON(w, r, &in, services.MaxPayloadBytes+(8<<10)); err != nil {
		httpx.Error(w, r, err)
		return
	}
	tenant := TenantFrom(r.Context())
	res, err := h.ingest.Publish(r.Context(), tenant.ID, in, r.Header.Get("Idempotency-Key"))
	if err != nil {
		if errors.Is(err, services.ErrValidation) {
			httpx.Error(w, r, httpx.BadRequest(err.Error()))
			return
		}
		httpx.Error(w, r, httpx.Internal(err))
		return
	}

	body := map[string]any{
		"event_id":     res.Event.ID,
		"event_type":   res.Event.EventType,
		"created_at":   res.Event.CreatedAt,
		"deliveries":   len(res.DeliveryIDs),
		"delivery_ids": res.DeliveryIDs,
		"duplicate":    res.Duplicate,
	}
	if res.Duplicate {
		// Same key, same event: 200 rather than 202 makes the replay visible to
		// the producer without implying new work was accepted.
		httpx.JSON(w, http.StatusOK, body)
		return
	}
	httpx.JSON(w, http.StatusAccepted, body)
}

// List handles GET /events.
func (h *EventHandler) List(w http.ResponseWriter, r *http.Request) {
	tenant := TenantFrom(r.Context())
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	events, err := h.store.Events.List(r.Context(), tenant.ID, repos.EventFilter{
		EventType: q.Get("event_type"),
		Limit:     limit,
		Cursor:    q.Get("cursor"),
	})
	if err != nil {
		httpx.Error(w, r, httpx.Internal(err))
		return
	}

	// Attach a per-event delivery summary so the list page can show status
	// without one request per row.
	summaries := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		ds, err := h.store.Deliveries.List(r.Context(), tenant.ID, repos.DeliveryFilter{EventID: ev.ID, Limit: 100})
		if err != nil {
			httpx.Error(w, r, httpx.Internal(err))
			return
		}
		counts := map[string]int{}
		for _, d := range ds {
			counts[string(d.Status)]++
		}
		summaries = append(summaries, map[string]any{
			"event":                ev,
			"delivery_count":       len(ds),
			"deliveries_by_status": counts,
		})
	}

	next := ""
	if len(events) > 0 {
		next = events[len(events)-1].ID
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"events": summaries, "next_cursor": next})
}

// Get handles GET /events/{id} and returns every delivery with every attempt —
// the data behind the dashboard's delivery timeline.
func (h *EventHandler) Get(w http.ResponseWriter, r *http.Request) {
	tenant := TenantFrom(r.Context())
	eventID := chi.URLParam(r, "id")

	ev, err := h.store.Events.ByID(r.Context(), tenant.ID, eventID)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			httpx.Error(w, r, httpx.NotFound("event not found"))
			return
		}
		httpx.Error(w, r, httpx.Internal(err))
		return
	}

	deliveries, err := h.store.Deliveries.List(r.Context(), tenant.ID, repos.DeliveryFilter{EventID: eventID, Limit: 500})
	if err != nil {
		httpx.Error(w, r, httpx.Internal(err))
		return
	}
	ids := make([]uuid.UUID, 0, len(deliveries))
	for _, d := range deliveries {
		ids = append(ids, d.ID)
	}
	attempts, err := h.store.Deliveries.AttemptsFor(r.Context(), ids)
	if err != nil {
		httpx.Error(w, r, httpx.Internal(err))
		return
	}
	for _, d := range deliveries {
		if a, ok := attempts[d.ID]; ok {
			d.Attempts = a
		} else {
			d.Attempts = []models.Attempt{}
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"event": ev, "deliveries": deliveries})
}

// Replay handles POST /events/{id}/replay: reset and re-enqueue every delivery.
func (h *EventHandler) Replay(w http.ResponseWriter, r *http.Request) {
	tenant := TenantFrom(r.Context())
	eventID := chi.URLParam(r, "id")

	ids, err := h.ingest.ReplayEvent(r.Context(), tenant.ID, eventID)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			httpx.Error(w, r, httpx.NotFound("event not found"))
			return
		}
		httpx.Error(w, r, httpx.Internal(err))
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]any{
		"event_id":     eventID,
		"replayed":     len(ids),
		"delivery_ids": ids,
	})
}
