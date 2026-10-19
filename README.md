# Gemini Priority Quota Governor

A priority-aware rate limiting and dynamic traffic router for Google Cloud Platform (GCP) Gemini and Vertex AI models. It evaluates incoming request priority headers (such as `X-Request-Priority`) and dynamically routes traffic, sheds load via HTTP 429s, or triggers model fallback cascades based on real-time GCP quota availability.

---

## Documentation

| Document | Description |
| :--- | :--- |
| [docs/project_overview.md](docs/project_overview.md) | **Vision & Gap Analysis**: Deep dive into quota contention in enterprise fleets, tenant noisy-neighbor problems, and comparison with traditional/LLM gateways. |
| [docs/architecture.md](docs/architecture.md) | **System Architecture**: Detailed Control Plane vs. Data Plane design, priority taxonomy, lock-free state synchronization, and pluggable ingress topologies. |

---

## Repository Structure

```
repo/
├── cmd/
│   ├── controlplane/        # Polls GCP quotas + usage, serves QuotaSnapshots over gRPC (--once to dump and exit)
│   └── dataplane/           # Envoy ext_proc server: reads X-Request-Priority, sets x-governor-* headers / 429s
├── pkg/
│   ├── governor/            # Config loading, priority + cascade decision logic
│   ├── controlplane/        # GCP clients, reconciler, snapshot store
│   ├── server/              # QuotaDiscoveryService gRPC server
│   ├── dataplane/           # Snapshot cache (stream client), evaluator, ext_proc glue
│   └── router/              # Vertex AI URL parsing and path/host rewriting
├── proto/ , gen/            # QuotaDiscoveryService protobuf + generated Go
├── config/governor.yaml     # Example configuration
├── deploy/envoy/envoy.yaml  # Local Envoy with the ext_proc filter
├── test/e2e/                # Envoy + Docker end-to-end test with fake regional endpoints (build tag e2e)
└── docs/                    # Overview and architecture
```

---

## Running Locally

```bash
make test        # unit + in-process pipeline tests (config -> quotas -> headers)
make e2e         # real Envoy in Docker, fake GCP metrics
make live-check CONFIG=config/governor.yaml   # one real GCP poll, prints the snapshot

# Full stack (three terminals)
make run-controlplane   # plaintext gRPC locally (--insecure)
make run-dataplane
make run-envoy          # listens on :10000

# Send a request through the governor to real Vertex AI
curl -H "Host: us-central1-aiplatform.googleapis.com" \
     -H "Authorization: Bearer $(gcloud auth print-access-token)" \
     -H "X-Request-Priority: custom" -H "X-Governor-Policy: quality_first-eu" \
     -H "Content-Type: application/json" -d '{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}' \
     http://localhost:10000/v1/projects/PROJECT/locations/us-central1/publishers/google/models/gemini-3.5-flash:generateContent
```

Envoy uses a dynamic forward proxy: it connects (TLS) to whatever `*-aiplatform.googleapis.com` host is in `:authority`
after the governor runs, so a rewrite really sends the request to the fallback region. Other hosts get 404.
The client's `Authorization` header is passed through unchanged.

Control plane <-> data plane gRPC uses TLS unless `--insecure` is passed:
`controlplane --tls-cert cert.pem --tls-key key.pem` and `dataplane --control-plane-ca ca.pem`.

Request headers read: `X-Request-Priority` (`critical`, `best_effort`, `custom`), `X-Governor-Policy`.
On fallback, `:path` and `:authority` are rewritten to the target model/region.
Headers set: `x-governor-decision` (`forward`, `reroute`, `shed`, `bypass`), `x-governor-reason`,
`x-governor-target-model`, `x-governor-target-region`, `x-governor-original-path`; shed requests get HTTP 429 with `Retry-After`.

---

## Requirements

### Local Development
- **Go**: 1.25 or higher
- **Docker**: for `make e2e` and `make run-envoy` (Envoy v1.34 image)
- **GCP Credentials**: Application Default Credentials (`gcloud auth application-default login`) with permissions for Cloud Quotas (`cloudquotas.quotas.get`) and Cloud Monitoring.

### Cloud Deployment (GCP)
- **GCP Project**: Vertex AI API (`aiplatform.googleapis.com`) and Cloud Quotas API (`cloudquotas.googleapis.com`) enabled.
- **Compute & Ingress**:
  - *Option A (Self-Hosted)*: GKE or Cloud Run running Envoy + Governor behind an Internal Application Load Balancer with Cloud DNS Private Zone.
  - *Option B (Managed)*: Google Cloud Application Load Balancer or GCP Agent Gateway with Cloud Service Extensions (`networkservices.googleapis.com`). The proxy will likely be hosted as a [Envoy Proxy](https://www.envoyproxy.io/) running on Cloud Run or GKE.
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

