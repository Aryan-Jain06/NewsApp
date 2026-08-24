"use client";

import type {
  Delivery,
  Endpoint,
  EndpointStats,
  EventSummary,
  HookEvent,
  Overview,
  Tenant,
  Timeseries,
} from "./types";

const BASE =
  process.env.NEXT_PUBLIC_API_URL?.replace(/\/$/, "") ?? "http://localhost:8080";

const TOKEN_KEY = "hookrelay.token";
const TENANT_KEY = "hookrelay.tenant";

/** ApiError carries the HTTP status so callers can branch on 401 vs 500. */
export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "ApiError";
  }
}

export function getToken(): string | null {
  if (typeof window === "undefined") return null;
  try {
    return window.localStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

export function getStoredTenant(): Tenant | null {
  if (typeof window === "undefined") return null;
  try {
    const raw = window.localStorage.getItem(TENANT_KEY);
    return raw ? (JSON.parse(raw) as Tenant) : null;
  } catch {
    return null;
  }
}

function persistSession(token: string, tenant: Tenant) {
  try {
    window.localStorage.setItem(TOKEN_KEY, token);
    window.localStorage.setItem(TENANT_KEY, JSON.stringify(tenant));
  } catch {
    // A private window with storage disabled still works for one session.
  }
}

export function clearSession() {
  try {
    window.localStorage.removeItem(TOKEN_KEY);
    window.localStorage.removeItem(TENANT_KEY);
  } catch {
    // Nothing to clear.
  }
}

interface ApiErrorBody {
  error?: { code?: string; message?: string };
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Content-Type", "application/json");
  const token = getToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);

  let res: Response;
  try {
    res = await fetch(`${BASE}${path}`, { ...init, headers, cache: "no-store" });
  } catch {
    throw new ApiError(0, `cannot reach the HookRelay API at ${BASE}`);
  }

  if (res.status === 204) return undefined as T;

  const text = await res.text();
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`;
    try {
      const body = JSON.parse(text) as ApiErrorBody;
      if (body.error?.message) message = body.error.message;
    } catch {
      if (text) message = text;
    }
    throw new ApiError(res.status, message);
  }
  return text ? (JSON.parse(text) as T) : (undefined as T);
}

export const api = {
  async login(email: string, password: string): Promise<Tenant> {
    const res = await request<{ tenant: Tenant; token: string }>("/auth/login", {
      method: "POST",
      body: JSON.stringify({ email, password }),
    });
    persistSession(res.token, res.tenant);
    return res.tenant;
  },

  async register(name: string, email: string, password: string) {
    const res = await request<{ tenant: Tenant; token: string; api_key: string }>(
      "/auth/register",
      { method: "POST", body: JSON.stringify({ name, email, password }) },
    );
    persistSession(res.token, res.tenant);
    return res;
  },

  me: () => request<{ tenant: Tenant }>("/auth/me"),

  listEndpoints: () => request<{ endpoints: Endpoint[] | null }>("/endpoints"),

  getEndpoint: (id: string, revealSecret = false) =>
    request<Endpoint>(
      `/endpoints/${id}${revealSecret ? "?reveal_secret=true" : ""}`,
    ),

  createEndpoint: (body: {
    url: string;
    description: string;
    event_types: string[];
  }) => request<Endpoint>("/endpoints", { method: "POST", body: JSON.stringify(body) }),

  updateEndpoint: (
    id: string,
    body: Partial<{
      url: string;
      description: string;
      active: boolean;
      event_types: string[];
    }>,
  ) => request<Endpoint>(`/endpoints/${id}`, { method: "PATCH", body: JSON.stringify(body) }),

  deleteEndpoint: (id: string) =>
    request<void>(`/endpoints/${id}`, { method: "DELETE" }),

  rotateSecret: (id: string) =>
    request<{ endpoint: Endpoint; note: string }>(`/endpoints/${id}/rotate-secret`, {
      method: "POST",
    }),

  endpointStats: (id: string, windowHours = 24) =>
    request<EndpointStats>(`/endpoints/${id}/stats?window_hours=${windowHours}`),

  listEvents: (params: { limit?: number; event_type?: string; cursor?: string } = {}) => {
    const q = new URLSearchParams();
    if (params.limit) q.set("limit", String(params.limit));
    if (params.event_type) q.set("event_type", params.event_type);
    if (params.cursor) q.set("cursor", params.cursor);
    const qs = q.toString();
    return request<{ events: EventSummary[]; next_cursor: string }>(
      `/events${qs ? `?${qs}` : ""}`,
    );
  },

  getEvent: (id: string) =>
    request<{ event: HookEvent; deliveries: Delivery[] }>(`/events/${id}`),

  replayEvent: (id: string) =>
    request<{ event_id: string; replayed: number }>(`/events/${id}/replay`, {
      method: "POST",
    }),

  listDeliveries: (
    params: {
      status?: string;
      endpoint_id?: string;
      event_id?: string;
      limit?: number;
      offset?: number;
      include_attempts?: boolean;
    } = {},
  ) => {
    const q = new URLSearchParams();
    if (params.status) q.set("status", params.status);
    if (params.endpoint_id) q.set("endpoint_id", params.endpoint_id);
    if (params.event_id) q.set("event_id", params.event_id);
    if (params.limit) q.set("limit", String(params.limit));
    if (params.offset) q.set("offset", String(params.offset));
    if (params.include_attempts) q.set("include_attempts", "true");
    const qs = q.toString();
    return request<{
      deliveries: Delivery[];
      counts: Partial<Record<string, number>>;
    }>(`/deliveries${qs ? `?${qs}` : ""}`);
  },

  replayDelivery: (id: string) =>
    request<{ replayed: number }>(`/deliveries/${id}/replay`, { method: "POST" }),

  bulkReplay: (body: { delivery_ids?: string[]; status?: string; limit?: number }) =>
    request<{ replayed: number; delivery_ids: string[] }>("/deliveries/replay", {
      method: "POST",
      body: JSON.stringify(body),
    }),

  overview: (windowHours = 24) =>
    request<Overview>(`/stats/overview?window_hours=${windowHours}`),

  timeseries: (windowHours = 1, bucketSeconds = 60) =>
    request<Timeseries>(
      `/stats/timeseries?window_hours=${windowHours}&bucket_seconds=${bucketSeconds}`,
    ),
};

export { BASE as API_BASE };
