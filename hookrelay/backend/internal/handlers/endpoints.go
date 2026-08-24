package handlers

import (
	"errors"
	"net/http"

	"github.com/aryan-jain06/hookrelay/backend/internal/httpx"
	"github.com/aryan-jain06/hookrelay/backend/internal/repos"
	"github.com/aryan-jain06/hookrelay/backend/internal/services"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// EndpointHandler serves endpoint CRUD and secret rotation.
type EndpointHandler struct {
	endpoints *services.EndpointService
}

// NewEndpointHandler builds an EndpointHandler.
func NewEndpointHandler(endpoints *services.EndpointService) *EndpointHandler {
	return &EndpointHandler{endpoints: endpoints}
}

// Create handles POST /endpoints.
func (h *EndpointHandler) Create(w http.ResponseWriter, r *http.Request) {
	var in services.CreateEndpointInput
	if err := httpx.DecodeJSON(w, r, &in, 32<<10); err != nil {
		httpx.Error(w, r, err)
		return
	}
	tenant := TenantFrom(r.Context())
	e, err := h.endpoints.Create(r.Context(), tenant.ID, in)
	if err != nil {
		writeEndpointError(w, r, err)
		return
	}
	// The secret is returned in full exactly once, at creation.
	httpx.JSON(w, http.StatusCreated, e)
}

// List handles GET /endpoints.
func (h *EndpointHandler) List(w http.ResponseWriter, r *http.Request) {
	tenant := TenantFrom(r.Context())
	out, err := h.endpoints.List(r.Context(), tenant.ID)
	if err != nil {
		httpx.Error(w, r, httpx.Internal(err))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"endpoints": out})
}

// Get handles GET /endpoints/{id}. ?reveal_secret=true includes the secret.
func (h *EndpointHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	reveal := r.URL.Query().Get("reveal_secret") == "true"
	tenant := TenantFrom(r.Context())
	e, err := h.endpoints.Get(r.Context(), tenant.ID, id, reveal)
	if err != nil {
		writeEndpointError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}

// Update handles PATCH /endpoints/{id}.
func (h *EndpointHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var in services.UpdateEndpointInput
	if err := httpx.DecodeJSON(w, r, &in, 32<<10); err != nil {
		httpx.Error(w, r, err)
		return
	}
	tenant := TenantFrom(r.Context())
	e, err := h.endpoints.Update(r.Context(), tenant.ID, id, in)
	if err != nil {
		writeEndpointError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}

// Delete handles DELETE /endpoints/{id}.
func (h *EndpointHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	tenant := TenantFrom(r.Context())
	if err := h.endpoints.Delete(r.Context(), tenant.ID, id); err != nil {
		writeEndpointError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RotateSecret handles POST /endpoints/{id}/rotate-secret.
func (h *EndpointHandler) RotateSecret(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	tenant := TenantFrom(r.Context())
	e, err := h.endpoints.RotateSecret(r.Context(), tenant.ID, id)
	if err != nil {
		writeEndpointError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"endpoint": e,
		"note":     "the previous secret keeps verifying until previous_secret_expires_at",
	})
}

// pathUUID parses a UUID URL parameter.
func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	raw := chi.URLParam(r, name)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, httpx.BadRequest("path parameter " + name + " is not a valid uuid")
	}
	return id, nil
}

// writeEndpointError maps service/repo errors onto HTTP statuses.
func writeEndpointError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, services.ErrValidation):
		httpx.Error(w, r, httpx.BadRequest(err.Error()))
	case errors.Is(err, repos.ErrNotFound):
		httpx.Error(w, r, httpx.NotFound("endpoint not found"))
	case errors.Is(err, repos.ErrConflict):
		httpx.Error(w, r, httpx.Conflict("endpoint conflicts with an existing record"))
	default:
		httpx.Error(w, r, httpx.Internal(err))
	}
}
