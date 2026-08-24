package services

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// RetrySchedule is the delay before each retry, in order. A delivery gets one
// initial attempt plus len(RetrySchedule) retries, spread over roughly 8 hours,
// which is long enough for an endpoint to survive a deploy or a short outage
// without the sender giving up.
var RetrySchedule = []time.Duration{
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	5 * time.Hour,
}

// JitterFraction spreads retries by ±20%. Without it, a fleet of deliveries that
// failed together would retry in lockstep and hammer a recovering endpoint with
// a synchronised thundering herd.
const JitterFraction = 0.20

// EnqueueLease is how far ahead next_attempt_at is pushed when a delivery is
// handed to the queue. If the enqueue is lost, the row becomes due again after
// the lease instead of being orphaned.
const EnqueueLease = 60 * time.Second

// RetryPolicy is a retry schedule. It is a value type so tests — and a
// compressed schedule in a test environment — can use their own without
// mutating global state.
type RetryPolicy struct {
	Schedule []time.Duration
}

// DefaultRetryPolicy is the production schedule.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Schedule: RetrySchedule}
}

// ParseRetryPolicy reads a comma-separated duration list such as
// "5s,30s,2m,10m". An empty string yields the default schedule. This exists so a
// test or staging environment can compress the schedule to seconds and still
// exercise the real dead-letter path.
func ParseRetryPolicy(spec string) (RetryPolicy, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return DefaultRetryPolicy(), nil
	}
	parts := strings.Split(spec, ",")
	out := make([]time.Duration, 0, len(parts))
	for _, raw := range parts {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return RetryPolicy{}, fmt.Errorf("retry schedule entry %q: %w", raw, err)
		}
		if d <= 0 {
			return RetryPolicy{}, fmt.Errorf("retry schedule entry %q must be positive", raw)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return RetryPolicy{}, fmt.Errorf("retry schedule %q contains no entries", spec)
	}
	return RetryPolicy{Schedule: out}, nil
}

// String renders the policy for logging.
func (p RetryPolicy) String() string {
	parts := make([]string, 0, len(p.Schedule))
	for _, d := range p.Schedule {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, ",")
}

// MaxAttempts is the total number of HTTP attempts before a delivery is parked
// in the dead-letter queue.
func (p RetryPolicy) MaxAttempts() int { return len(p.Schedule) + 1 }

// BaseDelay returns the un-jittered delay after attemptNo has failed. attemptNo
// is 1-based. ok is false when the retry budget is exhausted, which is the signal
// to mark the delivery dead.
func (p RetryPolicy) BaseDelay(attemptNo int) (delay time.Duration, ok bool) {
	if attemptNo < 1 || attemptNo > len(p.Schedule) {
		return 0, false
	}
	return p.Schedule[attemptNo-1], true
}

// NextDelay returns the jittered delay after attemptNo has failed. The result is
// always within ±JitterFraction of BaseDelay and never negative.
func (p RetryPolicy) NextDelay(attemptNo int) (delay time.Duration, ok bool) {
	base, ok := p.BaseDelay(attemptNo)
	if !ok {
		return 0, false
	}
	return applyJitter(base, rand.Float64()), true
}

// JitterBounds returns the inclusive range NextDelay can produce for attemptNo.
func (p RetryPolicy) JitterBounds(attemptNo int) (lo, hi time.Duration, ok bool) {
	base, ok := p.BaseDelay(attemptNo)
	if !ok {
		return 0, 0, false
	}
	return time.Duration(float64(base) * (1 - JitterFraction)),
		time.Duration(float64(base) * (1 + JitterFraction)),
		true
}

// Window is the total time a delivery may spend retrying before going dead.
func (p RetryPolicy) Window() time.Duration {
	var total time.Duration
	for _, d := range p.Schedule {
		total += d
	}
	return total
}

// applyJitter scales d by 1 + (2*r-1)*JitterFraction for r in [0,1). It is split
// out from NextDelay so tests can drive the randomness deterministically.
func applyJitter(d time.Duration, r float64) time.Duration {
	factor := 1 + (2*r-1)*JitterFraction
	jittered := time.Duration(float64(d) * factor)
	if jittered < 0 {
		return 0
	}
	return jittered
}

// MaxAttempts is the default policy's attempt budget.
var MaxAttempts = DefaultRetryPolicy().MaxAttempts()

// BaseDelay delegates to the default policy.
func BaseDelay(attemptNo int) (time.Duration, bool) { return DefaultRetryPolicy().BaseDelay(attemptNo) }

// NextDelay delegates to the default policy.
func NextDelay(attemptNo int) (time.Duration, bool) { return DefaultRetryPolicy().NextDelay(attemptNo) }

// JitterBounds delegates to the default policy.
func JitterBounds(attemptNo int) (time.Duration, time.Duration, bool) {
	return DefaultRetryPolicy().JitterBounds(attemptNo)
}
