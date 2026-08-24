"use client";

import { useState } from "react";
import { api } from "@/lib/api";
import { absoluteTime, countdown, latency, relativeTime } from "@/lib/format";
import type { Delivery } from "@/lib/types";
import { useNow } from "@/lib/usePolling";
import { EndpointLink, Mono, OutcomeDot, StatusBadge } from "./ui";

/**
 * DeliveryTimeline renders one endpoint's full attempt history for an event:
 * every try with its status code and latency, plus a live countdown to the next
 * retry and a replay button.
 */
export function DeliveryTimeline({
  delivery,
  onChanged,
}: {
  delivery: Delivery;
  onChanged: () => void;
}) {
  const now = useNow(1000);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const replay = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.replayDelivery(delivery.id);
      onChanged();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const attempts = delivery.attempts ?? [];
  const retryDue = delivery.status === "failed" && delivery.next_attempt_at;

  return (
    <div className="card overflow-hidden">
      <header className="flex flex-wrap items-center gap-3 border-b border-border px-4 py-3">
        <StatusBadge status={delivery.status} />
        <EndpointLink id={delivery.endpoint_id}>
          <span className="font-mono text-xs">{delivery.endpoint_url ?? delivery.endpoint_id}</span>
        </EndpointLink>
        <span className="text-xs text-muted">
          {attempts.length} attempt{attempts.length === 1 ? "" : "s"}
        </span>
        {retryDue ? (
          <span className="rounded-md border border-warn/30 bg-warn/10 px-2 py-0.5 text-xs text-warn tabular-nums">
            next retry {countdown(delivery.next_attempt_at)}
          </span>
        ) : null}
        <div className="ml-auto flex items-center gap-2">
          {error ? <span className="text-xs text-danger">{error}</span> : null}
          <button type="button" className="btn-primary" onClick={replay} disabled={busy}>
            {busy ? "Replaying…" : "Replay"}
          </button>
        </div>
      </header>

      {attempts.length === 0 ? (
        <div className="px-4 py-6 text-sm text-muted">
          No attempt recorded yet — the delivery is queued.
        </div>
      ) : (
        <ol className="divide-y divide-border">
          {attempts.map((a) => (
            <li key={a.id} className="flex items-start gap-3 px-4 py-3">
              <div className="flex w-16 shrink-0 items-center gap-2 pt-0.5">
                <OutcomeDot outcome={a.outcome} />
                <Mono className="text-muted">#{a.attempt_no}</Mono>
              </div>

              <div className="w-20 shrink-0">
                {a.status_code ? (
                  <Mono
                    className={
                      a.status_code >= 200 && a.status_code < 300 ? "text-ok" : "text-danger"
                    }
                  >
                    HTTP {a.status_code}
                  </Mono>
                ) : (
                  <Mono className="text-muted">{a.outcome === "skipped" ? "skipped" : "no reply"}</Mono>
                )}
              </div>

              <div className="w-20 shrink-0">
                <Mono className="text-muted tabular-nums">{latency(a.response_ms)}</Mono>
              </div>

              <div className="min-w-0 flex-1">
                {a.error ? (
                  <p className="break-words font-mono text-xs text-danger/90">{a.error}</p>
                ) : (
                  <p className="text-xs text-muted">delivered</p>
                )}
              </div>

              <div className="w-40 shrink-0 text-right" title={absoluteTime(a.attempted_at)}>
                <Mono className="text-muted">{relativeTime(a.attempted_at)}</Mono>
              </div>
            </li>
          ))}
        </ol>
      )}

      <footer className="flex flex-wrap items-center gap-x-6 gap-y-1 border-t border-border bg-raised/40 px-4 py-2 text-xs text-muted">
        <span>
          delivery <Mono>{delivery.id}</Mono>
        </span>
        <span>created {relativeTime(delivery.created_at)}</span>
        {delivery.completed_at ? <span>settled {relativeTime(delivery.completed_at)}</span> : null}
        {delivery.last_error ? (
          <span className="text-danger/80">last error: {delivery.last_error.slice(0, 120)}</span>
        ) : null}
        <span className="ml-auto tabular-nums" suppressHydrationWarning>
          {new Date(now).toLocaleTimeString()}
        </span>
      </footer>
    </div>
  );
}
