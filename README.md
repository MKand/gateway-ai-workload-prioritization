# Gemini Priority Quota Governor

Lets an organization decide which of its own Gemini traffic gets the protected part of its Vertex AI quota.

With Dynamic Shared Quota (DSQ), each org gets a spend-based allowance. Traffic inside it is protected, while burst above it is dropped first when a region is busy. Vertex AI cannot tell which of your requests matter, so a batch job can use up the allowance and push your chat traffic into the droppable burst.

The governor sits in front of Vertex AI (as an Envoy `ext_proc` filter) and fixes that:

- **Critical** traffic is always admitted.
- **Best-effort** traffic (and any request without a priority header) stops at 70% of the allowance. The last 30% is reserved for critical traffic.
- **Custom** traffic also stops at 70%, then falls back to another model or region from a configured policy, always in the caller's own project.

![Architecture](docs/images/architecture.jpg)

## How it works

| Part | What it does |
| :--- | :--- |
| Data plane (`cmd/dataplane`) | Envoy `ext_proc` server. Keeps a token bucket per `location/model` and decides forward, reroute or 429 on every request. Counts upstream 429s. |
| Control plane (`cmd/controlplane`) | Splits each allowance across data plane replicas by their traffic. Reads org-wide usage from Cloud Monitoring and shrinks every share by the traffic that bypassed the governor. |
| Envoy (`deploy/envoy`) | Proxies `aiplatform.googleapis.com` and `*-aiplatform.googleapis.com`. Fails open if the governor is down. |

Details: [docs/architecture.md](docs/architecture.md). Background and gap analysis: [docs/project_overview.md](docs/project_overview.md).

## Folder structure

```
repo/
├── cmd/controlplane/      # Control plane binary (shares, bypass, quota snapshots)
├── cmd/dataplane/         # Data plane binary (Envoy ext_proc)
├── pkg/governor/          # Config, token buckets, limiter, decision engine
├── pkg/controlplane/      # GCP clients, reconciler, share and bypass trackers
├── pkg/dataplane/         # ext_proc server, evaluator, usage reporter, metrics
├── pkg/server/            # QuotaDiscoveryService gRPC server
├── pkg/router/            # Vertex AI URL parsing and rewriting
├── pkg/telemetry/         # Prometheus-format metrics, logging setup
├── pkg/transport/         # gRPC TLS credentials
├── proto/, gen/           # gRPC API and generated Go
├── config/governor.yaml   # Example configuration
├── deploy/envoy/          # Envoy config with the ext_proc filter
├── deploy/compose/        # Local full stack with Docker Compose
├── test/e2e/              # Real Envoy in Docker with fake Vertex endpoints
├── test/live/             # Live GCP integration tests (setup, test, teardown)
└── docs/                  # Architecture, overview, images
```

## Requirements

**Local:** Go 1.26+, Docker (with Compose), `gcloud` with Application Default Credentials (`gcloud auth application-default login`), and `jq` for the live tests.

**Cloud:** a GCP project with `aiplatform.googleapis.com`, `cloudquotas.googleapis.com` and `monitoring.googleapis.com` enabled. The control plane's identity needs `roles/monitoring.viewer` and `roles/cloudquotas.viewer` on every governed project (or on the metrics scope project). Apps keep calling Vertex AI with their own credentials; the governor passes `Authorization` through unchanged.

## Configure

Copy [config/governor.yaml](config/governor.yaml) and set:

