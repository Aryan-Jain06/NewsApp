package services

import (
	"testing"
	"time"
)

func TestRetryScheduleMatchesSpec(t *testing.T) {
	want := []time.Duration{
		5 * time.Second,
		30 * time.Second,
		2 * time.Minute,
		10 * time.Minute,
		30 * time.Minute,
		2 * time.Hour,
		5 * time.Hour,
	}
	if len(RetrySchedule) != len(want) {
		t.Fatalf("schedule has %d entries, want %d", len(RetrySchedule), len(want))
	}
	for i, w := range want {
		if RetrySchedule[i] != w {
			t.Errorf("retry %d: got %s, want %s", i+1, RetrySchedule[i], w)
		}
	}
	if MaxAttempts != len(want)+1 {
		t.Errorf("MaxAttempts = %d, want %d (one initial attempt plus %d retries)", MaxAttempts, len(want)+1, len(want))
	}
}

func TestRetryScheduleIsStrictlyIncreasing(t *testing.T) {
	for i := 1; i < len(RetrySchedule); i++ {
		if RetrySchedule[i] <= RetrySchedule[i-1] {
			t.Errorf("retry %d (%s) is not longer than retry %d (%s)",
				i+1, RetrySchedule[i], i, RetrySchedule[i-1])
		}
	}
}

func TestBaseDelayBoundaries(t *testing.T) {
	if _, ok := BaseDelay(0); ok {
		t.Error("attempt 0 should be invalid")
	}
	if _, ok := BaseDelay(-1); ok {
		t.Error("negative attempt should be invalid")
	}
	if d, ok := BaseDelay(1); !ok || d != 5*time.Second {
		t.Errorf("attempt 1: got (%s, %v), want (5s, true)", d, ok)
	}
	last := len(RetrySchedule)
	if d, ok := BaseDelay(last); !ok || d != 5*time.Hour {
		t.Errorf("attempt %d: got (%s, %v), want (5h, true)", last, d, ok)
	}
	// The attempt after the last scheduled retry has no delay: it goes dead.
	if _, ok := BaseDelay(last + 1); ok {
		t.Errorf("attempt %d should exhaust the budget and return ok=false", last+1)
	}
	if _, ok := BaseDelay(MaxAttempts); ok {
		t.Errorf("the final attempt (%d) must not schedule another retry", MaxAttempts)
	}
}

func TestApplyJitterBounds(t *testing.T) {
	base := 100 * time.Second
	cases := map[string]struct {
		r    float64
		want time.Duration
	}{
		"lower bound": {r: 0.0, want: 80 * time.Second},
		"midpoint":    {r: 0.5, want: 100 * time.Second},
		"upper bound": {r: 1.0, want: 120 * time.Second},
	}
	for name, c := range cases {
		if got := applyJitter(base, c.r); got != c.want {
			t.Errorf("%s: applyJitter(%s, %v) = %s, want %s", name, base, c.r, got, c.want)
		}
	}
}

func TestNextDelayStaysWithinJitterBounds(t *testing.T) {
	for attempt := 1; attempt <= len(RetrySchedule); attempt++ {
		lo, hi, ok := JitterBounds(attempt)
		if !ok {
			t.Fatalf("attempt %d: JitterBounds reported not ok", attempt)
		}
		base, _ := BaseDelay(attempt)
		if lo != time.Duration(float64(base)*0.8) || hi != time.Duration(float64(base)*1.2) {
			t.Errorf("attempt %d: bounds [%s, %s] are not ±20%% of %s", attempt, lo, hi, base)
		}

		// Sample enough draws to be confident the range is respected and that
		// jitter is actually being applied rather than silently skipped.
		var sawBelowBase, sawAboveBase bool
		for range 2000 {
			d, ok := NextDelay(attempt)
			if !ok {
				t.Fatalf("attempt %d: NextDelay reported not ok", attempt)
			}
			if d < lo || d > hi {
				t.Fatalf("attempt %d: delay %s outside bounds [%s, %s]", attempt, d, lo, hi)
			}
			if d < base {
				sawBelowBase = true
			}
			if d > base {
				sawAboveBase = true
			}
		}
		if !sawBelowBase || !sawAboveBase {
			t.Errorf("attempt %d: jitter never varied in both directions (below=%v above=%v)",
				attempt, sawBelowBase, sawAboveBase)
		}
	}
}

func TestNextDelayExhaustsBudget(t *testing.T) {
	if _, ok := NextDelay(len(RetrySchedule) + 1); ok {
		t.Error("NextDelay should report not ok once the retry budget is spent")
	}
}

func TestTotalRetryWindowIsHours(t *testing.T) {
	var total time.Duration
	for _, d := range RetrySchedule {
		total += d
	}
	// The spec promises retries "for hours"; guard against an accidental edit
	// that collapses the schedule.
	if total < 7*time.Hour {
		t.Errorf("total retry window is %s, expected at least 7h", total)
	}
}
