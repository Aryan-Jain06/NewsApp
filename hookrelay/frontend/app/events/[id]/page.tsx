"use client";

import { useParams } from "next/navigation";
import { useState } from "react";
import { api } from "@/lib/api";
import { absoluteTime, relativeTime } from "@/lib/format";
import { usePolling } from "@/lib/usePolling";
import { DeliveryTimeline } from "@/components/DeliveryTimeline";
import { Shell } from "@/components/Shell";
import { Card, Empty, ErrorNote, Mono, Spinner } from "@/components/ui";

/**
 * The event detail page is the debugging surface: one timeline per subscribed
 * endpoint, every attempt with its status code and latency, a live countdown to
 * the next retry, and replay for a single endpoint or the whole event.
 */
export default function EventDetailPage() {
  const params = useParams<{ id: string }>();
  const id = params.id;
  const { data, error, loading, refresh } = usePolling(() => api.getEvent(id), 5000, [id]);
  const [busy, setBusy] = useState(false);
  const [replayError, setReplayError] = useState<string | null>(null);

  const replayAll = async () => {
    setBusy(true);
    setReplayError(null);
    try {
      await api.replayEvent(id);
      refresh();
    } catch (err) {
      setReplayError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const event = data?.event;
  const deliveries = data?.deliveries ?? [];

  return (
    <Shell
      title={<Mono className="text-lg">{id}</Mono>}
      subtitle={
        event
          ? `${event.event_type} · published ${relativeTime(event.created_at)}`
          : "Delivery timeline"
      }
      actions={
        <div className="flex items-center gap-2">
          {replayError ? <span className="text-xs text-danger">{replayError}</span> : null}
          <button type="button" className="btn-primary" onClick={replayAll} disabled={busy}>
            {busy ? "Replaying…" : "Replay all endpoints"}
          </button>
        </div>
      }
    >
      {error ? <ErrorNote message={error} /> : null}
      {loading && !data ? <Spinner label="Loading event" /> : null}

      {event ? (
        <div className="grid gap-4 lg:grid-cols-3">
          <div className="space-y-4 lg:col-span-2">
            {deliveries.length === 0 ? (
              <Card title="Deliveries">
                <Empty>
                  No endpoint was subscribed to <Mono>{event.event_type}</Mono> when this
                  event was published, so nothing was fanned out.
                </Empty>
              </Card>
            ) : (
              deliveries.map((d) => (
                <DeliveryTimeline key={d.id} delivery={d} onChanged={refresh} />
              ))
            )}
          </div>

          <div className="space-y-4">
            <Card title="Event">
              <dl className="divide-y divide-border text-sm">
                <Row label="Type">
                  <Mono>{event.event_type}</Mono>
                </Row>
                <Row label="Published">
                  <span title={absoluteTime(event.created_at)}>
                    {relativeTime(event.created_at)}
                  </span>
                </Row>
                <Row label="Idempotency key">
                  {event.idempotency_key ? <Mono>{event.idempotency_key}</Mono> : "—"}
                </Row>
                <Row label="Endpoints">{deliveries.length}</Row>
                <Row label="Attempts">
                  {deliveries.reduce((n, d) => n + (d.attempts?.length ?? 0), 0)}
                </Row>
              </dl>
            </Card>

            <Card title="Payload">
              <pre className="max-h-96 overflow-auto px-4 py-3 font-mono text-xs leading-relaxed text-ink">
                {JSON.stringify(event.payload, null, 2)}
              </pre>
            </Card>
          </div>
        </div>
      ) : null}
    </Shell>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3 px-4 py-2.5">
      <dt className="text-xs uppercase tracking-wide text-muted">{label}</dt>
      <dd className="text-right">{children}</dd>
    </div>
  );
}
