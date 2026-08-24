package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/aryan-jain06/hookrelay/backend/internal/httpx"
	"github.com/aryan-jain06/hookrelay/backend/internal/models"
	"github.com/aryan-jain06/hookrelay/backend/internal/repos"
	"github.com/aryan-jain06/hookrelay/backend/internal/services"
	"github.com/google/uuid"
)

// DeliveryHandler serves delivery reads, the dead-letter queue, replays and stats.
type DeliveryHandler struct {
	store  *repos.Store
	ingest *services.IngestService
}

// NewDeliveryHandler builds a DeliveryHandler.
func NewDeliveryHandler(store *repos.Store, ingest *services.IngestService) *DeliveryHandler {
	return &DeliveryHandler{store: store, ingest: ingest}
}

var validStatuses = map[string]bool{
	"": true, "pending": true, "delivering": true,
	"succeeded": true, "failed": true, "dead": true,
}

// List handles GET /deliveries?status=dead&endpoint_id=&event_id=&limit=&offset=.
// With status=dead this is the dead-letter queue.
func (h *DeliveryHandler) List(w http.ResponseWriter, r *http.Request) {
	tenant := TenantFrom(r.Context())
	q := r.URL.Query()

	status := q.Get("status")
	if !validStatuses[status] {
		httpx.Error(w, r, httpx.BadRequest("status must be one of pending, delivering, succeeded, failed, dead"))
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	filter := repos.DeliveryFilter{
		Status:  status,
		EventID: q.Get("event_id"),
		Limit:   limit,
		Offset:  offset,
	}
	if raw := q.Get("endpoint_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, r, httpx.BadRequest("endpoint_id is not a valid uuid"))
			return
		}
		filter.EndpointID = &id
	}

	deliveries, err := h.store.Deliveries.List(r.Context(), tenant.ID, filter)
	if err != nil {
		httpx.Error(w, r, httpx.Internal(err))
		return
	}

	// The DLQ view needs the attempt history to explain why each row died.
	if q.Get("include_attempts") == "true" || status == string(models.StatusDead) {
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
	}

	counts, err := h.store.Deliveries.CountByStatus(r.Context(), tenant.ID)
	if err != nil {
		httpx.Error(w, r, httpx.Internal(err))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"deliveries": deliveries,
		"counts":     counts,
	})
}

// Get handles GET /deliveries/{id}.
func (h *DeliveryHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	tenant := TenantFrom(r.Context())
	d, err := h.store.Deliveries.ByIDForTenant(r.Context(), tenant.ID, id)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			httpx.Error(w, r, httpx.NotFound("delivery not found"))
			return
		}
		httpx.Error(w, r, httpx.Internal(err))
		return
	}
	attempts, err := h.store.Deliveries.AttemptsFor(r.Context(), []uuid.UUID{d.ID})
	if err != nil {
		httpx.Error(w, r, httpx.Internal(err))
		return
	}
	d.Attempts = attempts[d.ID]
	if d.Attempts == nil {
		d.Attempts = []models.Attempt{}
	}
	httpx.JSON(w, http.StatusOK, d)
}

// Replay handles POST /deliveries/{id}/replay.
func (h *DeliveryHandler) Replay(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	tenant := TenantFrom(r.Context())
	ids, err := h.ingest.ReplayDeliveries(r.Context(), tenant.ID, []uuid.UUID{id})
	if err != nil {
		writeReplayError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]any{"replayed": len(ids), "delivery_ids": ids})
}

type bulkReplayRequest struct {
	DeliveryIDs []uuid.UUID `json:"delivery_ids"`
	// Status replays every delivery in that state (used for "replay all dead").
	Status string `json:"status"`
	Limit  int    `json:"limit"`
}

