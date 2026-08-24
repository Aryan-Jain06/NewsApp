"use client";

import { useRouter } from "next/navigation";
import { type ReactNode, useEffect, useState } from "react";
import { getStoredTenant, getToken } from "@/lib/api";
import type { Tenant } from "@/lib/types";

/**
 * AuthGate keeps every dashboard page behind a token check.
 *
 * The JWT lives in localStorage rather than a cookie because the dashboard is a
 * pure client of the API — there is no Next.js server route to attach a session
 * to, and the API already treats the token as a bearer credential.
 */
export function AuthGate({ children }: { children: ReactNode }) {
  const router = useRouter();
  const [tenant, setTenant] = useState<Tenant | null>(null);
  const [checked, setChecked] = useState(false);

  useEffect(() => {
    if (!getToken()) {
      router.replace("/login");
      return;
    }
    setTenant(getStoredTenant());
    setChecked(true);
  }, [router]);

  if (!checked) {
    return (
      <div className="flex min-h-screen items-center justify-center text-sm text-muted">
        Checking session…
      </div>
    );
  }
  return (
    <>
      <span className="hidden" data-tenant={tenant?.email ?? ""} />
      {children}
    </>
  );
}
