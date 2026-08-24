"use client";

import {
  Area,
  AreaChart,
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { TimeseriesPoint } from "@/lib/types";

const AXIS = "#8b93a7";
const GRID = "#252932";

function timeLabel(iso: string, bucketSeconds: number): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  // Sub-minute buckets need seconds, or every tick reads the same.
  const opts: Intl.DateTimeFormatOptions =
    bucketSeconds < 60
      ? { hour: "2-digit", minute: "2-digit", second: "2-digit" }
      : { hour: "2-digit", minute: "2-digit" };
  return d.toLocaleTimeString(undefined, opts);
}

interface Row {
  t: string;
  attempts: number;
  succeeded: number;
  failed: number;
  successRate: number | null;
  p95: number | null;
}

function toRows(points: TimeseriesPoint[], bucketSeconds: number): Row[] {
  const perMinute = 60 / Math.max(bucketSeconds, 1);
  return points.map((p) => ({
    t: timeLabel(p.bucket, bucketSeconds),
    // Normalise to per-minute so the axis means the same thing whatever the
    // bucket size is.
    attempts: Math.round(p.attempts * perMinute),
    succeeded: Math.round(p.succeeded * perMinute),
    failed: Math.round(p.failed * perMinute),
    successRate: p.succeeded + p.failed > 0 ? p.success_rate * 100 : null,
    p95: p.p95_latency_ms,
  }));
}

const tooltipStyle = {
  backgroundColor: "#12141a",
  border: "1px solid #252932",
  borderRadius: 8,
  fontSize: 12,
  color: "#e8eaf0",
} as const;

function ChartFrame({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="card p-4">
      <h3 className="mb-3 text-xs font-semibold uppercase tracking-wide text-muted">{title}</h3>
      <div className="h-48">{children}</div>
    </div>
  );
}

export function DeliveryRateChart({
  points,
  bucketSeconds,
}: {
  points: TimeseriesPoint[];
  bucketSeconds: number;
}) {
  const rows = toRows(points, bucketSeconds);
  return (
    <ChartFrame title="Deliveries per minute">
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={rows} margin={{ top: 4, right: 12, left: 0, bottom: 0 }}>
          <defs>
            <linearGradient id="gradOk" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="#3ecf8e" stopOpacity={0.45} />
              <stop offset="100%" stopColor="#3ecf8e" stopOpacity={0.02} />
            </linearGradient>
            <linearGradient id="gradFail" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="#f2544b" stopOpacity={0.45} />
              <stop offset="100%" stopColor="#f2544b" stopOpacity={0.02} />
            </linearGradient>
          </defs>
          <CartesianGrid stroke={GRID} strokeDasharray="3 3" vertical={false} />
          <XAxis dataKey="t" stroke={AXIS} tick={{ fontSize: 11 }} tickLine={false} />
          <YAxis stroke={AXIS} tick={{ fontSize: 11 }} tickLine={false} axisLine={false} width={44} />
          <Tooltip contentStyle={tooltipStyle} />
          <Area
            type="monotone"
            dataKey="succeeded"
            name="succeeded/min"
            stroke="#3ecf8e"
            fill="url(#gradOk)"
            strokeWidth={2}
            stackId="1"
          />
          <Area
            type="monotone"
            dataKey="failed"
            name="failed/min"
            stroke="#f2544b"
            fill="url(#gradFail)"
            strokeWidth={2}
            stackId="1"
          />
        </AreaChart>
      </ResponsiveContainer>
    </ChartFrame>
  );
}

export function SuccessRateChart({
  points,
  bucketSeconds,
}: {
  points: TimeseriesPoint[];
  bucketSeconds: number;
}) {
  const rows = toRows(points, bucketSeconds);
  return (
    <ChartFrame title="Success rate">
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={rows} margin={{ top: 4, right: 12, left: 0, bottom: 0 }}>
          <CartesianGrid stroke={GRID} strokeDasharray="3 3" vertical={false} />
          <XAxis dataKey="t" stroke={AXIS} tick={{ fontSize: 11 }} tickLine={false} />
          <YAxis
            stroke={AXIS}
            tick={{ fontSize: 11 }}
            tickLine={false}
            axisLine={false}
            domain={[0, 100]}
            unit="%"
            width={48}
          />
          <Tooltip contentStyle={tooltipStyle} formatter={(v) => `${Number(v).toFixed(1)}%`} />
          <Line
            type="monotone"
            dataKey="successRate"
            name="success rate"
            stroke="#5b8cff"
            strokeWidth={2}
            dot={false}
            connectNulls
          />
        </LineChart>
      </ResponsiveContainer>
    </ChartFrame>
  );
}

export function LatencyChart({
  points,
  bucketSeconds,
}: {
  points: TimeseriesPoint[];
  bucketSeconds: number;
}) {
  const rows = toRows(points, bucketSeconds);
  return (
    <ChartFrame title="p95 delivery latency">
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={rows} margin={{ top: 4, right: 12, left: 0, bottom: 0 }}>
          <CartesianGrid stroke={GRID} strokeDasharray="3 3" vertical={false} />
          <XAxis dataKey="t" stroke={AXIS} tick={{ fontSize: 11 }} tickLine={false} />
          <YAxis
            stroke={AXIS}
            tick={{ fontSize: 11 }}
            tickLine={false}
            axisLine={false}
            unit="ms"
            width={56}
          />
          <Tooltip contentStyle={tooltipStyle} formatter={(v) => `${v} ms`} />
          <Line
            type="monotone"
            dataKey="p95"
            name="p95 latency"
            stroke="#f5b13d"
            strokeWidth={2}
            dot={false}
            connectNulls
          />
        </LineChart>
      </ResponsiveContainer>
    </ChartFrame>
  );
}
