# Live Integration Tests

Full end-to-end test against real GCP: real Cloud Quotas + Cloud Monitoring reads, real control plane / data plane, real Envoy config, real Vertex AI traffic.
Each script runs **setup -> test -> teardown**. Teardown always runs, even on failure or Ctrl-C.

| Script | Projects | What it adds |
| :--- | :--- | :--- |
| `run-single.sh PROJECT_A MODEL` | 1 | Live reads, forward, shed, reroute, failure modes |
| `run-dual.sh PROJECT_A PROJECT_B MODEL` | 2 (same org) | Plus per-project tracking (M1) and org usage = A + B (M2) |

```bash
gcloud auth application-default login
make live-single PROJECT_A=my-test-a MODEL=gemini-2.5-flash
make live-dual   PROJECT_A=my-test-a PROJECT_B=my-test-b MODEL=gemini-2.5-flash
make live-teardown   # manual cleanup, only needed after GQG_KEEP=1 or a failed teardown
```

## Requirements

- `gcloud` (with `beta` component), `docker`, `go`, `jq`.
- ADC user with, on every test project: enable/disable APIs, read and update quota preferences (`roles/cloudquotas.admin`), `roles/monitoring.viewer`, `roles/aiplatform.user`.
- Dedicated test projects: project A's quota is lowered during the run.
- `MODEL` must be a **base model ID** that has a per-project RPM quota (`GenerateContentRequestsPerMinutePerProjectPerRegionPerBaseModel`). Setup fails with instructions if it does not.
- Free local ports: 10000, 9901, 50051, 50052.

## What each step does

| Step | Changes |
| :--- | :--- |
| `setup.sh` | Enables `aiplatform`, `cloudquotas`, `monitoring` APIs where disabled; records project A's current RPM limit for `us-central1`/`MODEL`; lowers it to 10 via a quota preference; writes `.state/governor.yaml` and `.state/env` |
| `go test -tags live` | Starts the control plane (real GCP clients), data plane and Envoy (Docker); runs the scenarios below; about 100 tiny Vertex calls (`maxOutputTokens: 1`) |
| `teardown.sh` | Restores the quota preference to the recorded value; disables only the APIs setup enabled; removes the Envoy container; deletes `.state/` |

## Scenarios

| ID | Check |
| :--- | :--- |
| R1-R3 | Reconcile works; lowered limit is visible; every project/region has real limits |
| F1 | Healthy best effort is sent to the primary region |
| F2 | Non-Vertex host gets 404 |
| F3 | Background load saturates project A's primary (waits for Monitoring) |
| F4 | Best effort gets 429 with `Retry-After`, no upstream call |
| F5 | Custom is rerouted: Envoy dials the fallback region host with the rewritten path |
| F6 | Critical is never rewritten |
| M1, M2 | (dual) Project B is unaffected; org usage equals A + B |
| X1, X2 | Data plane down: Envoy fails open. Control plane down: data plane keeps the last snapshot |

## Notes

- A run takes about 15-30 minutes: quota changes take a few minutes to apply and usage metrics lag traffic.
- Quota preferences cannot be deleted, so teardown sets the preference back to the original value instead. Its ID is `gqg-live-rpm-<region>-<model>`.
- `DRY_RUN=1` prints every mutating `gcloud` command without running it.
- `GQG_KEEP=1` skips teardown for debugging; run `make live-teardown` afterwards.
- Overrides: `GQG_REGION_PRIMARY` (default `us-central1`), `GQG_REGION_FALLBACK` (`europe-west1`), `GQG_LOWERED_RPM` (`10`).
