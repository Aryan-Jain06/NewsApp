"use client";

import { api } from "@/lib/api";
import { latency, percent } from "@/lib/format";
import { usePolling } from "@/lib/usePolling";
import { DeliveryRateChart, LatencyChart, SuccessRateChart } from "@/components/Charts";
import { Shell } from "@/components/Shell";
import { ErrorNote, StatCard } from "@/components/ui";

/** Overview polls every 5s, which is fast enough to watch a load test run. */
export default function OverviewPage() {
  const overview = usePolling(() => api.overview(24), 5000);
  const series = usePolling(() => api.timeseries(1, 15), 5000);

  const o = overview.data;
  const byStatus = o?.deliveries_by_status ?? {};

  return (
    <Shell
      title="Overview"
      subtitle="Delivery health across every endpoint, last 24 hours. Refreshes every 5s."
    >
      {overview.error ? <ErrorNote message={overview.error} /> : null}

      <div className="grid grid-cols-2 gap-4 lg:grid-cols-6">
        <StatCard label="Events" value={o?.events ?? "—"} hint="published in 24h" />
        <StatCard
          label="Succeeded"
          value={byStatus.succeeded ?? 0}
          tone="ok"
          hint="delivered 2xx"
        />
        <StatCard
          label="Retrying"
          value={(byStatus.failed ?? 0) + (byStatus.pending ?? 0) + (byStatus.delivering ?? 0)}
          tone="warn"
          hint="in flight or backing off"
        />
        <StatCard label="Dead" value={o?.dead_count ?? 0} tone="dead" hint="awaiting replay" />
        <StatCard
          label="Success rate"
          value={percent(o?.success_rate)}
          hint="of settled deliveries"
        />
        <StatCard
          label="p95 latency"
          value={latency(o?.p95_latency_ms)}
          hint="endpoint response time"
        />
      </div>

      <div className="mt-4 grid gap-4 lg:grid-cols-3">
        {series.error ? (
          <div className="lg:col-span-3">
            <ErrorNote message={series.error} />
          </div>
        ) : null}
        <DeliveryRateChart
          points={series.data?.points ?? []}
          bucketSeconds={series.data?.bucket_seconds ?? 15}
        />
        <SuccessRateChart
          points={series.data?.points ?? []}
          bucketSeconds={series.data?.bucket_seconds ?? 15}
        />
        <LatencyChart
          points={series.data?.points ?? []}
          bucketSeconds={series.data?.bucket_seconds ?? 15}
        />
      </div>

      <div className="mt-4 grid gap-4 lg:grid-cols-2">
        <StatCard
          label="Endpoints"
          value={`${o?.active_endpoints ?? 0} active / ${o?.endpoints ?? 0} total`}
        />
        <StatCard
          label="Attempts charted"
          value={(series.data?.points ?? []).reduce((n, p) => n + p.attempts, 0)}
          hint="last hour"
        />
      </div>
    </Shell>
  );
}
