# Gemini Quota Governor: System Architecture

## 1. Overview

The **Gemini Quota Governor** is a priority-aware traffic governor and dynamic router for Google Cloud Platform (GCP) Vertex AI workloads. It prevents quota contention, noisy-neighbor starvation, and hard HTTP 429 rejections by enforcing tenant-aware priority shedding and fallback routing locally before traffic leaves the enterprise perimeter.

The system decouples the synchronous, sub-millisecond request path (**Data Plane**) from the asynchronous quota discovery path (**Control Plane**) using the open-source **Envoy `ext_proc` gRPC specification** (`envoy.service.ext_proc.v3`).

---

## 2. System Architecture

```
                             CONTROL PLANE (Async Reconciler)
                  ┌─────────────────────────────────────────────────────┐
                  │ 1. Telemetry Ingestion: Cloud Quotas & Monitoring   │
                  │ 2. Dynamic Headroom: Integer math & safety margins  │
                  │ 3. Config Watcher: Declarative policy ingestion     │
                  │ 4. State Publisher: In-memory store & fan-out       │
                  └──────────────────────────┬──────────────────────────┘
                                             │ gRPC StreamQuotas (Protobuf)
═════════════════════════════════════════════╪══════════════════════════════════════════════
                                             │
                               DATA PLANE (ext_proc Engine)
                  ┌──────────────────────────┴──────────────────────────┐
                  │ • Dual-Clock Token Buckets (RPM/TPM)                │
                  │ • Priority Evaluation (CRITICAL, BEST_EFFORT, CUSTOM│
                  │ • Fallback Cascade DAG Engine                       │
                  │ • Immediate HTTP 429 Shedding or Path Mutation      │
                  └──────────────────────────▲──────────────────────────┘
                                             │ gRPC envoy.service.ext_proc.v3
                  ┌──────────────────────────┴──────────────────────────┐
                  │        Envoy Proxy / GCP Cloud Service Extensions   │
                  └──────────────────────────┬──────────────────────────┘
                                             │ HTTPS Forward
                                             ▼
                      https://<region>-aiplatform.googleapis.com:443
```

---

## 3. Control Plane Architecture

The Control Plane executes out-of-path on a 30-second reconciliation cycle, isolating external GCP API latencies from production traffic.

### 3.1 Components

