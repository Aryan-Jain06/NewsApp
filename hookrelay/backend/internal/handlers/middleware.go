// Package handlers holds the HTTP layer: middleware, routing and request handlers.
package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aryan-jain06/hookrelay/backend/internal/httpx"
	"github.com/aryan-jain06/hookrelay/backend/internal/models"
	"github.com/aryan-jain06/hookrelay/backend/internal/services"
	"github.com/go-chi/chi/v5/middleware"
)

type ctxKey string

const tenantCtxKey ctxKey = "tenant"

// TenantFrom returns the authenticated tenant, or nil when unauthenticated.
func TenantFrom(ctx context.Context) *models.Tenant {
	t, _ := ctx.Value(tenantCtxKey).(*models.Tenant)
	return t
}

// withTenant stores the tenant on the request context.
func withTenant(ctx context.Context, t *models.Tenant) context.Context {
	return context.WithValue(ctx, tenantCtxKey, t)
}

// bearerToken extracts the credential from an Authorization header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// RequireAuth accepts either a tenant API key (hrk_...) or a dashboard JWT on the
// same Authorization: Bearer header, so the dashboard and producers share routes.
func RequireAuth(auth *services.AuthService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := bearerToken(r)
			if raw == "" {
				httpx.Error(w, r, httpx.Unauthorized("missing Authorization: Bearer credential"))
				return
			}

			var (
				tenant *models.Tenant
				err    error
			)
			if strings.HasPrefix(raw, services.APIKeyPrefix) {
				tenant, err = auth.TenantFromAPIKey(r.Context(), raw)
			} else {
				tenant, err = auth.TenantFromToken(r.Context(), raw)
			}
			if err != nil {
				if errors.Is(err, services.ErrInvalidCredentials) {
					httpx.Error(w, r, httpx.Unauthorized("invalid credential"))
					return
				}
				httpx.Error(w, r, httpx.Unauthorized("could not authenticate credential"))
				return
			}
			next.ServeHTTP(w, r.WithContext(withTenant(r.Context(), tenant)))
		})
	}
}

// RequestLogger logs one structured line per request. It never logs the
// Authorization header.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		level := slog.LevelInfo
		if ww.Status() >= 500 {
			level = slog.LevelError
		}
		slog.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}

// Recoverer turns a panic into a 500 instead of tearing the server down.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				slog.ErrorContext(r.Context(), "panic recovered",
					"panic", rec, "method", r.Method, "path", r.URL.Path)
				httpx.Error(w, r, httpx.Internal(errors.New("panic recovered")))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// CORS allows the dashboard dev server to call the API from another origin.
func CORS(allowedOrigin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && (allowedOrigin == "*" || origin == allowedOrigin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,DELETE,OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type,Idempotency-Key")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
