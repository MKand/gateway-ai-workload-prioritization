# Project Overview & Architectural Goals: Gemini Priority Quota Governor

## 1. Executive Summary & Vision

The **Gemini Priority Quota Governor** is a high-performance, priority-aware traffic control and dynamic routing gateway designed for Google Cloud Platform (GCP) Vertex AI and Gemini model workloads.

The request priority is set by the invoking application or user within the organization (e.g. via `X-Request-Priority`), and the Governor enforces that priority in real-time (< 1ms data-plane overhead) to eliminate LLM quota contention, noisy-neighbor starvation, and hard HTTP 429 rejections across multi-tenant enterprise agent fleets.

---

## 2. How Google Implements Rate Limiting for Gemini Models

Google enforces Gemini rate limits across two distinct planes: a macro **Control Plane** at the GCP Organization/Billing level and an in-flight **Data Plane** in the Vertex AI model serving backend. Under **Dynamic Shared Quota (DSQ 3.0)**, Google calculates shared Requests-Per-Minute (RPM) and Tokens-Per-Minute (TPM) capacity ceilings dynamically based on total 30-day rolling spend across all projects in the billing account.

Because edge proxies (GFE/ESF) cannot tokenize variable text/multimodal prompts or predict output tokens in advance, quota deduction happens directly in the **Vertex AI model serving backend**. The model backend tokenizes the prompt, queries shared Spanner quota counters, schedules TPU accelerator compute, and deducts output tokens as they stream back. When regional cluster utilization crosses 70–80%, unreserved Pay-As-You-Go requests are uniformly throttled with HTTP `429 Too Many Requests`.

---

## 3. Core Problem

In enterprise multi-tenant environments, multiple development teams, autonomous agent swarms, and background batch jobs share a common GCP project or organization-level Vertex AI quota allocation. Without priority-aware traffic governance:

1. **Noisy Neighbor Starvation**: Unthrottled background batch workloads (e.g. nightly vector embeddings, synthetic evals, offline indexing) burst and consume shared RPM/TPM ceilings, causing fatal HTTP `429` errors for critical interactive user agents.
2. **Priority Blindness in Upstream APIs**: Foundation model APIs treat every incoming request identically. A non-urgent batch prompt has the exact same scheduling priority as an executive demo or customer checkout transaction.
3. **Hard Drops vs. Graceful Degradation**: When quotas saturate, endpoints drop traffic uniformly rather than intelligently falling back (e.g., model downgrade or cross-region failover).
4. **Data-Plane Latency Constraints**: Control-plane quota lookups cannot run synchronously in-path without severely degrading Time-To-First-Token (TTFT).

---

## 4. Gap Analysis

| Existing Approach | Capabilities | Critical Gaps Solved by Gemini Quota Governor |
| :--- | :--- | :--- |
| **Native Vertex AI API** | High throughput, serverless endpoint management. | **Hard binary drops**: No concept of per-request priority; drops all incoming traffic equally during saturation; no dynamic model/regional fallback. |
| **Traditional API Gateways (Apigee / Kong)** | Enterprise auth, static rate limiting, spike arrest. | **Static client limits**: Enforces fixed client quotas, completely unaware of real-time upstream GCP quota headroom; no LLM token-aware degradation. |
| **Open-Source LLM Proxies (LiteLLM / Envoy AI Gateway)** | Model routing, load balancing, provider fallbacks. | **Decoupled from GCP Quotas**: Relies on synthetic local counters (often Redis-backed); lacks native asynchronous synchronization with live GCP Cloud Quotas/Monitoring baselines. |
| **Developer Agent Hubs (Antigravity Web Hub)** | Agent workspace management, UI session orchestration. | **Control-plane only**: Manages developer workflows and agent tasks, but does not govern the underlying L7 network traffic or protect shared cloud quotas. |
| **GCP Cloud Quotas API & Auto Quota Adjusters** | Programmatic quota adjustments and automated ceiling scaling. | **Macro control-plane only**: Adjustments take minutes to hours and scale total limits uniformly without request-priority awareness or real-time burst protection. |
| **Vertex AI MaaS QoS Tiers (Pinnacle Quota Allowlisting / PayGo-Low)** | Internal TPU priority queues (`PayGo-High`, `PayGo-Low`). | **Static internal allowlist**: Configured manually per-project in Spanner by Google SREs/PMs; invisible to consumers and cannot be tagged dynamically per request. |

---

## 5. Proposed Priority Tiers & Dynamic Routing Policy

The Gemini Quota Governor inspects the user-specified priority header on every incoming request (`X-Request-Priority: 1 | 2 | 3 | 4`) and enforces progressive traffic management:

```
[ Incoming Request with X-Request-Priority ]
   │
   ├── Priority 1 (Critical)      ──► FAST PATH: Sent through to requested model in primary region
   │
   ├── Priority 2 (Model Fallback) ──► DEGRADE: Sent to lighter/lower-tier model in SAME region (Pro ➔ Flash)
   │
   ├── Priority 3 (Regional Failover)──► REROUTE: Sent to SAME model in a DIFFERENT region (us-central1 ➔ us-east4)
   │
   └── Priority 4 (Batch / Low)   ──► SHED: Dropped immediately with HTTP 429 & Retry-After header
```

### Policy Definitions:

* **Priority 1 (Critical / Interactive)**:
  * **Behavior**: **All requests are sent through** directly to the target model in the primary region.
  * **Guarantee**: Never dropped or degraded; 100% of remaining quota headroom is reserved for Priority 1 during saturation events.
* **Priority 2 (High / Model Fallback in Same Region)**:
  * **Behavior**: When the primary model's TPM/RPM quota is under pressure (>80%), requests are dynamically rewritten and sent to a **lower-tier model in the same region** (e.g. downgrading `gemini-1.5-pro` $\to$ `gemini-1.5-flash`).
  * **Outcome**: Conserves heavy accelerator tokens while completing the request in-region with minimal latency impact.
* **Priority 3 (Normal / Cross-Region Failover)**:
  * **Behavior**: When local regional capacity is saturated, requests are dynamically dispatched to a **different region for the same model** (e.g. `us-central1` $\to$ `us-east4` or `europe-west4`) where organizational quota headroom remains available.
  * **Outcome**: Preserves model intelligence tier while routing around localized regional contention.
* **Priority 4 (Batch / Non-Urgent Load Shedding)**:
  * **Behavior**: Under elevated organizational quota load, requests are **immediately dropped (HTTP 429)** with a `Retry-After` header.
  * **Outcome**: Completely shields interactive and real-time agents from being starved by offline background workloads.

---

## 6. Deployment Topologies

* **Edge / Perimeter (L7 GCLB)**: Cloud Service Extensions (`ext_proc` callouts) at Google Cloud Load Balancer ingress.
* **AI Transit Gateway**: Dedicated Envoy AI Gateway cluster inside the shared VPC.
* **Host / Sidecar**: In-pod Envoy sidecar for microservices requiring sub-0.5ms evaluation.