// BulkReplay handles POST /deliveries/replay for the DLQ's bulk action.
func (h *DeliveryHandler) BulkReplay(w http.ResponseWriter, r *http.Request) {
	var req bulkReplayRequest
	if err := httpx.DecodeJSON(w, r, &req, 64<<10); err != nil {
		httpx.Error(w, r, err)
		return
	}
	tenant := TenantFrom(r.Context())

	ids := req.DeliveryIDs
	if len(ids) == 0 {
		if req.Status == "" {
			httpx.Error(w, r, httpx.BadRequest("provide delivery_ids or a status to replay"))
			return
		}
		if !validStatuses[req.Status] {
			httpx.Error(w, r, httpx.BadRequest("status must be one of pending, delivering, succeeded, failed, dead"))
			return
		}
		limit := req.Limit
		if limit <= 0 || limit > 1000 {
			limit = 1000
		}
		rows, err := h.store.Deliveries.List(r.Context(), tenant.ID, repos.DeliveryFilter{Status: req.Status, Limit: limit})
		if err != nil {
			httpx.Error(w, r, httpx.Internal(err))
			return
		}
		for _, d := range rows {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		httpx.JSON(w, http.StatusOK, map[string]any{"replayed": 0, "delivery_ids": []uuid.UUID{}})
		return
	}

	replayed, err := h.ingest.ReplayDeliveries(r.Context(), tenant.ID, ids)
	if err != nil {
		writeReplayError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]any{"replayed": len(replayed), "delivery_ids": replayed})
}

// EndpointStats handles GET /endpoints/{id}/stats?window_hours=24.
func (h *DeliveryHandler) EndpointStats(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	tenant := TenantFrom(r.Context())
	if _, err := h.store.Endpoints.ByID(r.Context(), tenant.ID, id); err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			httpx.Error(w, r, httpx.NotFound("endpoint not found"))
			return
		}
		httpx.Error(w, r, httpx.Internal(err))
		return
	}
	stats, err := h.store.Stats.ForEndpoint(r.Context(), tenant.ID, id, windowFrom(r, 24*time.Hour))
	if err != nil {
		httpx.Error(w, r, httpx.Internal(err))
		return
	}
	httpx.JSON(w, http.StatusOK, stats)
}

// Overview handles GET /stats/overview.
func (h *DeliveryHandler) Overview(w http.ResponseWriter, r *http.Request) {
	tenant := TenantFrom(r.Context())
	out, err := h.store.Stats.TenantOverview(r.Context(), tenant.ID, windowFrom(r, 24*time.Hour))
	if err != nil {
		httpx.Error(w, r, httpx.Internal(err))
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Timeseries handles GET /stats/timeseries?window_hours=1&bucket_seconds=60.
func (h *DeliveryHandler) Timeseries(w http.ResponseWriter, r *http.Request) {
	tenant := TenantFrom(r.Context())
	bucket := 60 * time.Second
	if raw := r.URL.Query().Get("bucket_seconds"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 3600 {
			bucket = time.Duration(n) * time.Second
		}
	}
	points, err := h.store.Stats.Timeseries(r.Context(), tenant.ID, windowFrom(r, time.Hour), bucket)
	if err != nil {
		httpx.Error(w, r, httpx.Internal(err))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"bucket_seconds": int(bucket.Seconds()),
		"points":         points,
	})
}

// windowFrom reads a ?window_hours= override, clamped to a week.
func windowFrom(r *http.Request, fallback time.Duration) time.Duration {
	raw := r.URL.Query().Get("window_hours")
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 24*7 {
		return fallback
	}
	return time.Duration(n) * time.Hour
}

// writeReplayError maps replay errors onto HTTP statuses.
func writeReplayError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, services.ErrValidation):
		httpx.Error(w, r, httpx.BadRequest(err.Error()))
	case errors.Is(err, repos.ErrNotFound):
		httpx.Error(w, r, httpx.NotFound("no matching delivery for this tenant"))
	default:
		httpx.Error(w, r, httpx.Internal(err))
	}
}
