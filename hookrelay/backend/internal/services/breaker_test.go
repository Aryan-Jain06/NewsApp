package services

import (
	"testing"
	"time"
)

func testBreaker() BreakerConfig {
	return BreakerConfig{Threshold: 20, Cooldown: 5 * time.Minute}
}

func TestBreakerStaysClosedBelowThreshold(t *testing.T) {
	c := testBreaker()
	now := time.Unix(1750000000, 0)
	s := BreakerState{}

	for i := 1; i < c.Threshold; i++ {
		s = c.OnFailure(s, now)
		if !c.Allow(s, now) {
			t.Fatalf("breaker opened after %d failures, threshold is %d", i, c.Threshold)
		}
		if s.ConsecutiveFailures != i {
			t.Fatalf("after %d failures ConsecutiveFailures = %d", i, s.ConsecutiveFailures)
		}
	}
}

func TestBreakerOpensExactlyAtThreshold(t *testing.T) {
	c := testBreaker()
	now := time.Unix(1750000000, 0)
	s := BreakerState{}
	for range c.Threshold {
		s = c.OnFailure(s, now)
	}

	if s.ConsecutiveFailures != c.Threshold {
		t.Fatalf("ConsecutiveFailures = %d, want %d", s.ConsecutiveFailures, c.Threshold)
	}
	if c.Allow(s, now) {
		t.Fatalf("breaker did not open at the %dth consecutive failure", c.Threshold)
	}
	if s.OpenUntil == nil {
		t.Fatal("OpenUntil was not set when the breaker opened")
	}
	if want := now.Add(c.Cooldown); !s.OpenUntil.Equal(want) {
		t.Errorf("OpenUntil = %s, want %s", s.OpenUntil, want)
	}
}

func TestBreakerClosesAfterCooldown(t *testing.T) {
	c := testBreaker()
	now := time.Unix(1750000000, 0)
	s := BreakerState{}
	for range c.Threshold {
		s = c.OnFailure(s, now)
	}

	justBefore := now.Add(c.Cooldown - time.Second)
	if c.Allow(s, justBefore) {
		t.Error("breaker allowed traffic one second before the cooldown expired")
	}
	if got, want := c.RetryAfter(s, justBefore), time.Second; got != want {
		t.Errorf("RetryAfter = %s, want %s", got, want)
	}

	after := now.Add(c.Cooldown + time.Millisecond)
	if !c.Allow(s, after) {
		t.Error("breaker still open after the cooldown expired")
	}
	if got := c.RetryAfter(s, after); got != 0 {
		t.Errorf("RetryAfter after cooldown = %s, want 0", got)
	}
}

func TestBreakerSuccessResetsStreak(t *testing.T) {
	c := testBreaker()
	now := time.Unix(1750000000, 0)

	s := BreakerState{}
	for range c.Threshold - 1 {
		s = c.OnFailure(s, now)
	}
	s = c.OnSuccess(s)
	if s.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d after success, want 0", s.ConsecutiveFailures)
	}
	if s.OpenUntil != nil {
		t.Error("OpenUntil should be cleared on success")
	}

	// The streak must start over, so one more failure cannot trip the breaker.
	s = c.OnFailure(s, now)
	if !c.Allow(s, now) {
		t.Error("breaker opened one failure after a success reset the streak")
	}
}

func TestBreakerSuccessClosesAnOpenBreaker(t *testing.T) {
	c := testBreaker()
	now := time.Unix(1750000000, 0)
	s := BreakerState{}
	for range c.Threshold {
		s = c.OnFailure(s, now)
	}
	if c.Allow(s, now) {
		t.Fatal("precondition: breaker should be open")
	}

	s = c.OnSuccess(s)
	if !c.Allow(s, now) {
		t.Error("a successful delivery did not close the breaker")
	}
}

func TestBreakerAlternatingFailuresNeverOpen(t *testing.T) {
	c := testBreaker()
	now := time.Unix(1750000000, 0)
	s := BreakerState{}

	// The breaker counts *consecutive* failures: a flaky endpoint that keeps
	// succeeding in between must never be paused.
	for range c.Threshold * 3 {
		s = c.OnFailure(s, now)
		s = c.OnSuccess(s)
		if !c.Allow(s, now) {
			t.Fatal("breaker opened on alternating failure/success")
		}
	}
}

func TestBreakerZeroThresholdNeverOpens(t *testing.T) {
	c := BreakerConfig{Threshold: 0, Cooldown: time.Minute}
	now := time.Unix(1750000000, 0)
	s := BreakerState{}
	for range 100 {
		s = c.OnFailure(s, now)
	}
	if !c.Allow(s, now) {
		t.Error("a zero threshold should disable the breaker, not open it immediately")
	}
}

func TestBreakerStateOpenIsTimeBased(t *testing.T) {
	now := time.Unix(1750000000, 0)
	until := now.Add(time.Minute)

	if (BreakerState{}).Open(now) {
		t.Error("a zero-value state must be closed")
	}
	if !(BreakerState{OpenUntil: &until}).Open(now) {
		t.Error("state with a future OpenUntil must be open")
	}
	if (BreakerState{OpenUntil: &until}).Open(until.Add(time.Second)) {
		t.Error("state must close once OpenUntil has passed")
	}
}
