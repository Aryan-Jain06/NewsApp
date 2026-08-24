"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { clearSession, getStoredTenant } from "@/lib/api";

const links = [
  { href: "/", label: "Overview" },
  { href: "/endpoints", label: "Endpoints" },
  { href: "/events", label: "Events" },
  { href: "/dlq", label: "Dead letters" },
];

export function Nav() {
  const pathname = usePathname();
  const router = useRouter();
  const [email, setEmail] = useState<string>("");

  useEffect(() => {
    setEmail(getStoredTenant()?.email ?? "");
  }, []);

  return (
    <header className="sticky top-0 z-20 border-b border-border bg-canvas/90 backdrop-blur">
      <div className="mx-auto flex h-14 max-w-[1600px] items-center gap-6 px-6">
        <Link href="/" className="flex items-center gap-2">
          <span className="grid h-6 w-6 place-items-center rounded bg-accent/20 text-xs font-bold text-accent">
            H
          </span>
          <span className="text-sm font-semibold tracking-tight">HookRelay</span>
        </Link>

        <nav className="flex items-center gap-1">
          {links.map((l) => {
            const active =
              l.href === "/" ? pathname === "/" : pathname.startsWith(l.href);
            return (
              <Link
                key={l.href}
                href={l.href}
                className={`rounded-md px-3 py-1.5 text-sm transition-colors ${
                  active
                    ? "bg-raised text-ink"
                    : "text-muted hover:bg-raised/60 hover:text-ink"
                }`}
              >
                {l.label}
              </Link>
            );
          })}
        </nav>

        <div className="ml-auto flex items-center gap-3">
          {email ? <span className="text-xs text-muted">{email}</span> : null}
          <button
            type="button"
            className="btn-ghost"
            onClick={() => {
              clearSession();
              router.replace("/login");
            }}
          >
            Sign out
          </button>
        </div>
      </div>
    </header>
  );
}
