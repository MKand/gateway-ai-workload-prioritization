# Gemini Priority Quota Governor

A priority-aware rate limiting and dynamic traffic router for Google Cloud Platform (GCP) Gemini and Vertex AI models. It evaluates incoming request priority headers (such as `X-Request-Priority`) and dynamically routes traffic, sheds load via HTTP 429s, or triggers model fallback cascades based on real-time GCP quota availability.

---

## Documentation Roadmap

| Document | Description |
| :--- | :--- |
| [docs/project_overview.md](docs/project_overview.md) | **Vision & Gap Analysis**: Deep dive into quota contention, comparison with traditional gateways, and Vertex AI Dynamic Shared Quotas (DSQ 3.0). |
| [docs/architecture.md](docs/architecture.md) | **System Architecture**: Detailed Control Plane vs. Data Plane design, lock-free state synchronization, and pluggable ingress topologies. |
| [docs/proxy_challenges_and_solutions.md](docs/proxy_challenges_and_solutions.md) | **Proxy Networking**: Challenges and solutions for VPC Private DNS interception, TLS termination, loop prevention, and local developer workloads (`agy`). |
| [docs/mvp_proposal_vpc_dns_interception.md](docs/mvp_proposal_vpc_dns_interception.md) | **MVP Implementation Proposal**: Phased 12-day milestone plan, success criteria, and quantitative benchmarks. |

---

## Priority Taxonomy

The Quota Governor evaluates the incoming `X-Request-Priority` header:

- **`critical`**: Protected interactive user chat and real-time agents (100% quota protection; automatically traverses model fallback cascades).
- **`best-effort`**: Sheddable offline indexing, synthetic evals, and batch jobs (throttled immediately with HTTP 429 when quota crosses 70%).
- **`custom`**: Policy-driven workloads executing custom fallback DAGs (`quality_first`, `cost_optimized`, `strict_exact`).

All data-plane decisions execute in **< 1ms** with zero payload buffering, preserving Time-To-First-Token (TTFT).

---

## Documentation Structure

```
repo/
├── docs/
│   ├── project_overview.md                 # Vision, problem statement, and DSQ 3.0 gap analysis
│   ├── architecture.md                     # System architecture: Control Plane vs. Data Plane
│   ├── proxy_challenges_and_solutions.md   # VPC Private DNS, TLS trust, and reachability
│   └── mvp_proposal_vpc_dns_interception.md # MVP implementation roadmap and milestones
└── README.md
```

---

## Requirements

### Local Development
- **Go**: 1.25 or higher
- **Envoy Proxy**: v1.28+ (or Docker / Docker Compose)
- **GCP Credentials**: Application Default Credentials (`gcloud auth application-default login`) with permissions for Cloud Quotas (`cloudquotas.quotas.get`) and Cloud Monitoring.

### Cloud Deployment (GCP)
- **GCP Project**: Vertex AI API (`aiplatform.googleapis.com`) and Cloud Quotas API (`cloudquotas.googleapis.com`) enabled.
- **Compute & Ingress**:
  - *Option A (Self-Hosted)*: GKE or Cloud Run running Envoy + Governor behind an Internal Application Load Balancer with Cloud DNS Private Zone.
  - *Option B (Managed)*: Google Cloud Application Load Balancer or GCP Agent Gateway with Cloud Service Extensions (`networkservices.googleapis.com`).
- **IAM Roles**:
  - `roles/cloudquotas.viewer` or `roles/monitoring.viewer` on the target Project or Organization.
  - `roles/aiplatform.user` for model invocation and fallback routing.

---

## External References

- [Envoy External Processing Filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter)
- [Google Cloud Service Extensions Overview](https://cloud.google.com/service-extensions/docs/overview)
- [Google Cloud DNS Private Zones](https://cloud.google.com/dns/docs/zones/zones-overview#private_zones)
- [Google Cloud Quotas Overview](https://cloud.google.com/docs/quotas/overview)
- [Vertex AI Generative AI Quotas & Limits](https://cloud.google.com/vertex-ai/generative-ai/docs/quotas)