1. **GCP Quota Ingestion**:
   - Polls regional and project limits via the [GCP Cloud Quotas API](https://cloud.google.com/docs/quotas/overview).
   - Queries 5-minute rolling rate metrics from Cloud Monitoring for request counts and token consumption.
   - Falls back to configured static limits if APIs are unreachable or empty.

2. **Dynamic Headroom Calculator**:
   - Reconciles project-level quotas against organization-wide Dynamic Shared Quota (DSQ) allocations.
   - Uses integer arithmetic to prevent floating-point drift:
     $$\text{Usable Capacity} = \frac{\text{Max} \times (100 - \text{SafetyMarginPercent})}{100}$$
     $$\text{Headroom} = \text{Usable Capacity} - \text{Current Usage}$$

3. **In-Memory Store & gRPC Discovery Service (`QuotaDiscoveryService`)**:
   - Stores the latest snapshot atomically.
   - Exposes `StreamQuotas` (server-streaming gRPC) to broadcast snapshot updates to connected Data Plane nodes. Drops evicted slow consumers to prevent memory leaks.

---

## 4. Data Plane Architecture & Runtime

The Data Plane operates synchronously in the request path with sub-millisecond evaluation overhead.

### 4.1 Request Lifecycle (`ext_proc`)
- **Mode**: Header-only inspection (`request_header_mode: SEND`, `response_header_mode: SKIP`, zero body buffering).
- **Pipeline**:
  1. **Parse**: Extract project, region, and model from `:path` via `pkg/router`.
  2. **Classify**: Inspect `X-Request-Priority` header (`critical`, `best_effort`, `custom`).
  3. **Acquire**: Check quota via local `TokenBucket` (`TryAcquire`).
  4. **Act**:
     - If capacity is available: Pass through without modification.
     - If saturated and `best_effort`: Terminate locally with HTTP 429 (`Retry-After: 5s`).
     - If saturated and `critical` or `custom`: Traverse cascade steps, rewrite `:path` and `host` headers to fallback model/region, and forward upstream.

### 4.2 Priority Tiers

| Priority Tier | Typical Workload | Saturated / High-Load Behavior |
| :--- | :--- | :--- |
| **`critical`** | Interactive user chat, production checkout, real-time agents | Never dropped proactively. Cascades through fallback models/regions; fails open to primary if exhausted. |
| **`best_effort`** | Batch indexing, synthetic test generation, offline eval | Dropped immediately with HTTP 429 when utilization exceeds 70% or headroom is zero. Never allowed to consume fallback model headroom. |
| **`custom`** | Workloads with explicit compliance, region, or cost constraints | Evaluated against a user-defined cascade DAG specified in `config/governor.yaml`. |

### 4.3 Fallback Cascade Configuration

Every cascade step requires an explicit `target_model` and `region`:

```yaml
# config/governor.yaml
custom_policies:
  quality_first:
    cascade:
      - target_model: "gemini-3.0-pro"
        region: "us-central1"
      - target_model: "gemini-3.5-flash"
        region: "us-central1"
      - target_model: "gemini-3.5-flash"
        region: "us-east4"
```

---

## 5. Token Estimation & Reconciliation Pipeline (TPM)

To enforce Tokens-Per-Minute (TPM) ceilings without payload buffering:

1. **Ingress (Pre-Admission)**:
   - Estimates prompt tokens from `Content-Length` or `X-Prompt-Tokens` header plus an output reservation buffer.
   - Deducts estimated tokens from the local TPM bucket via integer math.
2. **Streaming Egress (Zero-Buffering)**:
   - Response streams directly to client (TTFT preserved).
   - Proxy inspects the final SSE chunk containing Google's `usageMetadata` (`promptTokenCount`, `candidatesTokenCount`) and refunds or true-ups the delta.
3. **Control Plane (Periodic True-Up)**:
   - Cloud Monitoring usage is fetched every 30 seconds to calibrate local counters against live backend state using pessimistic minimum reconciliation (`min(localRemaining, gcpHeadroom)`).

---

## 6. Capacity Management: PayGo & Provisioned Throughput

- **Dynamic Shared Quotas (PayGo DSQ)**: GCP dynamically shares organizational quota across projects. The Governor's safety margin shields interactive workloads from bursting background jobs, preventing GCP-level throttling.
- **Provisioned Throughput (PT)**: Organizations with dedicated GenAI Scale Units (GSUs) can configure the Governor to direct critical traffic to reserved model/region endpoints, while routing burstable traffic to PayGo endpoints using the `X-Vertex-AI-LLM-Request-Type` header.

---

## 7. Ingress Topologies

| Topology | Mechanism | Latency Overhead | Ideal Deployment |
| :--- | :--- | :--- | :--- |
| **A. Self-Hosted Envoy** | VPC Private DNS overrides `*.aiplatform.googleapis.com` to Internal Load Balancer VIP pointing to Envoy. | < 1ms | Self-managed GKE clusters, Cloud Run VPC egress. |
| **B. Google Cloud Service Extensions** | Application Load Balancer callout to Governor `ext_proc` service. | 5ms – 15ms | Centralized multi-VPC ingress without sidecar proxies. |
| **C. Agent Gateway** | Envoy-based gateway with native AI routing and Governor filter. | 1ms – 3ms | Managed AI transits and multi-agent hub platforms. |

---

## 8. References

- [Envoy External Processing Filter Specification](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter)
- [Google Cloud Service Extensions Overview](https://cloud.google.com/service-extensions/docs/overview)
- [Google Cloud DNS Private Zones](https://cloud.google.com/dns/docs/zones/zones-overview)
- [Google Cloud Quotas API Documentation](https://cloud.google.com/docs/quotas/overview)
- [Vertex AI Generative AI Quotas & Limits](https://cloud.google.com/vertex-ai/generative-ai/docs/quotas)
- [Google Cloud Private Google Access](https://cloud.google.com/vpc/docs/private-google-access)

