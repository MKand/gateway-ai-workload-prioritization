# Gemini Quota Governor: System Architecture

## 1. Summary

The **Gemini Quota Governor** is a high-performance, priority-aware traffic management and dynamic routing system for Google Cloud Platform (GCP) Vertex AI workloads.

Built with **Go 1.25+**, the system strictly decouples the high-speed synchronous request path (**Data Plane**) from the asynchronous quota discovery and configuration path (**Control Plane**). Standardizing on the open-source **Envoy `ext_proc` gRPC specification** (`envoy.service.ext_proc.v3`), it runs with identical binary logic across both **Self-Hosted Envoy (via VPC Private DNS Interception)** and **Google-Managed Ingress (GCP Agent Gateway / Cloud Service Extensions)**.

---

## 2. End-to-End System Architecture

```
                                  CONTROL PLANE (Go 1.25+, Async Reconciler)
                        ┌─────────────────────────────────────────────────────────────┐
                        │  Governor Control Plane Controller                          │
                        │                                                             │
                        │  • 1. GCP Quota Ingestion: Cloud Quotas & Monitoring APIs   │
                        │  • 2. Dynamic Headroom Calculator: DSQ 3.0 Model Ceilings   │
                        │  • 3. Declarative Policy Watcher: Hot-reloads governor.yaml │
                        │  • 4. State Publisher: Lock-free atomic snapshot swap       │
                        └──────────────────────────────┬──────────────────────────────┘
                                                       │ (Atomic Pointer Push)
═══════════════════════════════════════════════════════╪══════════════════════════════════════════════
                                                       │
                                  DATA PLANE (Go 1.25+, Sub-Millisecond ext_proc)
                                                       │
                                  ┌────────────────────┴────────────────────┐
                                  │   Governor ext_proc Engine (Go 1.25+)   │
                                  │   • Local In-Memory Token Buckets       │
                                  │   • Priority Inspection (critical vs    │
                                  │     best-effort)                        │
                                  │   • Fallback Cascade DAG Engine         │
                                  │   • Immediate HTTP 429 Local Replies    │
                                  └────────────────────▲────────────────────┘
                                                       │
                              gRPC envoy.service.ext_proc.v3 (Shared Interface)
                                                       │
          ┌────────────────────────────────────────────┴────────────────────────────────────────────┐
          │                                                                                         │
   [ Topology A: Self-Hosted Envoy ]                                         [ Topology B: GCP Agent Gateway / GCLB ]
   • Intercepts via VPC Private DNS                                          • Managed Ingress Layer
   • Envoy Proxy container in GKE/Cloud Run                                  • Google Cloud Service Extensions Callout
          │                                                                                         │
          └────────────────────────────────────────────┬────────────────────────────────────────────┘
                                                       │ (Forwarded Request)
                                                       ▼
                       [ https://us-central1-aiplatform.googleapis.com:443 (Vertex AI) ]
```

---

## 3. Control Plane Architecture

The Control Plane executes asynchronously on macro timescales (30s intervals) and is completely isolated from the synchronous client request path.

```
┌─────────────────────────────────────────────────────────────────────────────────────────────────┐
│                                   GOVERNOR CONTROL PLANE                                        │
│                                                                                                 │
│  ┌─────────────────────────┐     ┌─────────────────────────┐     ┌───────────────────────────┐  │
│  │ 1. GCP Quota Ingestion  │     │ 2. Dynamic Headroom     │     │ 3. Declarative Policy     │  │
│  │    Engine               │     │    Calculator           │     │    Watcher                │  │
│  │ • Cloud Quotas API      │────►│ • Reconciles DSQ 3.0    │◄────│ • Watches governor.yaml   │  │
│  │ • Cloud Monitoring API  │     │ • Computes local bucket │     │ • Validates Fallback DAGs │  │
│  │ • Multi-Project Scopes  │     │   capacities & buffers  │     │ • Hot-reloads on change   │  │
│  └─────────────────────────┘     └────────────┬────────────┘     └───────────────────────────┘  │
│                                               │                                                 │
│                                               ▼                                                 │
│                                  ┌─────────────────────────┐                                    │
│                                  │ 4. State Sync & Push    │                                    │
│                                  │ • Atomic Pointer Swap   │                                    │
│                                  │ • In-Memory Snapshot    │                                    │
│                                  └────────────┬────────────┘                                    │
└───────────────────────────────────────────────┼─────────────────────────────────────────────────┘
                                                │ (Lock-Free State Push)
                                                ▼
                        [ Data Plane ext_proc Workers (In-Memory) ]
```