| Field | Meaning |
| :--- | :--- |
| `project_ids`, `regions`, `models` | What to govern. `global` is a valid region. Every cascade target must be listed. |
| `allowances` | Your org's DSQ allowance per `location/model` (`max_rpm` and/or `max_tpm`). It cannot be queried: get it from your account team or use your observed peak without 429s. Never commit real numbers. |
| `critical_reserve_percent` | Share of each allowance only critical traffic may use (default `30`). |
| `custom_policies` | Fallback cascades for `custom` traffic, chosen per request with `X-Governor-Policy`. |
| `bypass_margin_percent` | Extra share held back on top of the bypass estimate, to absorb Monitoring lag (default `10`). |
| `metrics_scope_project` | Optional Monitoring scoping project that covers every project using Gemini, so traffic from projects outside `project_ids` is subtracted too. |
| `dataplane_replicas` | Even split of the allowance used only when the control plane is unreachable (default `1`). |
| `default_request_tokens` | Token estimate for requests without `Content-Length` or `X-Prompt-Tokens` (default `1000`). |

## Run locally

```bash
make test                                   # unit tests
make e2e                                    # real Envoy in Docker, fake Vertex endpoints
make compose-up CONFIG=my-governor.yaml     # control plane + data plane + Envoy (Linux, host network)
make compose-down
```

Then send requests to Envoy on `localhost:10000` with the real Vertex host:

```bash
curl -H "Host: aiplatform.googleapis.com" \
     -H "Authorization: Bearer $(gcloud auth print-access-token)" \
     -H "X-Request-Priority: best_effort" -H "Content-Type: application/json" \
     -d '{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}' \
     http://localhost:10000/v1/projects/PROJECT/locations/global/publishers/google/models/gemini-3.5-flash:generateContent
```

Without Docker Compose: `make run-controlplane`, `make run-dataplane` and `make run-envoy` in three terminals.

**Request headers:** `X-Request-Priority` (`critical`, `best_effort`, `custom`), `X-Governor-Policy`, optional `X-Prompt-Tokens`.
**Response headers:** `x-governor-decision` (`forward`, `reroute`, `shed`, `bypass`) and `x-governor-reason`. Sheds are HTTP 429 with `Retry-After`.

## Operate

| Endpoint | Data plane | Control plane |
| :--- | :--- | :--- |
| `/metrics`, `/healthz` | `:9090` | `:9091` |
| gRPC health | `:50052` | `:50051` (quota service is `NOT_SERVING` until the first successful reconcile) |

Key metrics: `governor_decisions_total`, `governor_upstream_429_total`, `governor_critical_over_allowance_total`, `governor_allowance_available`, `governor_bypass_estimate`, `governor_allowance_factor`. Logs are `slog`; pass `--log-format json` for Cloud Logging.

## Deploy

The governor only protects the allowance if all of the org's Gemini traffic goes through it, or the bypass estimate will shrink everyone's budget.

1. **Self-hosted Envoy** (tested): run the two images from the [Dockerfile](Dockerfile) next to Envoy on GKE or a VM, and point apps at it with a Cloud DNS private zone for `aiplatform.googleapis.com`.
2. **Load balancer or Agent Gateway** (not yet tested): register the data plane as a Service Extensions `ext_proc` callout with `REQUEST_HEADERS` and `RESPONSE_HEADERS` events. Forwarding and 429s use standard header mutations. Rerouting to another region also needs the URL map to route the rewritten host, so verify that before relying on it.

**Hardening:** grant `roles/aiplatform.user` only to the gateway's identity and use VPC Service Controls, so apps cannot skip the governor. Run the control plane with TLS (`--tls-cert`, `--tls-key`; data plane `--control-plane-ca`) instead of `--insecure`.

## Known limits

- The allowance is a configured guess. Learning it from 429s is milestone 2.
- The priority header is trusted. Put the governor behind a gateway that sets it from caller identity.
- Cloud Monitoring lags 2 to 4 minutes, so bypass spikes are subtracted late. The margin covers part of that.

## Test status

| Suite | Command | Status |
| :--- | :--- | :--- |
| Unit, race | `make test-race` | Pass |
| Docker e2e (forward, shed, reroute, global endpoint, partner-model bypass, upstream 429 counting) | `make e2e` | Pass |
| Live GCP | `make live-single` ([setup guide](test/live/README.md)) | Last run 2026-10-27, before milestone 1; re-run pending |
