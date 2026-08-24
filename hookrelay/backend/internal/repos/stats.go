package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// StatsRepo serves the aggregate queries behind the dashboard charts.
type StatsRepo struct{ q Querier }

// NewStatsRepo builds a StatsRepo over any Querier.
func NewStatsRepo(q Querier) *StatsRepo { return &StatsRepo{q: q} }

// EndpointStats summarises one endpoint's recent health.
type EndpointStats struct {
	EndpointID    uuid.UUID `json:"endpoint_id"`
	WindowHours   int       `json:"window_hours"`
	Total         int       `json:"total"`
	Succeeded     int       `json:"succeeded"`
	Failed        int       `json:"failed"`
	Dead          int       `json:"dead"`
	Pending       int       `json:"pending"`
	SuccessRate   float64   `json:"success_rate"`
	AvgLatencyMS  *float64  `json:"avg_latency_ms"`
	P95LatencyMS  *float64  `json:"p95_latency_ms"`
	TotalAttempts int       `json:"total_attempts"`
}

// ForEndpoint computes delivery outcome counts and attempt latencies for an
// endpoint over the trailing window.
func (r *StatsRepo) ForEndpoint(ctx context.Context, tenantID, endpointID uuid.UUID, window time.Duration) (*EndpointStats, error) {
	since := time.Now().Add(-window)
	out := &EndpointStats{EndpointID: endpointID, WindowHours: int(window.Hours())}

	const outcomeQ = `
		SELECT
			count(*)                                                        AS total,
			count(*) FILTER (WHERE status = 'succeeded')                    AS succeeded,
			count(*) FILTER (WHERE status = 'failed')                       AS failed,
			count(*) FILTER (WHERE status = 'dead')                         AS dead,
			count(*) FILTER (WHERE status IN ('pending', 'delivering'))     AS pending
		FROM deliveries
		WHERE tenant_id = $1 AND endpoint_id = $2 AND created_at >= $3`
	err := r.q.QueryRow(ctx, outcomeQ, tenantID, endpointID, since).
		Scan(&out.Total, &out.Succeeded, &out.Failed, &out.Dead, &out.Pending)
	if err != nil {
		return nil, fmt.Errorf("endpoint outcome stats: %w", mapErr(err))
	}
	// Success rate is measured over settled deliveries only; counting still-
	// retrying rows as failures would make a healthy endpoint look broken.
	settled := out.Succeeded + out.Dead
	if settled > 0 {
		out.SuccessRate = float64(out.Succeeded) / float64(settled)
	}

	const latencyQ = `
		SELECT
			count(*),
			avg(a.response_ms)::float8,
			percentile_disc(0.95) WITHIN GROUP (ORDER BY a.response_ms)::float8
		FROM delivery_attempts a
		JOIN deliveries d ON d.id = a.delivery_id
		WHERE d.tenant_id = $1 AND d.endpoint_id = $2
		  AND a.attempted_at >= $3 AND a.response_ms IS NOT NULL`
	if err := r.q.QueryRow(ctx, latencyQ, tenantID, endpointID, since).
		Scan(&out.TotalAttempts, &out.AvgLatencyMS, &out.P95LatencyMS); err != nil {
		return nil, fmt.Errorf("endpoint latency stats: %w", mapErr(err))
	}
	return out, nil
}

// Overview is the tenant-wide headline panel.
type Overview struct {
	WindowHours     int            `json:"window_hours"`
	Endpoints       int            `json:"endpoints"`
	ActiveEndpoints int            `json:"active_endpoints"`
	Events          int            `json:"events"`
	Deliveries      map[string]int `json:"deliveries_by_status"`
	SuccessRate     float64        `json:"success_rate"`
	P95LatencyMS    *float64       `json:"p95_latency_ms"`
	DeadCount       int            `json:"dead_count"`
}