### 1. GCP Quota Ingestion Engine
* **API Integration**: Integrates with GCP client libraries (`cloud.google.com/go/cloudquotas/v1` and `cloud.google.com/go/monitoring/apiv3`).
* **Ceiling & Usage Queries**:
  * Periodically polls effective limits from the [GCP Cloud Quotas API](https://cloud.google.com/docs/quotas/overview).
  * Queries 1-minute rate metrics from Cloud Monitoring: `aiplatform.googleapis.com/quota/generate_content_tokens/usage`.
* **Jittered Polling**: Uses a 30-second interval with jitter and exponential backoff to avoid consuming GCP quota management rate limits.

### 2. Dynamic Headroom & Safety Margin Calculator
Under Vertex AI **Dynamic Shared Quotas (DSQ 3.0)**, quota capacity is pooled and shared at the **GCP Organization** and **Billing Account** level rather than being static per-project buckets. 

#### A. How Org-Level Quotas are Determined
The organization's quota tier is dynamically calculated by GCP based on the **rolling 30-day spend** across all billing accounts in the organization. Each tier defines a **Baseline Throughput** limit for "Critical" traffic (protected from fair-share throttling) in Tokens Per Minute (TPM):

> [!WARNING]
> **Disclaimer**: The spend thresholds, tiers, and baseline throughput allocations defined below are subject to change by Google Cloud Platform in the future. The Reconciler is designed to adapt to these changes dynamically by fetching limits at runtime rather than hardcoding thresholds.


| Model Family | Tier | Spend (30 Days) | Critical Traffic TPM (Org-Level) |
| :--- | :--- | :--- | :--- |
| **Pro Models** | Tier 1 <br> Tier 2 <br> Tier 3 | $10 - $250 <br> $250 - $2,000 <br> > $2,000 | 500,000 <br> 1,000,000 <br> 2,000,000 |
| **Flash / Flash-Lite** | Tier 1 <br> Tier 2 <br> Tier 3 | $10 - $250 <br> $250 - $2,000 <br> > $2,000 | 2,000,000 <br> 4,000,000 <br> 10,000,000 |

*   **Opportunistic Bursting**: Traffic exceeding the baseline is "Sheddable Plus" and subject to standard GCP `429` throttling during high congestion.
*   **Safety Margin**: The Reconciler retains a configurable safety buffer (default `5%` or `0.05` to `0.3` for tests) to shed or fallback traffic *before* hitting these limits:
    $$\text{Usable Headroom} = (\text{Effective Quota Ceiling} \times (1 - \text{SafetyMargin})) - \text{GCP Cloud Usage}$$

### 3. Declarative Policy Watcher
* **Configuration Hot-Reload**: Uses `fsnotify` in Go to monitor `config/governor.yaml`.
* **DAG Validation**: Validates fallback cascades, verifies model names, and detects circular dependency graphs.
* **Zero-Downtime Reload**: Compiles new policy trees in memory and publishes them without dropping in-flight connections.

### 4. gRPC-Based State Synchronization & Lock-Free Push
Since the Control Plane and Data Plane execute as **separate processes** (to isolate the heavy reconciler network logic from the sub-millisecond gRPC proxy path), they synchronize state over the network:

1.  **gRPC Streaming Interface**: The Control Plane runs a **`QuotaDiscoveryService`** gRPC server.
2.  **Streaming Push**: Every Data Plane node establishes a server-streaming gRPC connection (`StreamQuotas`) to the Control Plane.
3.  **Atomic Pointer Swap (Local)**: Whenever the Reconciler calculates a new snapshot, it serializes it as a Protobuf message and streams it to all connected Data Planes. Upon receiving the stream payload, the Data Plane node performs a lock-free **`atomic.Pointer` swap** locally in its memory space, ensuring worker threads can read the quotas in sub-microseconds without Mutex locking.

```go
type QuotaSnapshot struct {
    OrgQuotas     map[string]*ModelQuota
    ProjectQuotas map[string]*ModelQuota
    LastSyncedAt  time.Time
}

// Data plane workers read this pointer atomically on every request without blocking:
var globalState atomic.Pointer[QuotaSnapshot]
```

### 5. Telemetry & Metric Exporter
* Exposes a Prometheus `/metrics` endpoint on port `9090`:
  * `governor_quota_utilization_ratio{model="gemini-3.5-pro", region="us-central1"}`
  * `governor_requests_shed_total{priority="best-effort", reason="quota_exhausted"}`
  * `governor_cascade_fallbacks_total{from="gemini-3.5-pro", to="gemini-3.5-flash"}`
* Emits OpenTelemetry traces correlated with `X-Request-Id`.

---

## 4. Data Plane Architecture & Runtime

The Data Plane executes the synchronous request path in **< 1ms**, evaluating admission, priority shedding, and model fallback before traffic exits to Google.

### 1. `ext_proc` gRPC Handler Lifecycle
* Implements `envoy.service.ext_proc.v3.ExternalProcessorServer`.
* **Header-Only Mode**: Configured with `request_header_mode: SEND` and `response_header_mode: SKIP`.
* **Zero-Buffering**: Does not buffer prompt or streaming response bodies (`request_body_mode: NONE`, `response_body_mode: NONE`), preserving Time-To-First-Token (TTFT).

### 2. Priority Taxonomy & Traffic Classes

| Priority Tier | Target Workloads | Quota & Degradation Behavior |
| :--- | :--- | :--- |
| **`critical`** | Interactive user chat, real-time agent execution, checkout workflows. | **100% Protected**: Traverses default fallback cascades (e.g. 3.5 Pro $\to$ 3.5 Flash $\to$ 3.0 Flash $\to$ Regional Failover) before ever dropping. |
| **`best_effort`** | Offline document indexing, synthetic test generation, benchmark sweeps, batch evals. | **Early Shedding**: Throttled immediately with `HTTP 429 (Retry-After: 5s)` when quota crosses 70%. Never allowed to cascade down and consume lighter model headroom. |
| **`custom`** | Workloads with specific quality, cost, or regulatory routing requirements. | **Custom Cascade DAG**: Follows an explicit fallback policy specified in headers (e.g., `X-Fallback-Policy: quality_first` or `X-Fallback-Policy: cost_optimized`). |

### 3. Configurable Fallback Cascade Engine

```yaml
# config/governor.yaml
default_policy: "best_effort"  # Default policy for all requests that don't specify a specific X-Request-Priority header
custom_policies:
    custom1:                   # Failover to lower tier models when the requested model is full, but remain in the same region
        cascade:
          - target_model: "gemini-3.5-pro"
          - target_model: "gemini-3.5-flash"
          - target_model: "gemini-3.0-pro"
          - target_model: "gemini-3.0-flash"
    custom2:                   # Failover to different regions when the requested region is full, but don't change the model
        cascade:
          - region: "us-east4"
          - region: "europe-west4"
          - region: "asia-northeast1"
          - region: "asia-southeast1"

```

---

## 5. Token Estimation & Reconciliation Pipeline (TPM Handling)

To enforce Tokens-Per-Minute (TPM) ceilings without payload buffering:

```
1. INGRESS (Pre-Admission)
   • Inspects Content-Length or X-Prompt-Tokens header
   • Reserves: (Estimated Prompt Tokens + Buffer Output Tokens) from TPM Bucket
   • If remaining TPM < Reservation ──► Sheds best-effort requests
          │
          ▼
2. STREAMING EGRESS (Zero-Buffering Pass-Through)
   • Tokens stream directly to user (TTFT preserved)
   • Proxy taps the FINAL SSE chunk containing Google's `usageMetadata`:
     { "promptTokenCount": 1200, "candidatesTokenCount": 450, "totalTokenCount": 1650 }
   • Reconciles exact delta against the local token bucket
          │
          ▼
3. CONTROL PLANE (Async True-Up every 30s)
   • Queries Cloud Monitoring: `aiplatform.googleapis.com/quota/generate_content_tokens/usage`
   • Re-calibrates local TPM buckets against Google's live backend Spanner allocations
```

---

## 6. Pluggable Ingress Topologies

* **Invocation**: GCP makes an internal `ext_proc` gRPC callout to the Go Governor service running on Cloud Run or GKE. 

### Topology A: Self-Hosted Envoy + VPC Private DNS
* **Ingress**: [GCP Cloud DNS Private Zone](https://cloud.google.com/dns/docs/zones/zones-overview#private_zones) overrides `*.aiplatform.googleapis.com` to the Internal Load Balancer VIP. The proxy will be hosted as a [Envoy Proxy](https://www.envoyproxy.io/) running on Cloud Run or GKE.

### Topology B: Google-Managed Ingress (Cloud Service Extension Callout)
* **Ingress**: Uses [Google Cloud Service Extensions](https://cloud.google.com/service-extensions/docs/overview) attached directly to an Application Load Balancer as a `Service Extension Callout`.

### Topology C: Google-Managed Ingress (GCP Agent Gateway / Cloud Service Extensions)
* **Ingress**: Uses [Google Cloud Service Extensions](https://cloud.google.com/service-extensions/docs/overview) attached directly to an Application Load Balancer or Agent Gateway as a `Service Extension WASM plugin`

---

## 7. References & External Documentation

* [Envoy External Processing Filter Specification](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter)
* [Google Cloud Service Extensions Overview](https://cloud.google.com/service-extensions/docs/overview)
* [Google Cloud DNS Private Zones](https://cloud.google.com/dns/docs/zones/zones-overview#private_zones)
* [Google Cloud Quotas API Documentation](https://cloud.google.com/docs/quotas/overview)
* [Vertex AI Generative AI Quotas & Limits](https://cloud.google.com/vertex-ai/generative-ai/docs/quotas)
* [Google Cloud Private Google Access](https://cloud.google.com/vpc/docs/private-google-access)
