# Gemini Quota Governor: Architecture

![Architecture](images/architecture.jpg)

## 1. Problem

With Vertex AI Dynamic Shared Quota (DSQ), an organization gets a spend-based allowance. Traffic inside it is protected, while burst above it is shed first when a region gets busy. Vertex AI cannot see which of an org's requests matter, so batch jobs can use up the allowance and push interactive traffic into the sheddable burst.

The allowance itself cannot be queried, so the governor works from a configured value (milestone 1). Learning it from 429s is milestone 2.

## 2. Components

| Component | Runs | Responsibility |
| :--- | :--- | :--- |
| Envoy (or a load balancer / Agent Gateway with Service Extensions) | In the request path | Proxies `aiplatform.googleapis.com` and `*-aiplatform.googleapis.com`, calls the data plane over `ext_proc` for request and response headers, fails open. |
| Data plane | Next to Envoy, any number of replicas | Decides forward, reroute or 429 per request from local token buckets. Counts upstream responses. |
| Control plane | One instance, out of the request path | Splits allowances across replicas, subtracts bypass traffic, streams quota snapshots. |

## 3. Admission (data plane)

Each data plane holds one token bucket per configured `location/model` allowance, sized to its share (section 4). Buckets hold one minute of capacity and refill continuously.

| Priority | Rule |
| :--- | :--- |
| `critical` | Always admitted. It may use the reserve, and beyond the allowance it is still admitted and recorded as debt, which keeps best-effort traffic out until refill repays it. |
| `best_effort`, or no/unknown header | Admitted only while the bucket stays above the critical reserve (`critical_reserve_percent`, default 30%). Otherwise HTTP 429 with `Retry-After`. Never rerouted. |
| `custom` | Same ceiling as best effort. When the primary is full, tries the steps of the `X-Governor-Policy` cascade in order and rewrites `:path`/`:authority` to the first target with room. Never changes the caller's project. |

Details:

- **Token estimate:** `max(X-Prompt-Tokens, Content-Length / 4)`, else `default_request_tokens`. The larger value wins, so a client cannot understate a large body. Vertex quotas count input tokens, so output tokens are not charged.
- **Snapshot check:** if the control plane's snapshot has data for a target (for example a fixed per-project quota), it must also be below the ceiling. The snapshot is checked first, so full targets never consume local tokens.
- **Fail open:** targets with no allowance and no snapshot data are admitted. If the data plane is down, Envoy forwards unchanged.
- **Scope:** only `publishers/google` (Gemini) model paths are governed; partner models and other paths are bypassed untouched.
- **Global endpoint:** location `global` maps to `aiplatform.googleapis.com`; regional locations map to `{region}-aiplatform.googleapis.com`. Custom hostnames (private DNS, PSC) are never rewritten.

## 4. Replica shares (control plane)

Every 10 seconds each data plane calls `ReportUsage` with the requests and tokens it saw per target (all, and the admitted subset). The control plane returns its share of each allowance:

$$\text{share}_i = \frac{\text{floor}}{N} + (1 - \text{floor}) \cdot \frac{\text{demand}_i}{\sum_j \text{demand}_j}$$

with `floor` = 10% and demand smoothed as a moving average. Replicas silent for 30 seconds drop out. Growing a share raises the bucket's cap but not its balance, so replicas cannot jointly overshoot while shares shift. After three failed reports a replica falls back to `allowance / dataplane_replicas`.

## 5. Bypass subtraction (control plane)

The allowance covers the whole org, so traffic that reaches Vertex AI without going through the governor uses it up too. Every poll interval (default 30s) the control plane reads org-wide per-minute usage from Cloud Monitoring, through `metrics_scope_project` when set, and compares it minute by minute with what the data planes reported as admitted:

$$\text{bypass}(M) = \max(0,\ \text{usage}(M) - \text{admitted}(M))$$

$$\text{factor} = 1 - \text{margin} - \frac{\max_{\text{last 3 complete minutes}} \text{bypass}}{\text{allowance}}$$

Every share is multiplied by the factor (floored at 1%). Before the first Monitoring read the factor is 1. If the data is older than 5 minutes it falls back to `1 - margin`. Monitoring lags 2 to 4 minutes, so a bypass spike is subtracted late and released about 3 minutes after it ends: the safe direction. Critical traffic is unaffected by the factor.

## 6. Observability

- **Metrics** (Prometheus text, `/metrics`): decisions by priority and outcome, upstream responses and 429s by target and priority, critical admitted over the allowance, estimated tokens admitted, bucket balance, snapshot age; on the control plane, reconcile results, active replicas, bypass estimate, allowance factor and Monitoring data age.
- **Upstream 429s:** Envoy sends response headers to the data plane, which attributes each status to the request's target and priority. Governor sheds are `ImmediateResponse`s, so they never reach the response phase and are not miscounted.
- **Health:** `/healthz` on both, plus the gRPC health service. The control plane's quota service is `NOT_SERVING` until the first successful reconcile.
- **Logs:** `slog`, with `--log-format json` emitting Cloud Logging `severity`/`message` keys.

## 7. Deployment options

| Option | Status | Notes |
| :--- | :--- | :--- |
| Self-hosted Envoy (Docker Compose, GKE, VM) + Cloud DNS private zone for `aiplatform.googleapis.com` | Tested (Docker e2e) | Dynamic forward proxy follows rewritten hosts, so rerouting works. |
| Application Load Balancer or Agent Gateway + Service Extensions `ext_proc` callout | Not yet tested | Forward and 429 use standard header mutations. Rerouting needs the URL map to route the rewritten host. |

All of the org's Gemini traffic should go through the governor. Enforce that with IAM (only the gateway identity holds `roles/aiplatform.user`) and VPC Service Controls. Whatever still bypasses it is subtracted by section 5.

## 8. References

- [Envoy external processing filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter)
- [Service Extensions overview](https://cloud.google.com/service-extensions/docs/overview)
- [Vertex AI Dynamic Shared Quota](https://cloud.google.com/vertex-ai/generative-ai/docs/dynamic-shared-quota)
- [Cloud Monitoring metrics scopes](https://cloud.google.com/monitoring/settings)
- [Cloud DNS private zones](https://cloud.google.com/dns/docs/zones/zones-overview)
