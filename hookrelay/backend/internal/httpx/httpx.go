// Package httpx contains small helpers for writing JSON responses and errors.
package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// APIError is a client-visible error with an HTTP status attached.
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	err     error
}

func (e *APIError) Error() string {
	if e.err != nil {
		return e.Code + ": " + e.Message + ": " + e.err.Error()
	}
	return e.Code + ": " + e.Message
}

// Unwrap exposes the wrapped cause so errors.Is/As keep working.
func (e *APIError) Unwrap() error { return e.err }

// Errorf builds an APIError.
func Errorf(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}

// Wrap attaches an internal cause that is logged but never sent to the client.
func (e *APIError) Wrap(err error) *APIError {
	e.err = err
	return e
}

// Common error constructors.
func BadRequest(msg string) *APIError   { return Errorf(http.StatusBadRequest, "bad_request", msg) }
func Unauthorized(msg string) *APIError { return Errorf(http.StatusUnauthorized, "unauthorized", msg) }
func Forbidden(msg string) *APIError    { return Errorf(http.StatusForbidden, "forbidden", msg) }
func NotFound(msg string) *APIError     { return Errorf(http.StatusNotFound, "not_found", msg) }
func Conflict(msg string) *APIError     { return Errorf(http.StatusConflict, "conflict", msg) }
func Internal(err error) *APIError {
	return (&APIError{Status: http.StatusInternalServerError, Code: "internal_error", Message: "internal server error"}).Wrap(err)
}

// JSON writes v as a JSON body with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already flushed; all we can do is record it.
		slog.Error("write json response", "error", err)
	}
}

// Error writes err as a JSON error body, mapping unknown errors to a 500 and
// logging the internal cause.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = Internal(err)
	}
	if apiErr.Status >= 500 {
		slog.ErrorContext(r.Context(), "request failed",
			"method", r.Method, "path", r.URL.Path, "error", apiErr.Error())
	}
	JSON(w, apiErr.Status, map[string]any{"error": apiErr})
}

// DecodeJSON reads a JSON body into dst, rejecting unknown fields and oversized bodies.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return BadRequest("invalid JSON body: " + err.Error())
	}
	return nil
}
