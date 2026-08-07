# Quota Governor Proxy: Architecture Challenges & Solutions

## 1. Overview & Context

Deploying an in-path **Gemini Quota Governor** requires placing a high-performance proxy (Envoy with an `ext_proc` gRPC filter) between client workloads and Google Cloud Vertex AI (`aiplatform.googleapis.com`).

While an in-path gateway enables priority-tiered load shedding, dynamic model fallbacks, and centralized quota tracking, intercepting production AI traffic introduces networking, authentication, and developer-workflow considerations across cloud and local environments.

---

## 2. Core Technical Challenges

### A. Hardcoded SDK Endpoints
* **The Problem**: Standard client libraries (such as Google GenAI SDK `google-genai`, Vertex AI Python SDK `google-cloud-aiplatform`, and the Agent Development Kit / ADK) default internally to `https://{region}-aiplatform.googleapis.com` or `https://generativelanguage.googleapis.com`.
* **The Risk**: Requiring engineering teams across an organization to manually modify client constructors or base URLs creates operational friction, breaks default examples, and allows unmanaged scripts to bypass governance.

### B. TLS Termination & Certificate Trust
* **The Problem**: Official SDKs connect over HTTPS (port 443). Intercepting traffic requires the proxy to terminate TLS, inspect HTTP headers (such as `X-Request-Priority`), and establish a new TLS session with Google.
* **The Risk**: Without proper certificate distribution, client runtimes fail with `CERTIFICATE_VERIFY_FAILED` errors.

### C. Upstream DNS Resolution Loops
* **The Problem**: If a VPC's private DNS zone redirects `*.aiplatform.googleapis.com` to the proxy's Internal Load Balancer (ILB) VIP, the proxy itself risks resolving the upstream endpoint back to its own VIP, resulting in an infinite routing loop.

### D. Local Workloads & Developer Tooling (`agy`, CLI, Notebooks)
* **The Problem**: Under Vertex AI Dynamic Shared Quotas (DSQ 3.0), quota capacity is pooled at the **GCP Organization and Billing Account level**. Unthrottled local batch scripts, evaluation runs, or `agy` tasks on developer laptops compete for the exact same RPM/TPM pool as mission-critical production services.
* **The Risk**: If local developers bypass the governor, bursty local experiments can trigger upstream HTTP 429 errors for production workloads.

### E. Latency Overhead & Streaming Compatibility
* **The Problem**: Generative AI relies heavily on Server-Sent Events (SSE) and chunked HTTP/2 streaming for real-time tokens.
* **The Risk**: Buffering request or response bodies at the proxy layer destroys Time-To-First-Token (TTFT) performance.

---

## 3. Solutions by Deployment Environment

### 3.1 Cloud Workloads (Shared VPC / GKE / Cloud Run)

#### Architecture 1: VPC Private DNS Interception (Zero Code Changes)
Transparently routes all outbound Vertex AI traffic to the proxy at the network layer without modifying application code.

```
[ GKE Pod / Cloud Run / Compute VM ]
               │
               │ 1. Calls default: us-central1-aiplatform.googleapis.com:443
               ▼
[ GCP VPC Cloud DNS (Private Zone) ]
               │
               │ 2. Resolves wildcard *.aiplatform.googleapis.com -> 10.10.0.50 (ILB VIP)
               ▼
[ Envoy Transit Gateway (10.10.0.50) ]
               │
               │ 3. Terminate TLS (Internal CA Cert)
               │ 4. ext_proc inspects X-Request-Priority & Token Bucket
               │ 5. Upstream DNS: Queries 8.8.8.8 or routes to 199.36.153.8 (Private Google Access)
               ▼
[ Google Vertex AI Backend ]
```

* **DNS Configuration**:
  Create a [Cloud DNS Private Zone](https://cloud.google.com/dns/docs/zones/zones-overview#private_zones) for `aiplatform.googleapis.com.` with an `A` record `*.aiplatform.googleapis.com` $\to$ `10.10.0.50`.
* **Breaking the Upstream Loop**:
  Configure Envoy's upstream cluster to use an external resolver (such as `8.8.8.8`) via C-Ares DNS, or forward to Google's fixed Private Access VIPs (`199.36.153.8/30` or `199.36.153.4/30`).
* **TLS Trust**:
  Sign the gateway's TLS certificate using an internal Enterprise Certificate Authority (CA) and pre-install the CA root in base container images (`/etc/ssl/certs/ca-certificates.crt`).

---

### 3.2 Local Development & Developer Agent Fleets (`agy`, Workstations)

* **Automatic Priority Tagging**: Developer tools (`agy`, CLI runners, evaluation harnesses) default to `X-Request-Priority: best-effort`.
* **Shedding Behavior**: During peak production load (> 70–90% capacity), the Governor sheds best-effort traffic first with `HTTP 429 (Retry-After: 5s)`, preserving production availability.
* **Network Routing**: Workstations connect via Corporate VPN with DNS forwarding to the Shared VPC Private DNS Zone, or export standard `HTTPS_PROXY` variables.

---

## 4. Scope Boundary & Future Extensions

* **In-Scope**: Workloads where outbound API traffic can be routed through the VPC Private DNS zone (GKE pods, Compute VMs, serverless workloads with VPC egress, and VPN-connected developer machines).
* **Future Extension (Agent Gateway / Envoy AI Gateway)**: For workloads that cannot use Private DNS or that require explicit proxy hops and multi-provider routing, this Governor `ext_proc` engine can be deployed as an extension to **Google Agent Gateway** or **Envoy AI Gateway**.

---

## 5. End-to-End Decision & Solution Matrix

| Requirement | Recommended Solution | Mechanism |
| :--- | :--- | :--- |
| **No code changes in cloud agents/ADK** | VPC Private DNS Zone | Cloud DNS private zone wildcard override to Internal Load Balancer VIP. |
| **Prevent proxy upstream DNS loop** | Upstream Custom Resolver / Private Google Access | Envoy `typed_dns_resolver_config` pointing to `8.8.8.8` or static IP `199.36.153.8`. |
| **Protect production from local batch runs** | Default Priority Tagging (`best-effort`) | Automatic header injection in developer tooling + priority-tiered load shedding. |
| **Zero streaming TTFT degradation** | Header-only processing mode in `ext_proc` | Configure Envoy `ext_proc` with `request_header_mode: SEND` and `response_header_mode: SKIP`. |
| **Future non-DNS routing** | Google Agent Gateway / Envoy AI Gateway | Deploy Governor as an AI Gateway plugin/filter. |

---

## 6. References

* [Envoy External Processing (ext_proc) Filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter)
* [Google Cloud DNS Private Zones Overview](https://cloud.google.com/dns/docs/zones/zones-overview#private_zones)
* [Google Cloud Private Service Connect for Google APIs](https://cloud.google.com/vpc/docs/private-service-connect)
* [Google Cloud Quotas Overview](https://cloud.google.com/docs/quotas/overview)
* [Vertex AI Generative AI Quotas & Limits](https://cloud.google.com/vertex-ai/generative-ai/docs/quotas)
