"use client";

import { useParams, useRouter } from "next/navigation";
import { useState } from "react";
import { api } from "@/lib/api";
import { absoluteTime, countdown, latency, percent, relativeTime } from "@/lib/format";
import { usePolling } from "@/lib/usePolling";
import { Shell } from "@/components/Shell";
import {
  Card,
  CopyButton,
  Empty,
  ErrorNote,
  EventLink,
  Mono,
  Spinner,
  StatCard,
  StatusBadge,
} from "@/components/ui";

export default function EndpointDetailPage() {
  const params = useParams<{ id: string }>();
  const id = params.id;
  const router = useRouter();

  const endpoint = usePolling(() => api.getEndpoint(id), 5000, [id]);
  const stats = usePolling(() => api.endpointStats(id, 24), 5000, [id]);
  const deliveries = usePolling(
    () => api.listDeliveries({ endpoint_id: id, limit: 50 }),
    5000,
    [id],
  );

  const e = endpoint.data;
  const s = stats.data;

  return (
    <Shell
      title={e ? <Mono className="text-lg">{e.url}</Mono> : "Endpoint"}
      subtitle={e?.description || "Endpoint configuration, signing secret and recent deliveries."}
      actions={
        <div className="flex gap-2">
          <button type="button" className="btn-ghost" onClick={() => router.push("/endpoints")}>
            Back
          </button>
        </div>
      }
    >
      {endpoint.error ? <ErrorNote message={endpoint.error} /> : null}
      {!e ? (
        <Spinner label="Loading endpoint" />
      ) : (
        <>
          <div className="grid grid-cols-2 gap-4 lg:grid-cols-5">
            <StatCard
              label="Success rate (24h)"
              value={percent(s?.success_rate)}
              tone={s && s.success_rate >= 0.99 ? "ok" : s && s.success_rate < 0.9 ? "danger" : "default"}
              hint={s ? `${s.succeeded} of ${s.succeeded + s.dead} settled` : undefined}
            />
            <StatCard label="Deliveries (24h)" value={s?.total ?? "—"} />
            <StatCard label="Dead" value={s?.dead ?? 0} tone="dead" />
            <StatCard label="p95 latency" value={latency(s?.p95_latency_ms)} />
            <StatCard
              label="Consecutive failures"
              value={e.consecutive_failures}
              tone={e.consecutive_failures > 0 ? "warn" : "default"}
              hint={
                e.circuit_opened_until
                  ? `breaker open, resumes ${countdown(e.circuit_opened_until)}`
                  : "breaker closed"
              }
            />
          </div>

          <div className="mt-4 grid gap-4 lg:grid-cols-2">
            <ConfigCard endpointId={id} onChanged={endpoint.refresh} initial={e} />
            <SecretCard endpointId={id} previousExpiresAt={e.previous_secret_expires_at ?? null} />
          </div>

          <div className="mt-4">
            <Card
              title="Recent deliveries"
              action={<span className="text-xs text-muted">refreshes every 5s</span>}
            >
              {deliveries.error ? (
                <div className="p-4">
                  <ErrorNote message={deliveries.error} />
                </div>
              ) : (deliveries.data?.deliveries ?? []).length === 0 ? (
                <Empty>No deliveries to this endpoint yet.</Empty>
              ) : (
                <div className="overflow-x-auto">
                  <table className="w-full">
                    <thead className="border-b border-border">
                      <tr>
                        <th className="th">Status</th>
                        <th className="th">Event</th>
                        <th className="th">Type</th>
                        <th className="th">Attempts</th>
                        <th className="th">Last code</th>
                        <th className="th">Next retry</th>
                        <th className="th">Created</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-border">
                      {(deliveries.data?.deliveries ?? []).map((d) => (
                        <tr key={d.id} className="hover:bg-raised/40">
                          <td className="td">
                            <StatusBadge status={d.status} />
                          </td>
                          <td className="td">
                            <EventLink id={d.event_id} />
                          </td>
                          <td className="td text-muted">
                            <Mono>{d.event_type}</Mono>
                          </td>
                          <td className="td tabular-nums">{d.attempt_count}</td>
                          <td className="td">
                            {d.last_status_code ? (
                              <Mono
                                className={
                                  d.last_status_code < 300 ? "text-ok" : "text-danger"
                                }
                              >
                                {d.last_status_code}
                              </Mono>
                            ) : (
                              <span className="text-muted">—</span>
                            )}
                          </td>
                          <td className="td tabular-nums text-muted">
                            {d.status === "failed" ? countdown(d.next_attempt_at) : "—"}
                          </td>
                          <td className="td text-muted" title={absoluteTime(d.created_at)}>
                            {relativeTime(d.created_at)}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </Card>
          </div>
        </>
      )}
    </Shell>
  );
}

function ConfigCard({
  endpointId,
  initial,
  onChanged,
}: {
  endpointId: string;
  initial: { url: string; description: string; active: boolean; event_types: string[] };
  onChanged: () => void;
}) {
  const router = useRouter();
  const [url, setUrl] = useState(initial.url);
  const [description, setDescription] = useState(initial.description);
  const [types, setTypes] = useState(initial.event_types.join(", "));
  const [active, setActive] = useState(initial.active);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setSaved(false);
    try {
      await api.updateEndpoint(endpointId, {
        url,
        description,
        active,
        event_types: types.split(",").map((t) => t.trim()).filter(Boolean),
      });
      setSaved(true);
      onChanged();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!window.confirm("Delete this endpoint and all of its delivery history?")) return;
    setBusy(true);
    try {
      await api.deleteEndpoint(endpointId);
      router.push("/endpoints");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setBusy(false);
    }
  };

  return (
    <Card title="Configuration">
      <form onSubmit={save} className="space-y-4 p-4">
        <div>
          <label className="label" htmlFor="cfg-url">
            Destination URL
          </label>
          <input id="cfg-url" className="input" value={url} onChange={(e) => setUrl(e.target.value)} />
        </div>
        <div>
          <label className="label" htmlFor="cfg-desc">
            Description
          </label>
          <input
            id="cfg-desc"
            className="input"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>
        <div>
          <label className="label" htmlFor="cfg-types">
            Event types
          </label>
          <input
            id="cfg-types"
            className="input"
            value={types}
            onChange={(e) => setTypes(e.target.value)}
          />
        </div>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={active}
            onChange={(e) => setActive(e.target.checked)}
            className="h-4 w-4 rounded border-border bg-canvas"
          />
          Active — a disabled endpoint stops receiving new attempts
        </label>

        {error ? <ErrorNote message={error} /> : null}

        <div className="flex items-center gap-2">
          <button type="submit" className="btn-primary" disabled={busy}>
            {busy ? "Saving…" : "Save changes"}
          </button>
          {saved ? <span className="text-xs text-ok">Saved</span> : null}
          <button type="button" className="btn-danger ml-auto" onClick={remove} disabled={busy}>
            Delete
          </button>
        </div>
      </form>
    </Card>
  );
}

function SecretCard({
  endpointId,
  previousExpiresAt,
}: {
  endpointId: string;
  previousExpiresAt: string | null;
}) {
  const [secret, setSecret] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [rotatedAt, setRotatedAt] = useState<string | null>(previousExpiresAt);

  const reveal = async () => {
    setBusy(true);
    setError(null);
    try {
      const e = await api.getEndpoint(endpointId, true);
      setSecret(e.secret ?? null);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const rotate = async () => {
    if (
      !window.confirm(
        "Rotate the signing secret? The previous secret keeps verifying for 24 hours so receivers can roll over.",
      )
    )
      return;
    setBusy(true);
    setError(null);
    try {
      const res = await api.rotateSecret(endpointId);
      setSecret(res.endpoint.secret ?? null);
      setRotatedAt(res.endpoint.previous_secret_expires_at ?? null);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card title="Signing secret">
      <div className="space-y-4 p-4">
        <p className="text-sm text-muted">
          HookRelay signs every request with HMAC-SHA256 over{" "}
          <Mono>{"{id}.{timestamp}.{body}"}</Mono> and sends it as{" "}
          <Mono>X-HookRelay-Signature: v1=…</Mono>.
        </p>

        {secret ? (
          <div className="flex items-center gap-2">
            <code className="flex-1 overflow-x-auto rounded-md border border-border bg-canvas px-3 py-2 font-mono text-xs">
              {secret}
            </code>
            <CopyButton text={secret} />
          </div>
        ) : (
          <div className="flex items-center gap-2">
            <code className="flex-1 rounded-md border border-border bg-canvas px-3 py-2 font-mono text-xs text-muted">
              whsec_•••••••••••••••••••••••••••
            </code>
            <button type="button" className="btn-ghost" onClick={reveal} disabled={busy}>
              Reveal
            </button>
          </div>
        )}

        {rotatedAt ? (
          <div className="rounded-md border border-warn/30 bg-warn/10 px-3 py-2 text-xs text-warn">
            A previous secret is still accepted until {absoluteTime(rotatedAt)} (
            {countdown(rotatedAt)}). Requests carry both signatures, space separated,
            until then.
          </div>
        ) : null}

        {error ? <ErrorNote message={error} /> : null}

        <button type="button" className="btn-ghost" onClick={rotate} disabled={busy}>
          {busy ? "Working…" : "Rotate secret"}
        </button>
      </div>
    </Card>
  );
}
