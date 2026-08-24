"use client";

import Link from "next/link";
import { type ReactNode, useState } from "react";
import type { DeliveryStatus } from "@/lib/types";

/** StatusBadge colours a delivery status consistently across every page. */
export function StatusBadge({ status }: { status: DeliveryStatus | string }) {
  const styles: Record<string, string> = {
    succeeded: "border-ok/30 bg-ok/10 text-ok",
    pending: "border-accent/30 bg-accent/10 text-accent",
    delivering: "border-accent/40 bg-accent/15 text-accent",
    failed: "border-warn/30 bg-warn/10 text-warn",
    dead: "border-dead/30 bg-dead/10 text-dead",
  };
  return (
    <span
      className={`inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium ${
        styles[status] ?? "border-border bg-raised text-muted"
      }`}
    >
      {status}
    </span>
  );
}

/** OutcomeDot marks an individual attempt in the timeline. */
export function OutcomeDot({ outcome }: { outcome: string }) {
  const color =
    outcome === "success" ? "bg-ok" : outcome === "skipped" ? "bg-muted" : "bg-danger";
  return <span className={`inline-block h-2 w-2 shrink-0 rounded-full ${color}`} />;
}

export function StatCard({
  label,
  value,
  hint,
  tone = "default",
}: {
  label: string;
  value: ReactNode;
  hint?: ReactNode;
  tone?: "default" | "ok" | "warn" | "danger" | "dead";
}) {
  const tones = {
    default: "text-ink",
    ok: "text-ok",
    warn: "text-warn",
    danger: "text-danger",
    dead: "text-dead",
  };
  return (
    <div className="card p-4">
      <div className="text-xs font-medium uppercase tracking-wide text-muted">{label}</div>
      <div className={`mt-1.5 text-2xl font-semibold tabular-nums ${tones[tone]}`}>{value}</div>
      {hint ? <div className="mt-1 text-xs text-muted">{hint}</div> : null}
    </div>
  );
}

export function Card({
  title,
  action,
  children,
  className = "",
}: {
  title?: ReactNode;
  action?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <section className={`card ${className}`}>
      {title ? (
        <header className="flex items-center justify-between gap-3 border-b border-border px-4 py-3">
          <h2 className="text-sm font-semibold text-ink">{title}</h2>
          {action}
        </header>
      ) : null}
      {children}
    </section>
  );
}

/** CopyButton copies text and confirms inline — no toast infrastructure needed. */
export function CopyButton({ text, label = "Copy" }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      className="btn-ghost"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
          setCopied(true);
          window.setTimeout(() => setCopied(false), 1500);
        } catch {
          // Clipboard can be blocked; the value is already on screen.
        }
      }}
    >
      {copied ? "Copied" : label}
    </button>
  );
}

export function Mono({ children, className = "" }: { children: ReactNode; className?: string }) {
  return <span className={`font-mono text-xs ${className}`}>{children}</span>;
}

export function ErrorNote({ message }: { message: string }) {
  return (
    <div className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
      {message}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="px-4 py-10 text-center text-sm text-muted">{children}</div>;
}

export function Spinner({ label = "Loading" }: { label?: string }) {
  return (
    <div className="flex items-center gap-2 px-4 py-10 text-sm text-muted">
      <span className="h-3 w-3 animate-spin rounded-full border-2 border-border border-t-accent" />
      {label}
    </div>
  );
}

/** EventLink and EndpointLink keep id → route wiring in one place. */
export function EventLink({ id }: { id: string }) {
  return (
    <Link href={`/events/${id}`} className="font-mono text-xs text-accent hover:underline">
      {id}
    </Link>
  );
}

export function EndpointLink({ id, children }: { id: string; children: ReactNode }) {
  return (
    <Link href={`/endpoints/${id}`} className="text-accent hover:underline">
      {children}
    </Link>
  );
}
