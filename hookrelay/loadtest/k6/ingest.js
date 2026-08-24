// k6 ingestion script — the same 10,000-event load as `go run ./cmd/loadtest`,
// for anyone who would rather use k6.
//
// k6 measures ingestion only: it has no way to poll the database for the
// zero-loss accounting, so run the Go tool for the assertions and this for a
// second opinion on ingestion latency under a controlled arrival rate.
//
// Install k6 (free, Apache 2.0): https://grafana.com/docs/k6/latest/set-up/install-k6/
//
//   k6 run -e API_URL=http://localhost:8080 -e RECEIVER_URL=http://localhost:9090 ingest.js
//
// Then check the accounting by hand:
//
//   curl -H "Authorization: Bearer $KEY" localhost:8080/deliveries?limit=1 | jq .counts

import http from "k6/http";
import { check, fail } from "k6";
import { Counter, Trend } from "k6/metrics";
import { scenario } from "k6/execution";

const API = __ENV.API_URL || "http://localhost:8080";
const RECEIVER = __ENV.RECEIVER_URL || "http://localhost:9090";
const EVENTS = Number(__ENV.EVENTS || 10000);
const VUS = Number(__ENV.VUS || 50);

const deliveriesCreated = new Counter("hookrelay_deliveries_created");
const ingestLatency = new Trend("hookrelay_ingest_latency", true);

export const options = {
  scenarios: {
    ingest: {
      executor: "shared-iterations",
      vus: VUS,
      iterations: EVENTS,
      maxDuration: "10m",
    },
  },
  thresholds: {
    // Ingestion must stay fast: it only writes rows and one stream entry.
    "http_req_duration{name:publish}": ["p(95)<1000"],
    "http_req_failed{name:publish}": ["rate<0.001"],
  },
};

// setup runs once: register a tenant and the three endpoints the run needs.
export function setup() {
  const email = `k6-${Date.now()}@hookrelay.test`;
  const reg = http.post(
    `${API}/auth/register`,
    JSON.stringify({ name: "k6 load test", email, password: "k6-password-123" }),
    { headers: { "Content-Type": "application/json" } },
  );
  if (reg.status !== 201) {
    fail(`register failed: ${reg.status} ${reg.body}`);
  }
  const apiKey = reg.json("api_key");
  const headers = { "Content-Type": "application/json", Authorization: `Bearer ${apiKey}` };

  const targets = [
    { url: `${RECEIVER}/ok`, description: "healthy" },
    { url: `${RECEIVER}/flaky?rate=0.3`, description: "flaky" },
    { url: `${RECEIVER}/slow?ms=15000`, description: "slow" },
  ];
  const endpointIds = {};
  for (const t of targets) {
    const res = http.post(
      `${API}/endpoints`,
      JSON.stringify({ ...t, event_types: ["load.test"] }),
      { headers },
    );
    if (res.status !== 201) {
      fail(`create endpoint ${t.description} failed: ${res.status} ${res.body}`);
    }
    endpointIds[t.description] = res.json("id");
  }
  console.log(`tenant ${email} with ${targets.length} endpoints ready`);
  return { apiKey, endpointIds };
}

export default function (data) {
  const n = scenario.iterationInTest;
  const res = http.post(
    `${API}/events`,
    JSON.stringify({
      event_type: "load.test",
      payload: {
        n,
        order: `ord_${String(n).padStart(6, "0")}`,
        amount: 1000 + (n % 9000),
        created: Date.now(),
      },
    }),
    {
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${data.apiKey}` },
      tags: { name: "publish" },
    },
  );

  const ok = check(res, {
    "accepted with 202": (r) => r.status === 202,
    "fanned out to 3 endpoints": (r) => r.json("deliveries") === 3,
  });
  if (ok) {
    deliveriesCreated.add(res.json("deliveries"));
    ingestLatency.add(res.timings.duration);
  }
}

export function teardown(data) {
  const headers = { Authorization: `Bearer ${data.apiKey}` };
  const res = http.get(`${API}/deliveries?limit=1`, { headers });
  const counts = res.json("counts") || {};
  console.log("");
  console.log("delivery counts immediately after ingestion:");
  console.log(JSON.stringify(counts, null, 2));
  console.log("");
  console.log("The pipeline is still draining. Poll until pending+failed+delivering is 0:");
  console.log(`  watch -n3 'curl -s -H "Authorization: Bearer ${data.apiKey}" ${API}/deliveries?limit=1 | jq .counts'`);
  console.log("");
  console.log("For the zero-loss assertion, use the Go tool instead:");
  console.log("  go run ./cmd/loadtest -events 10000 -concurrency 50");
}
