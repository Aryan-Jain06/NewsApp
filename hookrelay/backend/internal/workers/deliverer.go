// Package workers contains the delivery pipeline: a pool of workers consuming
// the Redis Stream, a scheduler that makes due retries visible again, and a
// reaper that rescues work abandoned by a crashed worker.
package workers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/aryan-jain06/hookrelay/backend/internal/models"
	"github.com/aryan-jain06/hookrelay/backend/internal/repos"
	"github.com/aryan-jain06/hookrelay/backend/internal/services"
	"github.com/google/uuid"
)

// maxResponseBytes caps how much of a receiver's response body we read into the
// error field. Receivers sometimes answer an error with a full HTML page.
const maxResponseBytes = 2 << 10

// Deliverer performs a single delivery attempt and applies the resulting state
// transition. It holds no per-delivery state, so one instance is shared by every
// worker goroutine.
type Deliverer struct {
	store   *repos.Store
	client  *http.Client
	breaker services.BreakerConfig
	policy  services.RetryPolicy
	timeout time.Duration
	maxAge  time.Duration
}

// NewDeliverer builds a Deliverer. maxAge is the absolute deadline after which a
// delivery is dead-lettered regardless of how many attempts it has left.
func NewDeliverer(store *repos.Store, breaker services.BreakerConfig, policy services.RetryPolicy, timeout, maxAge time.Duration) *Deliverer {
	return &Deliverer{
		store:   store,
		breaker: breaker,
		policy:  policy,
		timeout: timeout,
		maxAge:  maxAge,
		client: &http.Client{
			// Per-request timeouts come from the context; this is a backstop.
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        512,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
			// Webhook receivers should not be able to bounce us somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Outcome describes what happened to one delivery, for logging and metrics.
type Outcome string

// Possible outcomes of Deliver.
const (
	// OutcomeDelivered means the endpoint answered 2xx.
	OutcomeDelivered Outcome = "delivered"
	// OutcomeRetrying means the attempt failed and a retry is scheduled.
	OutcomeRetrying Outcome = "retrying"
	// OutcomeDead means the retry budget is exhausted.
	OutcomeDead Outcome = "dead"
	// OutcomeSkipped means the endpoint's breaker was open or it is inactive.
	OutcomeSkipped Outcome = "skipped"
	// OutcomeExpired means the delivery outlived DeliveryMaxAge.
	OutcomeExpired Outcome = "expired"
	// OutcomeNotClaimable means another worker owns it, or it is already settled.
	OutcomeNotClaimable Outcome = "not_claimable"
	// OutcomeGone means the delivery or its event/endpoint no longer exists.
	OutcomeGone Outcome = "gone"
)

// Deliver runs one attempt for deliveryID.
//
// Every path returns without error once the delivery's state has been durably
// recorded, because the caller acknowledges the stream entry only on a nil
// error. An error therefore means "nothing was recorded, leave the entry
// pending" — which is what preserves at-least-once delivery.
func (d *Deliverer) Deliver(ctx context.Context, deliveryID uuid.UUID) (Outcome, error) {
	claimed, err := d.store.Deliveries.ClaimForDelivery(ctx, deliveryID)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			// Already succeeded, already dead, waiting out a backoff, or held by
			// another worker. A duplicate stream entry lands here and is dropped.
			return OutcomeNotClaimable, nil
		}
		return "", fmt.Errorf("claim delivery %s: %w", deliveryID, err)
	}

	item, err := d.store.Deliveries.LoadWorkItem(ctx, claimed.ID)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			return OutcomeGone, nil
		}
		// Release the claim so the row does not sit in 'delivering' forever.
		d.release(ctx, claimed.ID)
		return "", fmt.Errorf("load work item %s: %w", deliveryID, err)
	}

	now := time.Now()
	attemptNo := item.Delivery.AttemptCount + 1

	// Absolute deadline. Breaker skips do not consume the retry budget, so this
	// is what guarantees a delivery to a permanently dead endpoint terminates
	// instead of being deferred in a loop forever.
	if d.maxAge > 0 && now.Sub(item.Delivery.CreatedAt) > d.maxAge {
		return d.expire(ctx, item, attemptNo, now)
	}

	// Circuit breaker and inactive endpoints are both "do not call out now".
	state := services.BreakerState{
		ConsecutiveFailures: item.Endpoint.ConsecutiveFailures,
		OpenUntil:           item.Endpoint.CircuitOpenedUntil,
	}
	if !item.Endpoint.Active || !d.breaker.Allow(state, now) {
		return d.skip(ctx, item, attemptNo, now, state)
	}

	body, err := services.BuildBody(item.Event.ID, item.Event.EventType, now, item.Event.Payload)
	if err != nil {
		d.release(ctx, claimed.ID)
		return "", fmt.Errorf("build body for %s: %w", deliveryID, err)
	}

	res := d.post(ctx, item, body, now, attemptNo)

	if res.success {
		return d.recordSuccess(ctx, item, attemptNo, res)
	}
	return d.recordFailure(ctx, item, attemptNo, res, now)
}

