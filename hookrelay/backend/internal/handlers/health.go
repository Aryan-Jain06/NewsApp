package handlers

import (
	"net/http"
	"time"

	"github.com/aryan-jain06/hookrelay/backend/internal/httpx"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// HealthHandler reports process and dependency health.
type HealthHandler struct {
	pool  *pgxpool.Pool
	redis *redis.Client
}

// NewHealthHandler builds a HealthHandler.
func NewHealthHandler(pool *pgxpool.Pool, rdb *redis.Client) *HealthHandler {
	return &HealthHandler{pool: pool, redis: rdb}
}

// Healthz is a liveness probe: the process is up.
func (h *HealthHandler) Healthz(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz is a readiness probe: Postgres and Redis both answer.
func (h *HealthHandler) Readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 2*time.Second)
	defer cancel()

	out := map[string]any{"status": "ok", "postgres": "ok", "redis": "ok"}
	status := http.StatusOK

	if err := h.pool.Ping(ctx); err != nil {
		out["postgres"], out["status"], status = err.Error(), "degraded", http.StatusServiceUnavailable
	}
	if err := h.redis.Ping(ctx).Err(); err != nil {
		out["redis"], out["status"], status = err.Error(), "degraded", http.StatusServiceUnavailable
	}
	httpx.JSON(w, status, out)
}
