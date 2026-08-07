# MVP Proposal: Priority-Aware Gemini Quota Governor (Architecture 1 + Pluggable Data Plane)

## 1. Executive Summary

This proposal outlines the Minimal Viable Product (MVP) design, implementation milestones, and verification plan for the **Gemini Quota Governor**.

Built with **Go 1.25+**, the Governor decouples a high-performance **Data Plane** from an asynchronous **Control Plane**. It standardizes on the open-source **Envoy `ext_proc` gRPC specification** (`envoy.service.ext_proc.v3`), enabling a unified Go governor engine that deploys seamlessly across both **Self-Hosted Envoy (via VPC Private DNS Interception)** and **Google-Managed Ingress (GCP Agent Gateway / Cloud Service Extensions)** with zero code changes to client applications or agent SDKs (such as ADK and `google-genai`).

---

## 2. Decoupled Control Plane vs. Pluggable Data Plane Architecture

```
                                  CONTROL PLANE (Go 1.25+, Async Reconciler)
                        ┌─────────────────────────────────────────────────────────────┐
                        │  Governor Control Plane Controller                          │
                        │                                                             │
                        │  • Polls GCP Cloud Quotas API (cloudquotas.googleapis.com)  │
                        │  • Ingests Cloud Monitoring usage metrics                   │
                        │  • Dynamically calculates regional & model ceilings         │
                        │  • Pushes live bucket limits & policies to Data Plane       │
                        └──────────────────────────────┬──────────────────────────────┘
                                                       │ (Async State Sync Channel)
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

### Data Plane Engine (Go 1.25+)
* Standardizes on `envoy.service.ext_proc.v3.ExternalProcessorServer`.
* Evaluates `X-Request-Priority` headers against in-memory token buckets in **< 0.5ms**.
* Returns immediate `HTTP 429 (Retry-After: 5s)` local replies for `best-effort` traffic during quota saturation.
* Mutates the request `:path` header to execute fallback cascades (e.g. `gemini-3.5-pro` $\to$ `gemini-3.5-flash`) for `critical` traffic.
* Operates in **Header-Only, Zero-Buffering mode**, preserving Time-To-First-Token (TTFT) performance (< 1ms overhead).

### Pluggable Ingress Topologies
1. **Topology A: Self-Hosted Envoy + VPC Private DNS (MVP Baseline)**:
   - Zero application code changes.
   - Envoy terminates TLS via an internal CA certificate and forwards traffic to Vertex AI.
2. **Topology B: GCP Agent Gateway / Cloud Service Extensions**:
   - Uses GCP-managed load balancing / Agent Gateway.
   - GCP invokes the same Go `ext_proc` engine via an internal `LbTrafficExtension` callout.

### Control Plane Controller (Go 1.25+)
* Periodically polls the [GCP Cloud Quotas API](https://cloud.google.com/docs/quotas/overview) and Cloud Monitoring metrics every 30 seconds.
* Dynamically updates in-memory token bucket limits on data-plane instances without restarts.
* Exposes declarative policy reloading for fallback cascades (`config/governor.yaml`).

---

## 3. Priority Taxonomy & Traffic Classes

| Priority Tier | Industry Names | Target Workloads | Quota & Degradation Behavior |
| :--- | :--- | :--- | :--- |
| **`critical`** | Guaranteed / Protected | User-facing interactive chat, real-time agent execution, checkout workflows. | **100% Protected**: Never shed unless total fleet capacity is exhausted. Automatically traverses global default fallback cascades (e.g. 3.5 Pro $\to$ 3.5 Flash $\to$ 3.0 Flash $\to$ Regional Failover). |
| **`best-effort`** | Sheddable / Preemptible / Opportunistic | Offline document indexing, synthetic test generation, benchmark sweeps, batch evals. | **Early Shedding**: Throttled immediately with `HTTP 429 (Retry-After: 5s)` when quota crosses 70%. Never allowed to cascade down and consume lighter model headroom. |
| **`custom`** | Policy-Driven / Tenant-Defined | Workloads with specific quality, cost, or regulatory routing requirements. | **Custom Cascade DAG**: Follows an explicit fallback policy specified in headers (e.g., `X-Fallback-Policy: quality_first` or `X-Fallback-Policy: cost_optimized`). |

---

## 4. Configurable Fallback Cascade Engine

Fallback policies are defined declaratively in `config/governor.yaml`:

```yaml
# Fallback Cascades & Degradation Policies
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

## 5. Token Estimation & Reconciliation Pipeline (TPM Handling)

Tokens-Per-Minute (TPM) quota limits (Input Prompt Tokens + Output Completion Tokens) are enforced through a **3-Layer Estimation & Reconciliation Pipeline** that avoids buffering and preserves streaming TTFT:

```
1. INGRESS (Pre-Admission < 0.1ms)
   • Inspects Content-Length or X-Prompt-Tokens header
   • Reserves: (Estimated Prompt Tokens + 500 Output Buffer) from TPM Bucket
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

1. **Pre-Admission Reservation (< 0.1ms)**:
   The proxy calculates an upfront token reservation using either client-declared headers (`X-Prompt-Tokens`) or byte heuristics ($\text{Bytes} / 4$) plus an average completion buffer, deducting capacity before admitting the request.
2. **Final Response Chunk Tapping (Exact Local Accounting)**:
   As the stream closes, the proxy inspects the `usageMetadata` object in the final Server-Sent Event (SSE) chunk, adjusting the token bucket with the exact token count.
3. **Control Plane Asynchronous True-Up (Ground Truth)**:
   The Control Plane continuously ingests rolling 1-minute usage metrics from Cloud Monitoring (`aiplatform.googleapis.com/quota/generate_content_tokens/usage`) every 30 seconds, correcting drift across distributed proxy replicas.

---

## 6. Scope Boundary & Ingress Scope

* **In-Scope for MVP**: Workloads where outbound Vertex AI calls are routed through the VPC Private DNS zone (GKE pods, Compute VMs, serverless workloads with VPC egress, and VPN-connected developer workstations), or fronted by GCP Agent Gateway.
* **Out-of-Scope**: Un-routed calls originating outside the VPC Private DNS boundary (e.g. disconnected home Wi-Fi workstations without VPN).

---

## 7. Phased Implementation Plan

### Phase 1: Local Standalone Prototype & Fallback Engine (Days 1–3)
* Implement Go 1.25+ `ext_proc` Data Plane service with local token bucket and cascade evaluation.
* Implement asynchronous Control Plane reconciler loop.
* Configure local Envoy with `ext_proc` filter in header-only mode.
* Create local Docker Compose test environment and synthetic load benchmark generator.

### Phase 2: GCP Cloud Infrastructure & Ingress Wiring (Days 4–6)
* Provision Terraform definitions for:
  - **Cloud DNS Private Zone**: `aiplatform.googleapis.com.` $\to$ Internal LB VIP.
  - **Internal Load Balancer (ILB)** with backend service pointing to the Data Plane.
  - **GCP Service Extension Callout**: Optional wiring for Agent Gateway / GCLB.
  - **GKE / Cloud Run Deployment**: Running Envoy and Governor services.
* Issue and install an internal TLS certificate for `*.aiplatform.googleapis.com`.
* Configure Envoy upstream DNS resolution via public DNS (`8.8.8.8`) to prevent routing loops.

### Phase 3: Cloud Quotas API Integration & Dynamic Cascade (Days 7–9)
* Integrate live [GCP Cloud Quotas API](https://cloud.google.com/docs/quotas/overview) polling in the Control Plane (`cloudquotas.googleapis.com`).
* Verify dynamic cascade rewrites (e.g. `gemini-3.5-pro` $\to$ `gemini-3.5-flash` $\to$ `gemini-3.0-flash`) under simulated quota saturation.

### Phase 4: Benchmarking, Validation & Documentation (Days 10–12)
* Run end-to-end benchmark with 500 concurrent streaming agents generating 200% quota overload.
* Validate TTFT latency overhead (< 1ms vs direct Vertex AI calls).
* Package runbooks, deployment guides, and benchmark reports.

---

## 8. Success Criteria & Quantitative Metrics

| Metric | Target Goal | Verification Method |
| :--- | :--- | :--- |
| **SDK Compatibility** | **0 lines of code modified** | Standard ADK and `google-genai` scripts execute without endpoint overrides. |
| **Ingress Interoperability** | **100% ext_proc parity** | Same Go engine runs with both Self-Hosted Envoy and GCP Agent Gateway. |
| **Critical Traffic Drop Rate** | **0.0% HTTP 429s** | Under 200% aggregate burst load, 100% of `critical` priority requests succeed. |
| **Best-Effort Shedding Accuracy** | **100% shed above 70% load** | Low-priority traffic receives immediate HTTP 429 with valid `Retry-After` headers. |
| **TTFT Latency Overhead** | **< 1.0 millisecond** | Compare p50/p95 Time-To-First-Token on streaming requests (Direct vs. Gateway). |

---

## 9. References & External Documentation

* [Envoy External Processing Filter Specification](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter)
* [Google Cloud Service Extensions Overview](https://cloud.google.com/service-extensions/docs/overview)
* [Google Cloud DNS Private Zones](https://cloud.google.com/dns/docs/zones/zones-overview#private_zones)
* [Google Cloud Quotas API Documentation](https://cloud.google.com/docs/quotas/overview)
* [Vertex AI Generative AI Quotas & Limits](https://cloud.google.com/vertex-ai/generative-ai/docs/quotas)
* [Google Cloud Private Google Access](https://cloud.google.com/vpc/docs/private-google-access)