// attemptResult is the observable result of one HTTP call.
type attemptResult struct {
	success    bool
	statusCode *int
	latencyMS  int
	errMsg     string
}

// post signs and sends the webhook, and classifies the response. Only a 2xx
// counts as success; everything else — including a 3xx we refuse to follow — is
// a failure worth retrying.
func (d *Deliverer) post(ctx context.Context, item *repos.WorkItem, body []byte, now time.Time, attemptNo int) attemptResult {
	reqCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, item.Endpoint.URL, bytes.NewReader(body))
	if err != nil {
		return attemptResult{errMsg: fmt.Sprintf("build request: %v", err)}
	}
	ts := now.Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "HookRelay/1.0")
	req.Header.Set(services.HeaderID, item.Event.ID)
	req.Header.Set(services.HeaderTimestamp, fmt.Sprintf("%d", ts))
	req.Header.Set(services.HeaderEventType, item.Event.EventType)
	req.Header.Set(services.HeaderAttempt, fmt.Sprintf("%d", attemptNo))
	// During a rotation grace window this carries both signatures.
	req.Header.Set(services.HeaderSignature,
		services.SignatureHeader(item.Endpoint.SigningSecrets(now), item.Event.ID, ts, body))

	start := time.Now()
	resp, err := d.client.Do(req)
	latency := int(time.Since(start).Milliseconds())
	if err != nil {
		return attemptResult{latencyMS: latency, errMsg: classifyTransportError(err)}
	}
	defer func() {
		// Drain a little so the connection can be reused, then close.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()

	code := resp.StatusCode
	out := attemptResult{statusCode: &code, latencyMS: latency}
	if code >= 200 && code < 300 {
		out.success = true
		return out
	}

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	out.errMsg = fmt.Sprintf("endpoint returned HTTP %d: %s", code, truncate(string(snippet), 512))
	return out
}

// recordSuccess writes the successful attempt and settles the delivery. The
// attempt row and the state change go in one transaction so an observer never
// sees a succeeded delivery with no attempt behind it.
func (d *Deliverer) recordSuccess(ctx context.Context, item *repos.WorkItem, attemptNo int, res attemptResult) (Outcome, error) {
	err := d.store.InTx(ctx, func(tx *repos.TxStore) error {
		if err := tx.Deliveries.RecordAttempt(ctx, models.Attempt{
			DeliveryID: item.Delivery.ID,
			AttemptNo:  attemptNo,
			StatusCode: res.statusCode,
			ResponseMS: &res.latencyMS,
			Outcome:    models.OutcomeSuccess,
		}); err != nil {
			return err
		}
		if err := tx.Deliveries.MarkSucceeded(ctx, item.Delivery.ID, attemptNo, *res.statusCode); err != nil {
			return err
		}
		return tx.Endpoints.RecordSuccess(ctx, item.Endpoint.ID)
	})
	if err != nil {
		return "", fmt.Errorf("record success for %s: %w", item.Delivery.ID, err)
	}
	return OutcomeDelivered, nil
}

