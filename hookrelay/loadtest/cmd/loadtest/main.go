// Command loadtest ingests a large batch of events into HookRelay and then
// accounts for every single delivery.
//
// The point is not raw throughput — it is the zero-loss assertion. The tool
// records exactly how many deliveries ingestion created, waits for the pipeline
// to drain, and then proves that every one of them reached a terminal state.
// Nothing may be pending, and nothing may have vanished.
//
// It is written in Go rather than as a k6 script so the repo needs no extra
// runtime: `go run ./cmd/loadtest` works with the toolchain already required to
// build the backend. An equivalent k6 script lives at loadtest/k6/ingest.js for
// anyone who prefers it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nloadtest failed: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	apiURL      string
	receiverURL string
	events      int
	concurrency int
	settle      time.Duration
	flakyRate   float64
	slowMS      int
}

func run() error {
	cfg := config{}
	flag.StringVar(&cfg.apiURL, "api", env("API_URL", "http://localhost:8080"), "HookRelay API base URL")
	flag.StringVar(&cfg.receiverURL, "receiver", env("RECEIVER_URL", "http://localhost:9090"), "test receiver base URL")
	flag.IntVar(&cfg.events, "events", 10000, "number of events to publish")
	flag.IntVar(&cfg.concurrency, "concurrency", 50, "concurrent publishers")
	flag.DurationVar(&cfg.settle, "settle", 10*time.Minute, "how long to wait for the pipeline to drain")
	flag.Float64Var(&cfg.flakyRate, "flaky-rate", 0.3, "failure rate of the flaky endpoint")
	flag.IntVar(&cfg.slowMS, "slow-ms", 15000, "delay of the slow endpoint in ms")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := newClient(cfg.apiURL)

	fmt.Printf("HookRelay load test\n")
	fmt.Printf("  api          %s\n", cfg.apiURL)
	fmt.Printf("  receiver     %s\n", cfg.receiverURL)
	fmt.Printf("  events       %d\n", cfg.events)
	fmt.Printf("  publishers   %d\n\n", cfg.concurrency)

	if err := c.waitHealthy(ctx); err != nil {
		return err
	}

	// A dedicated tenant makes every count in the report unambiguous.
	email := fmt.Sprintf("loadtest-%d@hookrelay.test", time.Now().UnixNano())
	key, err := c.register(ctx, "Load Test", email, "loadtest-password-123")
	if err != nil {
		return fmt.Errorf("register tenant: %w", err)
	}
	c.apiKey = key

	// Three endpoints with deliberately different health, so the run exercises
	// the happy path, the retry path and the timeout path at once.
	targets := []struct {
		url  string
		desc string
	}{
		{cfg.receiverURL + "/ok", "healthy"},
		{fmt.Sprintf("%s/flaky?rate=%g", cfg.receiverURL, cfg.flakyRate), "flaky"},
		{fmt.Sprintf("%s/slow?ms=%d", cfg.receiverURL, cfg.slowMS), "slow"},
	}
	endpointIDs := make(map[string]string, len(targets))
	for _, t := range targets {
		id, err := c.createEndpoint(ctx, t.url, t.desc, []string{"load.test"})
		if err != nil {
			return fmt.Errorf("create %s endpoint: %w", t.desc, err)
		}
		endpointIDs[t.desc] = id
		fmt.Printf("  endpoint %-8s %s\n", t.desc, t.url)
	}
	healthyID := endpointIDs["healthy"]
	fmt.Println()

	// ---- ingestion ----
	fmt.Printf("Publishing %d events with %d publishers...\n", cfg.events, cfg.concurrency)
	ing, err := c.publishAll(ctx, cfg)
	if err != nil {
		return err
	}

	fmt.Printf("\nIngestion\n")
	fmt.Printf("  published        %d ok, %d failed\n", ing.ok, ing.failed)
	fmt.Printf("  wall clock       %s\n", ing.elapsed.Round(time.Millisecond))
	fmt.Printf("  throughput       %.0f events/sec\n", float64(ing.ok)/ing.elapsed.Seconds())
	fmt.Printf("  latency p50      %s\n", pct(ing.latencies, 0.50).Round(time.Microsecond))
	fmt.Printf("  latency p95      %s\n", pct(ing.latencies, 0.95).Round(time.Microsecond))
	fmt.Printf("  latency p99      %s\n", pct(ing.latencies, 0.99).Round(time.Microsecond))
	fmt.Printf("  deliveries made  %d  (expected %d = %d events x %d endpoints)\n",
		ing.deliveries, ing.ok*len(targets), ing.ok, len(targets))

	if ing.failed > 0 {
		return fmt.Errorf("%d publishes failed; aborting before the accounting phase", ing.failed)
	}
	if ing.deliveries != ing.ok*len(targets) {
		return fmt.Errorf("fan-out mismatch: got %d deliveries, expected %d",
			ing.deliveries, ing.ok*len(targets))
	}

	// ---- drain ----
	fmt.Printf("\nWaiting for the pipeline to drain (timeout %s)...\n", cfg.settle)
	final, drained, err := c.waitDrained(ctx, cfg.settle)
	if err != nil {
		return err
	}

	// ---- accounting ----
	total := 0
	for _, n := range final {
		total += n
	}
	open := final["pending"] + final["failed"] + final["delivering"]
	settled := final["succeeded"] + final["dead"]
	lost := ing.deliveries - total

	fmt.Printf("\nDelivery accounting\n")
	fmt.Printf("  created by ingestion   %d\n", ing.deliveries)
	fmt.Printf("  found in the database  %d\n", total)
	fmt.Printf("  succeeded              %d\n", final["succeeded"])
	fmt.Printf("  dead (DLQ)             %d\n", final["dead"])
	fmt.Printf("  still open             %d  (pending=%d failed=%d delivering=%d)\n",
		open, final["pending"], final["failed"], final["delivering"])
	fmt.Printf("  LOST                   %d\n", lost)

	// End-to-end latency for the endpoint that should never fail.
	e2e, err := c.endpointStats(ctx, healthyID)
	if err == nil {
		fmt.Printf("\nHealthy endpoint (%s)\n", targets[0].url)
		fmt.Printf("  deliveries       %d\n", e2e.Total)
		fmt.Printf("  success rate     %.2f%%\n", e2e.SuccessRate*100)
		fmt.Printf("  attempt p95      %s\n", msString(e2e.P95LatencyMS))
		fmt.Printf("  attempt avg      %s\n", msString(e2e.AvgLatencyMS))
	}
	if e2e2, err := c.endToEnd(ctx, healthyID); err == nil {
		fmt.Printf("  end-to-end p95   %s  (publish -> delivered)\n", e2e2.p95.Round(time.Millisecond))
		fmt.Printf("  end-to-end p50   %s\n", e2e2.p50.Round(time.Millisecond))
	}

	// Attempt-outcome mix explains *how* the pipeline settled: a mass of skips
	// means the circuit breaker did its job instead of tying up workers.
	if mix, err := c.attemptMix(ctx); err == nil {
		fmt.Printf("\nAttempt outcomes (all endpoints)\n")
		fmt.Printf("  succeeded        %d\n", mix.succeeded)
		fmt.Printf("  failed           %d\n", mix.failed)
		fmt.Printf("  skipped          %d  (circuit breaker open — no HTTP call made)\n", mix.skipped)
		fmt.Printf("  total attempts   %d\n", mix.total)
	}

	fmt.Printf("\nAssertions\n")
	fails := 0
	assert := func(ok bool, format string, args ...any) {
		label := "\033[32mPASS\033[0m"
		if !ok {
			label = "\033[31mFAIL\033[0m"
			fails++
		}
		fmt.Printf("  %s %s\n", label, fmt.Sprintf(format, args...))
	}
	assert(ing.ok == cfg.events, "ingested %d of %d events", ing.ok, cfg.events)
	assert(lost == 0, "zero lost deliveries (created %d, accounted for %d)", ing.deliveries, total)
	assert(drained, "queue drained within %s", cfg.settle)
	assert(open == 0, "no delivery stuck in a non-terminal state (%d open)", open)
	assert(settled == ing.deliveries, "every delivery settled (%d of %d)", settled, ing.deliveries)
	// The healthy endpoint must never dead-letter: if it did, the pipeline gave
	// up on a target that was always answering 200.
	if e2e.Total > 0 {
		assert(e2e.Dead == 0, "healthy endpoint dead-lettered nothing (%d dead)", e2e.Dead)
	}

	if fails > 0 {
		return fmt.Errorf("%d assertion(s) failed", fails)
	}
	fmt.Printf("\n  \033[32mALL ASSERTIONS PASSED — zero lost deliveries\033[0m\n\n")
	return nil
}

