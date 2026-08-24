// Mirrors the JSON the HookRelay API returns.

export type DeliveryStatus =
  | "pending"
  | "delivering"
  | "succeeded"
  | "failed"
  | "dead";

export interface Tenant {
  id: string;
  name: string;
  email: string;
  api_key_prefix: string;
  created_at: string;
}

export interface Endpoint {
  id: string;
  tenant_id: string;
  url: string;
  description: string;
  active: boolean;
  secret?: string;
  previous_secret_expires_at?: string | null;
  consecutive_failures: number;
  circuit_opened_until?: string | null;
  event_types: string[];
  created_at: string;
  updated_at: string;
}

export interface Attempt {
  id: number;
  delivery_id: string;
  attempt_no: number;
  status_code?: number | null;
  response_ms?: number | null;
  error?: string | null;
  outcome: "success" | "failure" | "skipped";
  attempted_at: string;
}

export interface Delivery {
  id: string;
  event_id: string;
  endpoint_id: string;
  tenant_id: string;
  status: DeliveryStatus;
  attempt_count: number;
  next_attempt_at?: string | null;
  last_status_code?: number | null;
  last_error?: string | null;
  created_at: string;
  updated_at: string;
  completed_at?: string | null;
  endpoint_url?: string;
  event_type?: string;
  attempts: Attempt[];
}

export interface HookEvent {
  id: string;
  tenant_id: string;
  event_type: string;
  payload: unknown;
  idempotency_key?: string | null;
  created_at: string;
}

export interface EventSummary {
  event: HookEvent;
  delivery_count: number;
  deliveries_by_status: Partial<Record<DeliveryStatus, number>>;
}

export interface EndpointStats {
  endpoint_id: string;
  window_hours: number;
  total: number;
  succeeded: number;
  failed: number;
  dead: number;
  pending: number;
  success_rate: number;
  avg_latency_ms: number | null;
  p95_latency_ms: number | null;
  total_attempts: number;
}

export interface Overview {
  window_hours: number;
  endpoints: number;
  active_endpoints: number;
  events: number;
  deliveries_by_status: Partial<Record<DeliveryStatus, number>>;
  success_rate: number;
  p95_latency_ms: number | null;
  dead_count: number;
}

export interface TimeseriesPoint {
  bucket: string;
  attempts: number;
  succeeded: number;
  failed: number;
  skipped: number;
  success_rate: number;
  p95_latency_ms: number | null;
}

export interface Timeseries {
  bucket_seconds: number;
  points: TimeseriesPoint[];
}
