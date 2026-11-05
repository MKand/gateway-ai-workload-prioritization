# Live Integration Tests

End-to-end tests against real GCP: real Cloud Quotas and Cloud Monitoring reads, the real control plane and data plane, the real Envoy config, and real Vertex AI traffic.
Each run is **setup -> test -> teardown**. Teardown always runs, even on failure or Ctrl-C, and restores everything it changed.

| Command | Projects | Covers | Time |
| :--- | :--- | :--- | :--- |
| `make live-single PROJECT_A=... MODEL=...` | 1 | Live reads, forward, shed, reroute, fail-open | ~3-5 min |
| `make live-dual PROJECT_A=... PROJECT_B=... MODEL=...` | 2 (same org) | Plus per-project isolation and org usage = A + B | ~3-5 min |

## Quick start

```bash
# One-time, as a project owner: least-privilege service account (no keys)
test/live/create-sa.sh <a> [<b>]
export GQG_SA=gqg-live-test@<a>.iam.gserviceaccount.com
gcloud auth application-default login --impersonate-service-account=$GQG_SA

# Every run
make live-preflight PROJECT_A=<a> PROJECT_B=<b> MODEL=<model>   # read-only; tells you what is missing
DRY_RUN=1 make live-single PROJECT_A=<a> MODEL=<model>          # prints every change it would make
make live-single PROJECT_A=<a> MODEL=<model>
make live-dual   PROJECT_A=<a> PROJECT_B=<b> MODEL=<model>
```

## Step-by-step setup

### 1. Tools

`gcloud` (with `gcloud components install beta`), `docker` (daemon running), `go`, `jq`.

### 2. Projects

| Requirement | Why |
| :--- | :--- |
| Dedicated, throwaway project(s) | Project A's quota is lowered to 10 RPM during the run |
| Inside an **organization** | Setup reads the org ID; dual mode needs both projects in the **same** org |
| No org policy blocking `us-central1`, `europe-west1` or Gemini models | Primary and fallback regions |

### 3. Identity and roles

**Recommended: a service account, impersonated.** It has exactly the roles below, needs no keys, and works the same for a team or in CI.

```bash
test/live/create-sa.sh <a> [<b>]        # run as a project owner; safe to re-run
```

It creates `gqg-live-test@<a>.iam.gserviceaccount.com`, grants it the roles below on each project, lets you impersonate it (`roles/iam.serviceAccountTokenCreator`), and enables the APIs that calls made as the SA are charged to (IAM, IAM Credentials, Resource Manager, Cloud Billing, Service Usage on project A). Then, on the machine that runs the tests:

```bash
export GQG_SA=gqg-live-test@<a>.iam.gserviceaccount.com              # scripts impersonate it
gcloud auth application-default login --impersonate-service-account=$GQG_SA   # Go test impersonates it
```

New IAM grants take 1-2 minutes to propagate. Remove everything with `test/live/create-sa.sh --delete <a> [<b>]`.

**Alternative: your own account.** Grant it the same roles (or `roles/owner`), and log in twice with the same account:

```bash
gcloud auth login <account>
gcloud auth application-default login
```

| Role (on every test project) | Needed for |
| :--- | :--- |
| `roles/browser` | Reading the project and its org |
| `roles/serviceusage.serviceUsageAdmin` | Enabling/disabling APIs; using the project as the quota project |
| `roles/cloudquotas.admin` | Reading limits; lowering and restoring the quota preference |
| `roles/monitoring.viewer` | Reading usage metrics |
| `roles/aiplatform.user` | Calling Gemini |

### 4. Model

The test needs a model with a **per-project RPM quota in both regions** (`GenerateContentRequestsPerMinutePerProjectPerRegionPerBaseModel`).
Most current Gemini text models use Dynamic Shared Quota and have **no** such quota. TTS variants such as `gemini-2.5-flash-tts` do (verified 2026-10-27).
`make live-preflight` lists the eligible models once the Cloud Quotas API is enabled on project A. Retired models (`gemini-1.5-*`) are still listed but return 404.

### 5. Run and read the results

- `make live-preflight ...` must end with "All checks passed".
- `DRY_RUN=1 make live-single ...` shows every `gcloud` change.
- `make live-single ...` / `make live-dual ...` print `--- PASS`/`--- FAIL` per scenario plus `evidence:` lines (Envoy access log, snapshot values, responses).

## Scenarios

| ID | Check |
| :--- | :--- |
| R1-R3 | Reconcile works; the lowered limit is visible; every project/region has a real (non-fallback) limit |
| F1 | Healthy best effort is forwarded to the primary region |
| F2 | Non-Vertex host gets 404 |
| F3 | Background load saturates project A's primary (waits for Monitoring, ~1.5-2 min) |
| F4 | Best effort gets 429 with `Retry-After`, no upstream call |
| F5 | Custom is rerouted: Envoy dials the fallback region with the rewritten path |
| F6 | Critical is never rewritten |
| M1, M2 | (dual) Project B is unaffected; org usage equals A + B |
| X1, X2 | Data plane down: Envoy fails open. Control plane down: the last snapshot is kept |

## What setup and teardown change

| Step | Changes |
| :--- | :--- |
| `setup.sh` | Enables `aiplatform`, `cloudquotas`, `monitoring` where disabled; records project A's current RPM limit; lowers it to 10 via a quota preference; writes `.state/` |
| `teardown.sh` | Restores the preference to the recorded value; disables only the APIs setup enabled; removes the Envoy container and `.state/` |

Quota preferences can't be deleted, so the preference `gqg-live-rpm-<region>-<model>` stays, set to the original value.

## Troubleshooting

| Symptom | Cause / fix |
| :--- | :--- |
| `no '...PerBaseModel' entry for ... base_model=<model>` | The model has no per-project quota (Dynamic Shared Quota) or isn't a base model ID; pick one from preflight |
| `404 Publisher model ... not found` | The model is retired or not available in the region |
| `R3 ... equals the built-in fallback` | The limit wasn't read from Cloud Quotas; check the model ID and `make live-check` output |
| `API requires a quota project` | ADC user credentials need a quota project; `run.sh` sets `GOOGLE_CLOUD_QUOTA_PROJECT` to project A |
| Scripts and Go test use different identities | With `GQG_SA`: `gcloud auth application-default login --impersonate-service-account=$GQG_SA`. Without: re-run `gcloud auth application-default login` as the CLI account |
| `no valid identity for the scripts` with `GQG_SA` | You can't impersonate the SA yet: run `create-sa.sh`, wait 1-2 min |
| A command hangs, or fails with `API ... not enabled on project` | Calls as the SA are charged to project A; re-run `create-sa.sh` to enable the needed APIs |
| `port ... in use` | Stop whatever listens on 10000, 9901, 50051 or 50052 |
| `a previous run was not torn down` | `make live-teardown` |
| Teardown couldn't restore the quota | State is kept; re-run `make live-teardown`, or use the printed `gcloud` command |

Options: `GQG_SA` (service account to impersonate), `DRY_RUN=1` (print changes only), `GQG_KEEP=1` (skip teardown for debugging), `GQG_REGION_PRIMARY` / `GQG_REGION_FALLBACK` (defaults `us-central1` / `europe-west1`), `GQG_LOWERED_RPM` (default `10`), `GQG_REQUEST_BODY` (custom request JSON).