// ----- client -----

type client struct {
	base   string
	apiKey string
	http   *http.Client
}

func newClient(base string) *client {
	return &client{
		base: base,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        512,
				MaxIdleConnsPerHost: 256,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (c *client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, snippet)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

func (c *client) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := c.do(ctx, http.MethodGet, "/healthz", nil, nil)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("api never became healthy: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (c *client) register(ctx context.Context, name, email, password string) (string, error) {
	var out struct {
		APIKey string `json:"api_key"`
	}
	err := c.do(ctx, http.MethodPost, "/auth/register",
		map[string]string{"name": name, "email": email, "password": password}, &out)
	if err != nil {
		return "", err
	}
	return out.APIKey, nil
}

func (c *client) createEndpoint(ctx context.Context, url, desc string, types []string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, "/endpoints", map[string]any{
		"url": url, "description": desc, "event_types": types,
	}, &out)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

type ingestResult struct {
	ok         int
	failed     int
	deliveries int
	elapsed    time.Duration
	latencies  []time.Duration
}

// publishAll fans the publishes out over cfg.concurrency goroutines and records
// per-request latency plus how many deliveries the API says it created.
func (c *client) publishAll(ctx context.Context, cfg config) (*ingestResult, error) {
	type sample struct {
		latency    time.Duration
		deliveries int
		err        error
	}

	jobs := make(chan int, cfg.concurrency*2)
	results := make(chan sample, cfg.concurrency*2)

	var wg sync.WaitGroup
	for range cfg.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range jobs {
				start := time.Now()
				var out struct {
					Deliveries int `json:"deliveries"`
				}
				err := c.do(ctx, http.MethodPost, "/events", map[string]any{
					"event_type": "load.test",
					"payload": map[string]any{
						"n":       n,
						"order":   fmt.Sprintf("ord_%06d", n),
						"amount":  1000 + rand.IntN(9000),
						"created": time.Now().UnixMilli(),
					},
				}, &out)
				results <- sample{latency: time.Since(start), deliveries: out.Deliveries, err: err}
			}
		}()
	}

	var progress atomic.Int64
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fmt.Printf("\r  published %d/%d   ", progress.Load(), cfg.events)
			}
		}
	}()

	start := time.Now()
	go func() {
		defer close(jobs)
		for n := 1; n <= cfg.events; n++ {
			select {
			case jobs <- n:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	res := &ingestResult{latencies: make([]time.Duration, 0, cfg.events)}
	var firstErr error
	for s := range results {
		progress.Add(1)
		if s.err != nil {
			res.failed++
			if firstErr == nil {
				firstErr = s.err
			}
			continue
		}
		res.ok++
		res.deliveries += s.deliveries
		res.latencies = append(res.latencies, s.latency)
	}
	close(done)
	res.elapsed = time.Since(start)
	fmt.Printf("\r  published %d/%d   \n", res.ok, cfg.events)

	if res.failed > 0 && firstErr != nil {
		fmt.Printf("  first publish error: %v\n", firstErr)
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, nil
}

// waitDrained polls the delivery counts until nothing is open, printing progress.
func (c *client) waitDrained(ctx context.Context, timeout time.Duration) (map[string]int, bool, error) {
	deadline := time.Now().Add(timeout)
	var last map[string]int
	for {
		counts, err := c.counts(ctx)
		if err != nil {
			return nil, false, err
		}
		last = counts
		open := counts["pending"] + counts["failed"] + counts["delivering"]
		settled := counts["succeeded"] + counts["dead"]
		fmt.Printf("\r  open=%-7d settled=%-7d succeeded=%-7d dead=%-6d   ",
			open, settled, counts["succeeded"], counts["dead"])
		if open == 0 {
			fmt.Println()
			return counts, true, nil
		}
		if time.Now().After(deadline) {
			fmt.Println()
			return counts, false, nil
		}
		select {
		case <-ctx.Done():
			fmt.Println()
			return last, false, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func (c *client) counts(ctx context.Context) (map[string]int, error) {
	var out struct {
		Counts map[string]int `json:"counts"`
	}
	if err := c.do(ctx, http.MethodGet, "/deliveries?limit=1", nil, &out); err != nil {
		return nil, err
	}
	if out.Counts == nil {
		out.Counts = map[string]int{}
	}
	return out.Counts, nil
}

type endpointStats struct {
	Total        int      `json:"total"`
	Succeeded    int      `json:"succeeded"`
	Dead         int      `json:"dead"`
	SuccessRate  float64  `json:"success_rate"`
	AvgLatencyMS *float64 `json:"avg_latency_ms"`
	P95LatencyMS *float64 `json:"p95_latency_ms"`
}

func (c *client) endpointStats(ctx context.Context, endpointID string) (*endpointStats, error) {
	var out endpointStats
	if err := c.do(ctx, http.MethodGet, "/endpoints/"+endpointID+"/stats?window_hours=24", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type e2eStats struct {
	p50, p95 time.Duration
}

// endToEnd measures publish-to-delivered latency for one endpoint by sampling
// deliveries and diffing created_at against completed_at.
func (c *client) endToEnd(ctx context.Context, endpointID string) (*e2eStats, error) {
	var out struct {
		Deliveries []struct {
			Status      string     `json:"status"`
			CreatedAt   time.Time  `json:"created_at"`
			CompletedAt *time.Time `json:"completed_at"`
		} `json:"deliveries"`
	}
	path := fmt.Sprintf("/deliveries?endpoint_id=%s&status=succeeded&limit=500", endpointID)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	var samples []time.Duration
	for _, d := range out.Deliveries {
		if d.CompletedAt == nil {
			continue
		}
		samples = append(samples, d.CompletedAt.Sub(d.CreatedAt))
	}
	if len(samples) == 0 {
		return nil, errors.New("no completed deliveries to measure")
	}
	return &e2eStats{p50: pct(samples, 0.50), p95: pct(samples, 0.95)}, nil
}

type attemptMix struct{ total, succeeded, failed, skipped int }

// attemptMix sums the attempt outcomes over a wide window.
func (c *client) attemptMix(ctx context.Context) (*attemptMix, error) {
	var out struct {
		Points []struct {
			Attempts  int `json:"attempts"`
			Succeeded int `json:"succeeded"`
			Failed    int `json:"failed"`
			Skipped   int `json:"skipped"`
		} `json:"points"`
	}
	if err := c.do(ctx, http.MethodGet, "/stats/timeseries?window_hours=24&bucket_seconds=3600", nil, &out); err != nil {
		return nil, err
	}
	m := &attemptMix{}
	for _, p := range out.Points {
		m.total += p.Attempts
		m.succeeded += p.Succeeded
		m.failed += p.Failed
		m.skipped += p.Skipped
	}
	return m, nil
}

// ----- helpers -----

// pct returns the p-th percentile using nearest-rank on a copy of the samples.
func pct(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func msString(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.0fms", *v)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
