package handlers

import (
	"context"
	"net/http"
	"time"
)

// contextWithTimeout derives a bounded context from the request.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
