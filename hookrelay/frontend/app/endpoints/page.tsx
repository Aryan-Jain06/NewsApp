"use client";

import Link from "next/link";
import { useState } from "react";
import { api } from "@/lib/api";
import { relativeTime } from "@/lib/format";
import { usePolling } from "@/lib/usePolling";
import { Shell } from "@/components/Shell";
import { Card, Empty, ErrorNote, Mono, Spinner } from "@/components/ui";

export default function EndpointsPage() {
  const { data, error, loading, refresh } = usePolling(() => api.listEndpoints(), 5000);
  const [creating, setCreating] = useState(false);

  const endpoints = data?.endpoints ?? [];

  return (
    <Shell
      title="Endpoints"
      subtitle="Subscriber URLs and the event types each one receives."
      actions={
        <button type="button" className="btn-primary" onClick={() => setCreating((v) => !v)}>
          {creating ? "Cancel" : "New endpoint"}
        </button>
      }
    >
      {error ? <ErrorNote message={error} /> : null}

      {creating ? (
        <div className="mb-4">
          <CreateEndpointForm
            onCreated={() => {
              setCreating(false);
              refresh();
            }}
          />
        </div>
      ) : null}

      <Card title={`${endpoints.length} endpoint${endpoints.length === 1 ? "" : "s"}`}>
        {loading && endpoints.length === 0 ? (
          <Spinner />
        ) : endpoints.length === 0 ? (
          <Empty>
            No endpoints yet. Create one, then publish an event to see deliveries appear.
          </Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full">
              <thead className="border-b border-border">
                <tr>
                  <th className="th">URL</th>
                  <th className="th">Description</th>
                  <th className="th">Event types</th>
                  <th className="th">State</th>
                  <th className="th">Failures</th>
                  <th className="th">Created</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {endpoints.map((e) => (
                  <tr key={e.id} className="hover:bg-raised/40">
                    <td className="td">
                      <Link href={`/endpoints/${e.id}`} className="text-accent hover:underline">
                        <Mono>{e.url}</Mono>
                      </Link>
                    </td>
                    <td className="td text-muted">{e.description || "—"}</td>
                    <td className="td">
                      <div className="flex flex-wrap gap-1">
                        {e.event_types.map((t) => (
                          <span
                            key={t}
                            className="rounded border border-border bg-raised px-1.5 py-0.5 font-mono text-xs text-muted"
                          >
                            {t}
                          </span>
                        ))}
                      </div>
                    </td>
                    <td className="td">
                      {e.circuit_opened_until ? (
                        <span className="rounded-full border border-danger/30 bg-danger/10 px-2 py-0.5 text-xs text-danger">
                          paused
                        </span>
                      ) : e.active ? (
                        <span className="rounded-full border border-ok/30 bg-ok/10 px-2 py-0.5 text-xs text-ok">
                          active
                        </span>
                      ) : (
                        <span className="rounded-full border border-border bg-raised px-2 py-0.5 text-xs text-muted">
                          disabled
                        </span>
                      )}
                    </td>
                    <td className="td tabular-nums text-muted">{e.consecutive_failures}</td>
                    <td className="td text-muted">{relativeTime(e.created_at)}</td>
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

function CreateEndpointForm({ onCreated }: { onCreated: () => void }) {
  const [url, setUrl] = useState("");
  const [description, setDescription] = useState("");
  const [types, setTypes] = useState("order.created");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [secret, setSecret] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const created = await api.createEndpoint({
        url,
        description,
        event_types: types
          .split(",")
          .map((t) => t.trim())
          .filter(Boolean),
      });
      // The secret comes back only on create, so surface it before refreshing.
      setSecret(created.secret ?? null);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  if (secret) {
    return (
      <Card title="Endpoint created — copy its signing secret">
        <div className="space-y-3 p-4">
          <p className="text-sm text-muted">
            Give this secret to the receiver so it can verify the{" "}
            <Mono>X-HookRelay-Signature</Mono> header. It stays retrievable from the
            endpoint page, but treat it like a password.
          </p>
          <code className="block overflow-x-auto rounded-md border border-border bg-canvas px-3 py-2 font-mono text-xs">
            {secret}
          </code>
          <button type="button" className="btn-primary" onClick={onCreated}>
            Done
          </button>
        </div>
      </Card>
    );
  }

  return (
    <Card title="New endpoint">
      <form onSubmit={submit} className="grid gap-4 p-4 md:grid-cols-3">
        <div className="md:col-span-2">
          <label className="label" htmlFor="url">
            Destination URL
          </label>
          <input
            id="url"
            required
            className="input"
            placeholder="https://api.example.com/webhooks/hookrelay"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
          />
        </div>
        <div>
          <label className="label" htmlFor="desc">
            Description
          </label>
          <input
            id="desc"
            className="input"
            placeholder="Orders service"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>
        <div className="md:col-span-3">
          <label className="label" htmlFor="types">
            Event types (comma separated, <Mono>*</Mono> subscribes to everything)
          </label>
          <input
            id="types"
            required
            className="input"
            placeholder="order.created, order.refunded"
            value={types}
            onChange={(e) => setTypes(e.target.value)}
          />
        </div>
        {error ? (
          <div className="md:col-span-3">
            <ErrorNote message={error} />
          </div>
        ) : null}
        <div className="md:col-span-3">
          <button type="submit" className="btn-primary" disabled={busy}>
            {busy ? "Creating…" : "Create endpoint"}
          </button>
        </div>
      </form>
    </Card>
  );
}
