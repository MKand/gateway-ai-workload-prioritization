# Gemini Quota Governor: System Architecture

## 1. Executive Summary

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
* Under Vertex AI Dynamic Shared Quotas (DSQ 3.0), quota capacity is pooled and shared across the GCP Organization. The calculator computes usable headroom:
  $$\text{Usable Headroom} = (\text{Effective Quota Ceiling} \times 0.95) - \text{GCP Cloud Usage}$$
* **Safety Margin**: Retains a 5% safety buffer so the Governor always sheds or downgrades traffic *before* Vertex AI's blunt backend 429 occurs.
* **Partitioning**: Computes independent headroom per model family (`gemini-3.5-pro`, `gemini-3.5-flash`, `gemini-3.0-flash`) and per region (`us-central1`, `us-east4`).

### 3. Declarative Policy Watcher
* **Configuration Hot-Reload**: Uses `fsnotify` in Go to monitor `config/governor.yaml`.
* **DAG Validation**: Validates fallback cascades, verifies model names, and detects circular dependency graphs.
* **Zero-Downtime Reload**: Compiles new policy trees in memory and publishes them without dropping in-flight connections.

### 4. Lock-Free State Synchronization Engine
To push state from the Control Plane to Data Plane workers with **zero lock contention**:
* Uses Go 1.25+ `atomic.Pointer[QuotaSnapshot]` for lock-free snapshot replacement:
  ```go
  type QuotaSnapshot struct {
      ModelBuckets   map[string]*TokenBucket
      FallbackDAG    *PolicyGraph
      ActiveRegion   string
      LastSyncedAt   time.Time
  }
  
  // Data plane reads atomically on every request without Mutex blocking:
  var globalState atomic.Pointer[QuotaSnapshot]
  ```
* When new limits are calculated, the Control Plane allocates a new `QuotaSnapshot` and executes a single atomic pointer swap.

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
| **`best-effort`** | Offline document indexing, synthetic test generation, benchmark sweeps, batch evals. | **Early Shedding**: Throttled immediately with `HTTP 429 (Retry-After: 5s)` when quota crosses 70%. Never allowed to cascade down and consume lighter model headroom. |
| **`custom`** | Workloads with specific quality, cost, or regulatory routing requirements. | **Custom Cascade DAG**: Follows an explicit fallback policy specified in headers (e.g., `X-Fallback-Policy: quality_first` or `X-Fallback-Policy: cost_optimized`). |

### 3. Configurable Fallback Cascade Engine

```yaml
# config/governor.yaml
policies:
  # Default policy for 'critical' traffic
  default_cascade:
    - model: "gemini-3.5-pro"
      trigger_threshold: 0.95        # Trigger fallback when 3.5-pro quota > 95%
      cascade:
        - target_model: "gemini-3.5-flash"
        - target_model: "gemini-3.0-flash"
        - target_region: "us-east4"  # Regional failover

    - model: "gemini-3.5-flash"
      trigger_threshold: 0.95
      cascade:
        - target_model: "gemini-3.0-flash"

  # Custom Policies for 'custom' priority tier
  custom_policies:
    quality_first:                   # Preserve model quality via regional failover
      - model: "gemini-3.5-pro"
        cascade:
          - target_region: "us-east4"
          - target_region: "europe-west4"

    cost_optimized:                  # Aggressively downgrade model to save TPM
      - model: "gemini-3.5-pro"
        cascade:
          - target_model: "gemini-3.5-flash"
          - target_model: "gemini-3.0-flash"

    strict_exact:                    # Strict model requirements (no downgrade allowed)
      - model: "gemini-3.5-pro"
        cascade: []                  # Returns 429 if 3.5-pro is full
```

---

## 5. Pluggable Ingress Topologies

### Topology A: Self-Hosted Envoy + VPC Private DNS (Zero Code Changes)
* **Ingress**: [GCP Cloud DNS Private Zone](https://cloud.google.com/dns/docs/zones/zones-overview#private_zones) overrides `*.aiplatform.googleapis.com` to the Internal Load Balancer VIP.
* **TLS**: Envoy terminates TLS with an internal CA certificate trusted by client base containers.
* **Upstream Loop Prevention**: Envoy resolves upstream endpoints via public DNS (`8.8.8.8`) or routes directly to Google Private Access VIPs (`199.36.153.8:443`), bypassing the VPC private zone.

### Topology B: Google-Managed Ingress (GCP Agent Gateway / Cloud Service Extensions)
* **Ingress**: Uses [Google Cloud Service Extensions](https://cloud.google.com/service-extensions/docs/overview) attached directly to an Application Load Balancer or Agent Gateway.
* **Invocation**: GCP makes an internal `ext_proc` gRPC callout to the Go Governor service running on Cloud Run or GKE.
* **Shared Contract**: Reuses the exact same Go Data Plane server with zero code differences.

---

## 6. References & External Documentation

* [Envoy External Processing Filter Specification](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter)
* [Google Cloud Service Extensions Overview](https://cloud.google.com/service-extensions/docs/overview)
* [Google Cloud DNS Private Zones](https://cloud.google.com/dns/docs/zones/zones-overview#private_zones)
* [Google Cloud Quotas API Documentation](https://cloud.google.com/docs/quotas/overview)
* [Vertex AI Generative AI Quotas & Limits](https://cloud.google.com/vertex-ai/generative-ai/docs/quotas)
* [Google Cloud Private Google Access](https://cloud.google.com/vpc/docs/private-google-access)
