// Package models holds the domain types shared across repos, services and handlers.
package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// DeliveryStatus mirrors the delivery_status Postgres enum.
type DeliveryStatus string

const (
	// StatusPending means the delivery is queued and waiting for a worker.
	StatusPending DeliveryStatus = "pending"
	// StatusDelivering means a worker holds it and an HTTP request is in flight.
	StatusDelivering DeliveryStatus = "delivering"
	// StatusSucceeded means the endpoint answered 2xx.
	StatusSucceeded DeliveryStatus = "succeeded"
	// StatusFailed means the last attempt failed and a retry is scheduled.
	StatusFailed DeliveryStatus = "failed"
	// StatusDead means the retry budget is exhausted; the delivery is in the DLQ.
	StatusDead DeliveryStatus = "dead"
)

// Terminal reports whether no further attempts will be made without a replay.
func (s DeliveryStatus) Terminal() bool {
	return s == StatusSucceeded || s == StatusDead
}

// Attempt outcomes recorded in delivery_attempts.outcome.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeSkipped = "skipped" // circuit breaker was open
)

// Tenant is an API consumer: one set of endpoints, one API key, one dashboard login.
type Tenant struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Email        string    `json:"email"`
	APIKeyPrefix string    `json:"api_key_prefix"`
	CreatedAt    time.Time `json:"created_at"`
}

// Endpoint is a subscriber URL owned by a tenant.
type Endpoint struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	URL         string    `json:"url"`
	Description string    `json:"description"`
	Active      bool      `json:"active"`

	// Secret is omitted from list responses and only revealed through the
	// dedicated reveal endpoint.
	Secret                  string     `json:"secret,omitempty"`
	PreviousSecret          string     `json:"-"`
	PreviousSecretExpiresAt *time.Time `json:"previous_secret_expires_at,omitempty"`

	ConsecutiveFailures int        `json:"consecutive_failures"`
	CircuitOpenedUntil  *time.Time `json:"circuit_opened_until,omitempty"`

	EventTypes []string  `json:"event_types"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// CircuitOpen reports whether the breaker is currently tripped at time now.
func (e *Endpoint) CircuitOpen(now time.Time) bool {
	return e.CircuitOpenedUntil != nil && e.CircuitOpenedUntil.After(now)
}

// SigningSecrets returns every secret a receiver may legitimately have used to
// verify a signature, newest first. During a rotation grace window that is two.
func (e *Endpoint) SigningSecrets(now time.Time) []string {
	secrets := []string{e.Secret}
	if e.PreviousSecret != "" && e.PreviousSecretExpiresAt != nil && e.PreviousSecretExpiresAt.After(now) {
		secrets = append(secrets, e.PreviousSecret)
	}
	return secrets
}

// Event is an immutable record of something a producer published.
type Event struct {
	ID             string          `json:"id"`
	TenantID       uuid.UUID       `json:"tenant_id"`
	EventType      string          `json:"event_type"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey *string         `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// Delivery tracks one event's journey to one endpoint.
type Delivery struct {
	ID             uuid.UUID      `json:"id"`
	EventID        string         `json:"event_id"`
	EndpointID     uuid.UUID      `json:"endpoint_id"`
	TenantID       uuid.UUID      `json:"tenant_id"`
	Status         DeliveryStatus `json:"status"`
	AttemptCount   int            `json:"attempt_count"`
	NextAttemptAt  *time.Time     `json:"next_attempt_at,omitempty"`
	LastStatusCode *int           `json:"last_status_code,omitempty"`
	LastError      *string        `json:"last_error,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	CompletedAt    *time.Time     `json:"completed_at,omitempty"`

	// Populated by the read APIs for the dashboard.
	EndpointURL string    `json:"endpoint_url,omitempty"`
	EventType   string    `json:"event_type,omitempty"`
	Attempts    []Attempt `json:"attempts"`
}

// Attempt is one HTTP try against an endpoint.
type Attempt struct {
	ID          int64     `json:"id"`
	DeliveryID  uuid.UUID `json:"delivery_id"`
	AttemptNo   int       `json:"attempt_no"`
	StatusCode  *int      `json:"status_code,omitempty"`
	ResponseMS  *int      `json:"response_ms,omitempty"`
	Error       *string   `json:"error,omitempty"`
	Outcome     string    `json:"outcome"`
	AttemptedAt time.Time `json:"attempted_at"`
}
