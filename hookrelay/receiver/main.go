// Command receiver is a deliberately misbehaving webhook endpoint used to prove
// HookRelay's retry, dead-letter, replay and signing behaviour.
//
// Routes:
//
//	GET/POST /ok              always 200
//	         /flaky?rate=0.3  returns 500 with the given probability
//	         /slow?ms=15000   sleeps, so the sender's 10s timeout fires
//	         /fail?code=500   always fails (feeds the dead-letter queue)
//	         /switch          fails until flipped healthy via /_control
//	         /verify?secret=  verifies the HMAC signature and rejects bad ones
//	GET      /healthz         liveness
//	GET      /_stats          per-route counters and unique event ids seen
//	POST     /_control        {"switch":"ok"|"fail"} flips /switch at runtime
//	POST     /_reset          clears all counters
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxBody caps how much of a webhook we will read.
const maxBody = 1 << 20

// signature header names, mirroring the sender.
const (
	headerID        = "X-HookRelay-Id"
	headerTimestamp = "X-HookRelay-Timestamp"
	headerSignature = "X-HookRelay-Signature"
	headerAttempt   = "X-HookRelay-Attempt"
)

// toleranceWindow is how much clock skew we accept on headerTimestamp. Anything
// older is treated as a replay of a captured request.
const toleranceWindow = 5 * time.Minute

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := run(); err != nil {
		slog.Error("receiver exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := os.Getenv("RECEIVER_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	s := newState()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/ok", s.handleOK)
	mux.HandleFunc("/flaky", s.handleFlaky)
	mux.HandleFunc("/slow", s.handleSlow)
	mux.HandleFunc("/fail", s.handleFail)
	mux.HandleFunc("/switch", s.handleSwitch)
	mux.HandleFunc("/verify", s.handleVerify)
	mux.HandleFunc("/_stats", s.handleStats)
	mux.HandleFunc("/_control", s.handleControl)
	mux.HandleFunc("/_reset", s.handleReset)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Must exceed the longest /slow sleep so the handler, not the server,
		// decides when a slow request ends.
		WriteTimeout: 5 * time.Minute,
		ReadTimeout:  time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("receiver listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("listen and serve: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// routeStats counts what one route saw.
type routeStats struct {
	Requests       int            `json:"requests"`
	Accepted       int            `json:"accepted"`
	Rejected       int            `json:"rejected"`
	UniqueEvents   int            `json:"unique_events"`
	Duplicates     int            `json:"duplicates"`
	MaxAttemptSeen int            `json:"max_attempt_seen"`
	ByStatus       map[string]int `json:"by_status"`

	// seen tracks event ids so we can distinguish a retry from a new event.
	seen map[string]int `json:"-"`
}

// state is the receiver's mutable behaviour and bookkeeping.
type state struct {
	mu         sync.Mutex
	routes     map[string]*routeStats
	switchMode string // "fail" until flipped to "ok"
}

func newState() *state {
	return &state{routes: map[string]*routeStats{}, switchMode: "fail"}
}

// record notes one request against a route and returns whether the event id had
// been seen before on that route.
func (s *state) record(route, eventID string, attempt int) (duplicate bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, ok := s.routes[route]
	if !ok {
		rs = &routeStats{ByStatus: map[string]int{}, seen: map[string]int{}}
		s.routes[route] = rs
	}
	rs.Requests++
	if attempt > rs.MaxAttemptSeen {
		rs.MaxAttemptSeen = attempt
	}
	if eventID != "" {
		rs.seen[eventID]++
		if rs.seen[eventID] > 1 {
			rs.Duplicates++
			duplicate = true
		}
		rs.UniqueEvents = len(rs.seen)
	}
	return duplicate
}

// finish records the status a route answered with.
func (s *state) finish(route string, status int, accepted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, ok := s.routes[route]
	if !ok {
		return
	}
	rs.ByStatus[strconv.Itoa(status)]++
	if accepted {
		rs.Accepted++
	} else {
		rs.Rejected++
	}
}

// begin reads and drains the webhook, recording it against the route.
func (s *state) begin(route string, r *http.Request) (body []byte, eventID string, attempt int) {
	body, _ = io.ReadAll(io.LimitReader(r.Body, maxBody))
	eventID = r.Header.Get(headerID)
	attempt, _ = strconv.Atoi(r.Header.Get(headerAttempt))
	s.record(route, eventID, attempt)
	return body, eventID, attempt
}

func (s *state) handleOK(w http.ResponseWriter, r *http.Request) {
	_, id, attempt := s.begin("/ok", r)
	s.finish("/ok", http.StatusOK, true)
	slog.Debug("ok", "event_id", id, "attempt", attempt)
	writeJSON(w, http.StatusOK, map[string]any{"received": true, "event_id": id})
}

// handleFlaky fails with probability rate (default 0.5). This is what proves
// retries eventually win against an intermittently broken endpoint.
func (s *state) handleFlaky(w http.ResponseWriter, r *http.Request) {
	_, id, attempt := s.begin("/flaky", r)

	rate := 0.5
	if raw := r.URL.Query().Get("rate"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v >= 0 && v <= 1 {
			rate = v
		}
	}
	if rand.Float64() < rate {
		s.finish("/flaky", http.StatusInternalServerError, false)
		slog.Debug("flaky failing", "event_id", id, "attempt", attempt, "rate", rate)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "simulated downstream failure", "rate": rate,
		})
		return
	}
	s.finish("/flaky", http.StatusOK, true)
	writeJSON(w, http.StatusOK, map[string]any{"received": true, "event_id": id})
}

// handleSlow sleeps past the sender's timeout so a timeout path is exercised.
func (s *state) handleSlow(w http.ResponseWriter, r *http.Request) {
	_, id, _ := s.begin("/slow", r)

	delay := 15 * time.Second
	if raw := r.URL.Query().Get("ms"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 0 && v <= 120000 {
			delay = time.Duration(v) * time.Millisecond
		}
	}
	select {
	case <-time.After(delay):
	case <-r.Context().Done():
		// The sender gave up first, which is the point of this route.
		s.finish("/slow", 499, false)
		slog.Debug("slow: caller timed out", "event_id", id, "waited", delay.String())
		return
	}
	s.finish("/slow", http.StatusOK, true)
	writeJSON(w, http.StatusOK, map[string]any{"received": true, "slept_ms": delay.Milliseconds()})
}

// handleFail always fails, so deliveries against it exhaust their retries and
// land in the dead-letter queue.
func (s *state) handleFail(w http.ResponseWriter, r *http.Request) {
	_, id, attempt := s.begin("/fail", r)

	code := http.StatusInternalServerError
	if raw := r.URL.Query().Get("code"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 400 && v <= 599 {
			code = v
		}
	}
	s.finish("/fail", code, false)
	slog.Debug("fail", "event_id", id, "attempt", attempt, "code", code)
	writeJSON(w, code, map[string]any{"error": "this endpoint always fails", "event_id": id})
}

// handleSwitch fails until /_control flips it, which is how the replay test
// simulates "the receiver got fixed".
func (s *state) handleSwitch(w http.ResponseWriter, r *http.Request) {
	_, id, attempt := s.begin("/switch", r)

	s.mu.Lock()
	mode := s.switchMode
	s.mu.Unlock()

	if mode == "ok" {
		s.finish("/switch", http.StatusOK, true)
		writeJSON(w, http.StatusOK, map[string]any{"received": true, "event_id": id, "mode": mode})
		return
	}
	s.finish("/switch", http.StatusServiceUnavailable, false)
	slog.Debug("switch failing", "event_id", id, "attempt", attempt)
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "endpoint is broken", "mode": mode})
}

