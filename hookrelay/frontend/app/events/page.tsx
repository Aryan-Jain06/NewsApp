"use client";

import { useState } from "react";
import { api } from "@/lib/api";
import { absoluteTime, relativeTime } from "@/lib/format";
import { usePolling } from "@/lib/usePolling";
import { Shell } from "@/components/Shell";
import { Card, Empty, ErrorNote, EventLink, Mono, Spinner } from "@/components/ui";
import type { DeliveryStatus } from "@/lib/types";

const statusOrder: DeliveryStatus[] = ["succeeded", "delivering", "pending", "failed", "dead"];
const statusTone: Record<string, string> = {
  succeeded: "text-ok",
  pending: "text-accent",
  delivering: "text-accent",
  failed: "text-warn",
  dead: "text-dead",
};

export default function EventsPage() {
  const [filter, setFilter] = useState("");
  const { data, error, loading } = usePolling(
    () => api.listEvents({ limit: 50, event_type: filter || undefined }),
    5000,
    [filter],
  );

  const events = data?.events ?? [];

  return (
    <Shell
      title="Events"
      subtitle="Everything published to HookRelay, newest first, with per-endpoint delivery state."
      actions={
        <input
          className="input max-w-xs"
          placeholder="Filter by event type…"
          value={filter}
          onChange={(e) => setFilter(e.target.value.trim())}
        />
      }
    >
      {error ? <ErrorNote message={error} /> : null}

      <Card title={`${events.length} event${events.length === 1 ? "" : "s"}`}>
        {loading && events.length === 0 ? (
          <Spinner />
        ) : events.length === 0 ? (
          <Empty>
            No events yet. Publish one:{" "}
            <Mono>
              curl -X POST $API/events -H &quot;Authorization: Bearer $KEY&quot; -d
              &apos;{"{"}&quot;event_type&quot;:&quot;order.created&quot;,&quot;payload&quot;:{"{}"}
              {"}"}&apos;
            </Mono>
          </Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full">
              <thead className="border-b border-border">
                <tr>
                  <th className="th">Event ID</th>
                  <th className="th">Type</th>
                  <th className="th">Deliveries</th>
                  <th className="th">Breakdown</th>
                  <th className="th">Idempotency key</th>
                  <th className="th">Published</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {events.map(({ event, delivery_count, deliveries_by_status }) => (
                  <tr key={event.id} className="hover:bg-raised/40">
                    <td className="td">
                      <EventLink id={event.id} />
                    </td>
                    <td className="td">
                      <Mono className="text-muted">{event.event_type}</Mono>
                    </td>
                    <td className="td tabular-nums">{delivery_count}</td>
                    <td className="td">
                      <div className="flex gap-3">
                        {statusOrder
                          .filter((s) => (deliveries_by_status[s] ?? 0) > 0)
                          .map((s) => (
                            <span key={s} className={`text-xs ${statusTone[s]}`}>
                              {deliveries_by_status[s]} {s}
                            </span>
                          ))}
                        {delivery_count === 0 ? (
                          <span className="text-xs text-muted">no subscribers</span>
                        ) : null}
                      </div>
                    </td>
                    <td className="td text-muted">
                      {event.idempotency_key ? <Mono>{event.idempotency_key}</Mono> : "—"}
                    </td>
                    <td className="td text-muted" title={absoluteTime(event.created_at)}>
                      {relativeTime(event.created_at)}
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