// recordFailure writes the failed attempt and either schedules a retry or parks
// the delivery in the dead-letter queue.
func (d *Deliverer) recordFailure(ctx context.Context, item *repos.WorkItem, attemptNo int, res attemptResult, now time.Time) (Outcome, error) {
	delay, retry := d.policy.NextDelay(attemptNo)
	outcome := OutcomeRetrying
	if !retry {
		outcome = OutcomeDead
	}

	err := d.store.InTx(ctx, func(tx *repos.TxStore) error {
		errMsg := res.errMsg
		if err := tx.Deliveries.RecordAttempt(ctx, models.Attempt{
			DeliveryID: item.Delivery.ID,
			AttemptNo:  attemptNo,
			StatusCode: res.statusCode,
			ResponseMS: &res.latencyMS,
			Error:      &errMsg,
			Outcome:    models.OutcomeFailure,
		}); err != nil {
			return err
		}
		if retry {
			if err := tx.Deliveries.MarkFailed(ctx, item.Delivery.ID, attemptNo, res.statusCode, errMsg, now.Add(delay)); err != nil {
				return err
			}
		} else if err := tx.Deliveries.MarkDead(ctx, item.Delivery.ID, attemptNo, res.statusCode, errMsg); err != nil {
			return err
		}
		// The failure streak is counted in the same transaction so the breaker
		// cannot drift from the attempt history.
		_, _, err := tx.Endpoints.RecordFailure(ctx, item.Endpoint.ID, d.breaker.Threshold, d.breaker.Cooldown)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("record failure for %s: %w", item.Delivery.ID, err)
	}

	slog.DebugContext(ctx, "delivery attempt failed",
		"delivery_id", item.Delivery.ID, "endpoint", item.Endpoint.URL,
		"attempt", attemptNo, "of", d.policy.MaxAttempts(),
		"status_code", res.statusCode, "error", res.errMsg,
		"retry_in", delay.String(), "outcome", string(outcome))
	return outcome, nil
}

// skip records a skipped attempt and defers the delivery until the breaker's
// cooldown expires. The attempt count is deliberately not incremented: an
// endpoint being paused must not burn the delivery's retry budget.
func (d *Deliverer) skip(ctx context.Context, item *repos.WorkItem, attemptNo int, now time.Time, state services.BreakerState) (Outcome, error) {
	wait := d.breaker.RetryAfter(state, now)
	reason := fmt.Sprintf("circuit breaker open after %d consecutive failures", state.ConsecutiveFailures)
	if !item.Endpoint.Active {
		// A disabled endpoint has no cooldown to wait out, so use the breaker's
		// cooldown as a poll interval.
		wait = d.breaker.Cooldown
		reason = "endpoint is disabled"
	}
	if wait <= 0 {
		wait = d.breaker.Cooldown
	}

	err := d.store.InTx(ctx, func(tx *repos.TxStore) error {
		if err := tx.Deliveries.RecordAttempt(ctx, models.Attempt{
			DeliveryID: item.Delivery.ID,
			AttemptNo:  attemptNo,
			Error:      &reason,
			Outcome:    models.OutcomeSkipped,
		}); err != nil {
			return err
		}
		return tx.Deliveries.Defer(ctx, item.Delivery.ID, now.Add(wait))
	})
	if err != nil {
		return "", fmt.Errorf("record skip for %s: %w", item.Delivery.ID, err)
	}
	return OutcomeSkipped, nil
}

// expire dead-letters a delivery that outlived DeliveryMaxAge, recording why so
// the dashboard can explain it rather than showing an unexplained dead row.
func (d *Deliverer) expire(ctx context.Context, item *repos.WorkItem, attemptNo int, now time.Time) (Outcome, error) {
	age := now.Sub(item.Delivery.CreatedAt).Round(time.Second)
	reason := fmt.Sprintf("gave up after %s: exceeded the maximum delivery age of %s (endpoint was unavailable for the whole window)", age, d.maxAge)

	err := d.store.InTx(ctx, func(tx *repos.TxStore) error {
		if err := tx.Deliveries.RecordAttempt(ctx, models.Attempt{
			DeliveryID: item.Delivery.ID,
			AttemptNo:  attemptNo,
			Error:      &reason,
			Outcome:    models.OutcomeFailure,
		}); err != nil {
			return err
		}
		return tx.Deliveries.MarkDead(ctx, item.Delivery.ID, attemptNo, item.Delivery.LastStatusCode, reason)
	})
	if err != nil {
		return "", fmt.Errorf("expire delivery %s: %w", item.Delivery.ID, err)
	}
	return OutcomeExpired, nil
}

// release returns a claimed delivery to 'pending' without burning an attempt.
func (d *Deliverer) release(ctx context.Context, id uuid.UUID) {
	// Use a detached context: the caller's may already be cancelled.
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := d.store.Deliveries.ReleaseClaim(relCtx, id, time.Now().Add(services.EnqueueLease)); err != nil {
		slog.Error("release delivery claim", "delivery_id", id, "error", err)
	}
}

// classifyTransportError turns a Go transport error into a short, stable message
// so the dashboard can group failures instead of showing raw dial noise.
func classifyTransportError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out"
	case errors.Is(err, context.Canceled):
		return "request cancelled during shutdown"
	default:
		return truncate("transport error: "+err.Error(), 512)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
