package handlers

import (
	"net/http"
	"os"
	"time"

	"github.com/aryan-jain06/hookrelay/backend/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Deps is everything the router needs to serve the API.
type Deps struct {
	Auth       *AuthHandler
	Endpoints  *EndpointHandler
	Events     *EventHandler
	Deliveries *DeliveryHandler
	Health     *HealthHandler
	AuthMW     func(http.Handler) http.Handler
}

// NewRouter builds the full API surface.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(Recoverer)
	r.Use(RequestLogger)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(CORS(corsOrigin()))

	// Unauthenticated.
	r.Get("/healthz", d.Health.Healthz)
	r.Get("/readyz", d.Health.Readyz)
	r.Post("/auth/register", d.Auth.Register)
	r.Post("/auth/login", d.Auth.Login)

	// Authenticated: API key (producers) or dashboard JWT.
	r.Group(func(r chi.Router) {
		r.Use(d.AuthMW)

		r.Get("/auth/me", d.Auth.Me)

		r.Route("/endpoints", func(r chi.Router) {
			r.Post("/", d.Endpoints.Create)
			r.Get("/", d.Endpoints.List)
			r.Get("/{id}", d.Endpoints.Get)
			r.Patch("/{id}", d.Endpoints.Update)
			r.Delete("/{id}", d.Endpoints.Delete)
			r.Post("/{id}/rotate-secret", d.Endpoints.RotateSecret)
			r.Get("/{id}/stats", d.Deliveries.EndpointStats)
		})

		r.Route("/events", func(r chi.Router) {
			r.Post("/", d.Events.Publish)
			r.Get("/", d.Events.List)
			r.Get("/{id}", d.Events.Get)
			r.Post("/{id}/replay", d.Events.Replay)
		})

		r.Route("/deliveries", func(r chi.Router) {
			r.Get("/", d.Deliveries.List)
			r.Get("/{id}", d.Deliveries.Get)
			r.Post("/{id}/replay", d.Deliveries.Replay)
			r.Post("/replay", d.Deliveries.BulkReplay)
		})

		r.Get("/stats/overview", d.Deliveries.Overview)
		r.Get("/stats/timeseries", d.Deliveries.Timeseries)
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, httpx.NotFound("no such route"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, httpx.Errorf(http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed on this route"))
	})
	return r
}

// corsOrigin returns the dashboard origin allowed to call the API.
func corsOrigin() string {
	if v := os.Getenv("CORS_ALLOW_ORIGIN"); v != "" {
		return v
	}
	return "*"
}