// handleVerify performs full signature verification and rejects anything that
// does not check out — the reference implementation for a real subscriber.
func (s *state) handleVerify(w http.ResponseWriter, r *http.Request) {
	body, id, _ := s.begin("/verify", r)

	secret := r.URL.Query().Get("secret")
	if secret == "" {
		secret = os.Getenv("WEBHOOK_SECRET")
	}
	if secret == "" {
		s.finish("/verify", http.StatusInternalServerError, false)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "no secret configured; pass ?secret=whsec_... or set WEBHOOK_SECRET",
		})
		return
	}

	if err := verify(secret, id, r.Header.Get(headerSignature), r.Header.Get(headerTimestamp), body, time.Now()); err != nil {
		s.finish("/verify", http.StatusUnauthorized, false)
		slog.Warn("signature rejected", "event_id", id, "reason", err.Error())
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error(), "verified": false})
		return
	}
	s.finish("/verify", http.StatusOK, true)
	writeJSON(w, http.StatusOK, map[string]any{"verified": true, "event_id": id})
}

func (s *state) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := map[string]any{"switch_mode": s.switchMode, "routes": map[string]routeStats{}}
	routes := out["routes"].(map[string]routeStats)
	for name, rs := range s.routes {
		routes[name] = *rs
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *state) handleControl(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Switch string `json:"switch"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Switch != "ok" && req.Switch != "fail" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": `switch must be "ok" or "fail"`})
		return
	}
	s.mu.Lock()
	s.switchMode = req.Switch
	s.mu.Unlock()
	slog.Info("switch mode changed", "mode", req.Switch)
	writeJSON(w, http.StatusOK, map[string]any{"switch_mode": req.Switch})
}

func (s *state) handleReset(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.routes = map[string]*routeStats{}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

// verify checks the timestamp freshness and then the HMAC. This is exactly what
// a real subscriber should do, in this order: a stale request is refused before
// any signature work.
func verify(secret, id, sigHeader, tsHeader string, body []byte, now time.Time) error {
	if id == "" {
		return fmt.Errorf("missing %s header", headerID)
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(tsHeader), 10, 64)
	if err != nil {
		return fmt.Errorf("missing or malformed %s header", headerTimestamp)
	}
	skew := now.Sub(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > toleranceWindow {
		return fmt.Errorf("timestamp is %s old, outside the %s tolerance window", skew.Round(time.Second), toleranceWindow)
	}
	if sigHeader == "" {
		return fmt.Errorf("missing %s header", headerSignature)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s.%d.", id, ts)
	mac.Write(body)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))

	// The header may carry several signatures during a secret rotation.
	for _, got := range strings.Fields(sigHeader) {
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return nil
		}
	}
	return errors.New("signature does not match")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "error", err)
	}
}
