package services

import "time"

// BreakerConfig is the per-endpoint circuit-breaker policy.
//
// The breaker exists to protect the worker pool, not the receiver: an endpoint
// that is hard down would otherwise soak up a worker for the full delivery
// timeout on every queued delivery, starving healthy endpoints behind it. After
// Threshold consecutive failures the endpoint is skipped for Cooldown instead.
type BreakerConfig struct {
	Threshold int
	Cooldown  time.Duration
}

// BreakerState is the breaker's persisted state. It lives in the endpoints table
// rather than in worker memory so every worker — and a restarted worker — shares
// one view of an endpoint's health.
type BreakerState struct {
	ConsecutiveFailures int
	OpenUntil           *time.Time
}

// Open reports whether the breaker is tripped at now.
func (s BreakerState) Open(now time.Time) bool {
	return s.OpenUntil != nil && s.OpenUntil.After(now)
}

// Allow reports whether a delivery attempt may proceed.
func (c BreakerConfig) Allow(s BreakerState, now time.Time) bool {
	return !s.Open(now)
}

// OnFailure records a failure and trips the breaker once the streak reaches
// Threshold.
func (c BreakerConfig) OnFailure(s BreakerState, now time.Time) BreakerState {
	next := BreakerState{ConsecutiveFailures: s.ConsecutiveFailures + 1, OpenUntil: s.OpenUntil}
	if c.Threshold > 0 && next.ConsecutiveFailures >= c.Threshold {
		until := now.Add(c.Cooldown)
		next.OpenUntil = &until
	}
	return next
}

// OnSuccess clears the streak and closes the breaker. One success is enough:
// the endpoint has demonstrably recovered, and holding it half-open would only
// delay legitimate traffic.
func (c BreakerConfig) OnSuccess(BreakerState) BreakerState {
	return BreakerState{ConsecutiveFailures: 0, OpenUntil: nil}
}

// RetryAfter returns how long to defer a delivery whose endpoint's breaker is
// open, so it is retried just after the cooldown expires.
func (c BreakerConfig) RetryAfter(s BreakerState, now time.Time) time.Duration {
	if s.OpenUntil == nil || !s.OpenUntil.After(now) {
		return 0
	}
	return s.OpenUntil.Sub(now)
}