// TenantOverview computes the dashboard's headline numbers.
func (r *StatsRepo) TenantOverview(ctx context.Context, tenantID uuid.UUID, window time.Duration) (*Overview, error) {
	since := time.Now().Add(-window)
	out := &Overview{WindowHours: int(window.Hours()), Deliveries: map[string]int{}}

	const q = `
		SELECT
			(SELECT count(*) FROM endpoints WHERE tenant_id = $1),
			(SELECT count(*) FROM endpoints WHERE tenant_id = $1 AND active),
			(SELECT count(*) FROM events    WHERE tenant_id = $1 AND created_at >= $2),
			(SELECT count(*) FROM deliveries WHERE tenant_id = $1 AND status = 'dead')`
	if err := r.q.QueryRow(ctx, q, tenantID, since).
		Scan(&out.Endpoints, &out.ActiveEndpoints, &out.Events, &out.DeadCount); err != nil {
		return nil, fmt.Errorf("overview counts: %w", mapErr(err))
	}

	rows, err := r.q.Query(ctx, `
		SELECT status::text, count(*)
		FROM deliveries
		WHERE tenant_id = $1 AND created_at >= $2
		GROUP BY status`, tenantID, since)
	if err != nil {
		return nil, fmt.Errorf("overview status counts: %w", err)
	}
	defer rows.Close()
	var succeeded, dead int
	for rows.Next() {
		var (
			s string
			n int
		)
		if err := rows.Scan(&s, &n); err != nil {
			return nil, fmt.Errorf("scan overview status: %w", err)
		}
		out.Deliveries[s] = n
		switch s {
		case "succeeded":
			succeeded = n
		case "dead":
			dead = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if succeeded+dead > 0 {
		out.SuccessRate = float64(succeeded) / float64(succeeded+dead)
	}

	const p95Q = `
		SELECT percentile_disc(0.95) WITHIN GROUP (ORDER BY a.response_ms)::float8
		FROM delivery_attempts a
		JOIN deliveries d ON d.id = a.delivery_id
		WHERE d.tenant_id = $1 AND a.attempted_at >= $2 AND a.response_ms IS NOT NULL`
	if err := r.q.QueryRow(ctx, p95Q, tenantID, since).Scan(&out.P95LatencyMS); err != nil {
		return nil, fmt.Errorf("overview p95: %w", mapErr(err))
	}
	return out, nil
}

// TimeseriesPoint is one bucket of the dashboard charts.
type TimeseriesPoint struct {
	Bucket       time.Time `json:"bucket"`
	Attempts     int       `json:"attempts"`
	Succeeded    int       `json:"succeeded"`
	Failed       int       `json:"failed"`
	Skipped      int       `json:"skipped"`
	SuccessRate  float64   `json:"success_rate"`
	P95LatencyMS *float64  `json:"p95_latency_ms"`
}

// Timeseries buckets delivery attempts so the dashboard can chart
// deliveries/min, success rate and p95 latency over one window.
func (r *StatsRepo) Timeseries(ctx context.Context, tenantID uuid.UUID, window, bucket time.Duration) ([]TimeseriesPoint, error) {
	since := time.Now().Add(-window)
	const q = `
		SELECT
			to_timestamp(floor(extract(epoch FROM a.attempted_at) / $3) * $3) AS bucket,
			count(*)                                            AS attempts,
			count(*) FILTER (WHERE a.outcome = 'success')       AS succeeded,
			count(*) FILTER (WHERE a.outcome = 'failure')       AS failed,
			count(*) FILTER (WHERE a.outcome = 'skipped')       AS skipped,
			percentile_disc(0.95) WITHIN GROUP (ORDER BY a.response_ms)::float8 AS p95
		FROM delivery_attempts a
		JOIN deliveries d ON d.id = a.delivery_id
		WHERE d.tenant_id = $1 AND a.attempted_at >= $2
		GROUP BY 1
		ORDER BY 1`
	rows, err := r.q.Query(ctx, q, tenantID, since, bucket.Seconds())
	if err != nil {
		return nil, fmt.Errorf("timeseries: %w", err)
	}
	defer rows.Close()

	out := []TimeseriesPoint{}
	for rows.Next() {
		var p TimeseriesPoint
		if err := rows.Scan(&p.Bucket, &p.Attempts, &p.Succeeded, &p.Failed, &p.Skipped, &p.P95LatencyMS); err != nil {
			return nil, fmt.Errorf("scan timeseries point: %w", err)
		}
		if settled := p.Succeeded + p.Failed; settled > 0 {
			p.SuccessRate = float64(p.Succeeded) / float64(settled)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
