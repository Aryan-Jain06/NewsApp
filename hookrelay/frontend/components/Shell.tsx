"use client";

import type { ReactNode } from "react";
import { AuthGate } from "./AuthGate";
import { Nav } from "./Nav";

/** Shell is the authenticated chrome every dashboard page renders inside. */
export function Shell({
  title,
  subtitle,
  actions,
  children,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <AuthGate>
      <Nav />
      <main className="mx-auto max-w-[1600px] px-6 py-6">
        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
            {subtitle ? <p className="mt-1 text-sm text-muted">{subtitle}</p> : null}
          </div>
          {actions}
        </div>
        {children}
      </main>
    </AuthGate>
  );
}
