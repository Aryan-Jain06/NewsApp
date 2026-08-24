"use client";

import { useState } from "react";
import { api } from "@/lib/api";
import { absoluteTime, relativeTime } from "@/lib/format";
import { usePolling } from "@/lib/usePolling";
import { Shell } from "@/components/Shell";
import {
  Card,
  Empty,
  ErrorNote,
  EventLink,
  Mono,
  Spinner,
  StatCard,
} from "@/components/ui";

/**
 * The dead-letter queue: deliveries that exhausted their retries. Rows can be
 * replayed individually, in a selection, or all at once after the underlying
 * endpoint has been fixed.
 */
export default function DlqPage() {
  const { data, error, loading, refresh } = usePolling(
    () => api.listDeliveries({ status: "dead", limit: 200 }),
    5000,
  );
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const rows = data?.deliveries ?? [];
  const counts = data?.counts ?? {};

  const toggle = (id: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const allSelected = rows.length > 0 && rows.every((r) => selected.has(r.id));
  const toggleAll = () =>
    setSelected(allSelected ? new Set() : new Set(rows.map((r) => r.id)));

  const run = async (fn: () => Promise<{ replayed: number }>, label: string) => {
    setBusy(true);
    setActionError(null);
    setNote(null);
    try {
      const res = await fn();
      setNote(`${label}: ${res.replayed} deliver${res.replayed === 1 ? "y" : "ies"} re-queued.`);
      setSelected(new Set());
      refresh();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Shell
      title="Dead letters"
      subtitle="Deliveries that used their whole retry budget. Fix the endpoint, then replay."
      actions={
        <div className="flex items-center gap-2">
          <button
            type="button"
            className="btn-ghost"
            disabled={busy || selected.size === 0}
            onClick={() =>
              run(
                () => api.bulkReplay({ delivery_ids: [...selected] }),
                `Replayed ${selected.size} selected`,
              )
            }
          >
            Replay selected ({selected.size})
          </button>
          <button
            type="button"
            className="btn-primary"
            disabled={busy || rows.length === 0}
            onClick={() => {
              if (!window.confirm(`Replay all ${rows.length} dead deliveries?`)) return;
              void run(() => api.bulkReplay({ status: "dead", limit: 1000 }), "Replayed all dead");
            }}
          >
            {busy ? "Replaying…" : "Replay all dead"}
          </button>
        </div>
      }
    >
      <div className="mb-4 grid grid-cols-2 gap-4 lg:grid-cols-5">
        <StatCard label="Dead" value={counts.dead ?? 0} tone="dead" />
        <StatCard label="Retrying" value={counts.failed ?? 0} tone="warn" />
        <StatCard label="Queued" value={(counts.pending ?? 0) + (counts.delivering ?? 0)} />
        <StatCard label="Succeeded" value={counts.succeeded ?? 0} tone="ok" />
        <StatCard
          label="Total"
          value={Object.values(counts).reduce<number>((n, v) => n + (v ?? 0), 0)}
        />
      </div>

      {error ? <ErrorNote message={error} /> : null}
      {actionError ? <ErrorNote message={actionError} /> : null}
      {note ? (
        <div className="mb-4 rounded-md border border-ok/30 bg-ok/10 px-3 py-2 text-sm text-ok">
          {note}
        </div>
      ) : null}

      <Card title={`${rows.length} dead deliver${rows.length === 1 ? "y" : "ies"}`}>
        {loading && rows.length === 0 ? (
          <Spinner />
        ) : rows.length === 0 ? (
          <Empty>Nothing in the dead-letter queue. Every delivery reached its endpoint.</Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full">
              <thead className="border-b border-border">
                <tr>
                  <th className="th w-10">
                    <input
                      type="checkbox"
                      aria-label="Select all"
                      checked={allSelected}
                      onChange={toggleAll}
                      className="h-4 w-4 rounded border-border bg-canvas"
                    />
                  </th>
                  <th className="th">Event</th>
                  <th className="th">Type</th>
                  <th className="th">Endpoint</th>
                  <th className="th">Attempts</th>
                  <th className="th">Last code</th>
                  <th className="th">Last error</th>
                  <th className="th">Died</th>
                  <th className="th" />
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {rows.map((d) => (
                  <tr key={d.id} className="hover:bg-raised/40">
                    <td className="td">
                      <input
                        type="checkbox"
                        aria-label={`Select delivery ${d.id}`}
                        checked={selected.has(d.id)}
                        onChange={() => toggle(d.id)}
                        className="h-4 w-4 rounded border-border bg-canvas"
                      />
                    </td>
                    <td className="td">
                      <EventLink id={d.event_id} />
                    </td>
                    <td className="td">
                      <Mono className="text-muted">{d.event_type}</Mono>
                    </td>
                    <td className="td max-w-xs truncate">
                      <Mono className="text-muted">{d.endpoint_url}</Mono>
                    </td>
                    <td className="td tabular-nums">{d.attempt_count}</td>
                    <td className="td">
                      {d.last_status_code ? (
                        <Mono className="text-danger">{d.last_status_code}</Mono>
                      ) : (
                        <span className="text-muted">—</span>
                      )}
                    </td>
                    <td className="td max-w-sm truncate text-xs text-danger/80">
                      {d.last_error ?? "—"}
                    </td>
                    <td className="td text-muted" title={absoluteTime(d.completed_at)}>
                      {relativeTime(d.completed_at)}
                    </td>
                    <td className="td text-right">
                      <button
                        type="button"
                        className="btn-ghost"
                        disabled={busy}
                        onClick={() =>
                          run(() => api.replayDelivery(d.id), "Replayed delivery")
                        }
                      >
                        Replay
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </Shell>
  );
}
