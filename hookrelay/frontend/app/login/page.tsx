"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { api } from "@/lib/api";
import { CopyButton, ErrorNote } from "@/components/ui";

/**
 * Login doubles as sign-up: HookRelay has no invite flow, so a new tenant
 * registers here and is handed its one-time API key immediately.
 */
export default function LoginPage() {
  const router = useRouter();
  const [mode, setMode] = useState<"login" | "register">("login");
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [apiKey, setApiKey] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      if (mode === "login") {
        await api.login(email, password);
        router.replace("/");
        return;
      }
      const res = await api.register(name, email, password);
      // Show the key before navigating: it is never retrievable again.
      setApiKey(res.api_key);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  if (apiKey) {
    return (
      <main className="flex min-h-screen items-center justify-center px-6">
        <div className="card w-full max-w-lg p-6">
          <h1 className="text-lg font-semibold">Your API key</h1>
          <p className="mt-1 text-sm text-muted">
            Producers authenticate with this key. It is shown once and stored only as a
            hash, so copy it now.
          </p>
          <div className="mt-4 flex items-center gap-2">
            <code className="flex-1 overflow-x-auto rounded-md border border-border bg-canvas px-3 py-2 font-mono text-xs">
              {apiKey}
            </code>
            <CopyButton text={apiKey} />
          </div>
          <button
            type="button"
            className="btn-primary mt-5 w-full"
            onClick={() => router.replace("/")}
          >
            Continue to the dashboard
          </button>
        </div>
      </main>
    );
  }

  return (
    <main className="flex min-h-screen items-center justify-center px-6">
      <div className="w-full max-w-sm">
        <div className="mb-6 flex items-center gap-2">
          <span className="grid h-8 w-8 place-items-center rounded-md bg-accent/20 text-sm font-bold text-accent">
            H
          </span>
          <div>
            <h1 className="text-base font-semibold leading-tight">HookRelay</h1>
            <p className="text-xs text-muted">Reliable webhook delivery</p>
          </div>
        </div>

        <form onSubmit={submit} className="card space-y-4 p-6">
          {mode === "register" ? (
            <div>
              <label className="label" htmlFor="name">
                Organisation
              </label>
              <input
                id="name"
                className="input"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Acme Payments"
                autoComplete="organization"
              />
            </div>
          ) : null}

          <div>
            <label className="label" htmlFor="email">
              Email
            </label>
            <input
              id="email"
              type="email"
              required
              className="input"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="dev@acme.test"
              autoComplete="email"
            />
          </div>

          <div>
            <label className="label" htmlFor="password">
              Password
            </label>
            <input
              id="password"
              type="password"
              required
              minLength={8}
              className="input"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="at least 8 characters"
              autoComplete={mode === "login" ? "current-password" : "new-password"}
            />
          </div>

          {error ? <ErrorNote message={error} /> : null}

          <button type="submit" className="btn-primary w-full" disabled={busy}>
            {busy ? "Working…" : mode === "login" ? "Sign in" : "Create tenant"}
          </button>

          <button
            type="button"
            className="w-full text-center text-xs text-muted hover:text-ink"
            onClick={() => {
              setMode(mode === "login" ? "register" : "login");
              setError(null);
            }}
          >
            {mode === "login"
              ? "No tenant yet? Create one"
              : "Already have a tenant? Sign in"}
          </button>
        </form>
      </div>
    </main>
  );
}
