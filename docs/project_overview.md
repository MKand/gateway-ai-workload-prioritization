# Project Overview & Gap Analysis: Gemini Priority Quota Governor

## 1. Executive Summary & Vision

The **Gemini Priority Quota Governor** is a high-performance, priority-aware traffic management and dynamic routing gateway for Google Cloud Platform (GCP) Vertex AI and Gemini workloads.

Its mission is to **eliminate LLM quota contention, noisy-neighbor starvation, and hard HTTP 429 rejections** across multi-tenant enterprise agent fleets by introducing sub-millisecond, priority-tiered traffic governance and dynamic model/region fallbacks.

---

## 2. The Core Problem: Quota Contention in Multi-Tenant Agent Fleets

In enterprise AI deployments, dozens of disparate applications, autonomous agent swarms (e.g. ADK, LangChain), and batch jobs share a common GCP project or organization-level Vertex AI quota allocation (Requests-Per-Minute / RPM and Tokens-Per-Minute / TPM).

Without priority-aware traffic governance:
1. **Noisy-Neighbor Starvation**: An unthrottled background batch job (e.g., bulk document indexing, synthetic test generation, offline evaluation) can burst and exhaust shared RPM/TPM ceilings. This instantly causes fatal HTTP `429 Too Many Requests` errors for latency-critical, customer-facing interactive agents.
2. **Priority Blindness in Foundation Model Endpoints**: Upstream model endpoints (e.g., Vertex AI API) treat all incoming requests uniformly. A non-urgent batch prompt has the exact same scheduling priority as a mission-critical interactive agent turn.
3. **Hard Drops vs. Graceful Degradation**: When limits are reached, standard infrastructure drops traffic uniformly rather than performing adaptive degradation (e.g., shedding batch requests first, temporarily downgrading models, or rerouting to secondary GCP regions).
4. **Data-Plane Latency Constraints**: Querying cloud management APIs synchronously in the request path adds 50ms–200ms+ of latency, severely degrading **Time-To-First-Token (TTFT)**.

---

## 3. How Google & Vertex AI Implement Rate Limiting (DSQ 3.0)

Understanding Google's internal quota architecture highlights why enterprise-side priority governance is necessary:

* **Dynamic Shared Quota (DSQ 3.0)**: Rather than static per-project buckets, DSQ pools capacity across projects and computes baseline quotas dynamically based on 30-day rolling spend at the **GCP Organization and Billing Account level**.
* **Backend Token Enforcement**: Because edge proxies (GFE/ESF) cannot tokenize prompts or predict output tokens in advance, quota deduction happens inside the **Vertex AI backend serving layer** (querying Spanner quota tables and deducting completion tokens as they stream back).
* **Why GCP Quota Adjusters Do Not Solve Contention**: GCP Cloud Quotas API and automated Quota Adjusters operate in the **control plane over minutes to hours** (adjusting administrative ceilings), whereas traffic contention and 429 bursts occur in the **network data plane over milliseconds**.

---

## 4. Gap Analysis

| Solution Category | Examples | Capabilities & Strengths | Critical Gaps Solved by Gemini Quota Governor |
| :--- | :--- | :--- | :--- |
| **Native Cloud Model APIs** | Google Vertex AI, Gemini APIs | High throughput, serverless endpoint management. | **Hard binary limits**: No concept of request priority tiers; drops all incoming traffic equally when quotas saturate; no automated fallback. |
| **Traditional API Gateways** | Apigee, Kong, Apache APISIX | Enterprise auth, static rate limiting, spike arrest. | **Static client quotas**: Enforces fixed client-side rate limits, but is completely unaware of real-time upstream GCP quota headroom; no LLM token-aware degradation. |
| **Open-Source LLM Gateways** | LiteLLM Proxy, Envoy AI Gateway | Multi-provider routing, load balancing, model fallbacks. | **Decoupled from Cloud Quotas**: Relies on synthetic local counters (often Redis-backed); lacks native asynchronous synchronization with live GCP Cloud Quotas/Monitoring baselines. |
| **Developer Agent Hubs** | Antigravity Web Hub, Agent Platforms | Multi-agent orchestration, session management, UI dashboard. | **Control-plane only**: Manages developer workflows, but does not govern the underlying L7 network traffic or protect shared cloud quotas. |
| **GCP Cloud Quotas API & Adjusters** | Cloud Quotas API, Quota Adjuster | Programmatic limit increases. | **Timescale mismatch**: Operates on a 5–30+ minute timescale; cannot make millisecond-level admission or priority shedding decisions during traffic spikes. |
| **Vertex AI MaaS QoS Tiers** | Project Pinnacle Quota Allowlisting | Internal TPU priority queues (`PayGo-High`, `PayGo-Low`). | **Static internal allowlist**: Configured manually per-project in Spanner by Google SREs/PMs; invisible to consumers and cannot be tagged dynamically per request. |

---

## 5. Architectural References

* For the complete Control Plane and Data Plane technical architecture, see [docs/architecture.md](architecture.md).
