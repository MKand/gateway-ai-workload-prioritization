# Live Integration Tests

End-to-end tests against real GCP: real Cloud Quotas and Cloud Monitoring reads, the real control plane and data plane, the real Envoy config, and real Vertex AI traffic.
Each run is **setup -> test -> teardown**. Teardown always runs, even on failure or Ctrl-C, and restores everything it changed.

The governor enforces a configured allowance itself, so the test changes **no quotas**. Setup writes a deliberately low allowance (20 RPM by default) and the test fills it with a few dozen tiny requests, far below any real limit, so it costs cents and causes no real 429s.

| Command | Projects | Covers | Time |
| :--- | :--- | :--- | :--- |
| `make live-single PROJECT_A=... MODEL=...` | 1 | Live reads, critical reserve, reroute, metrics, bypass detection, failure modes | ~5-12 min |
| `make live-dual PROJECT_A=... PROJECT_B=... MODEL=...` | 2 (same org) | Plus: the allowance is shared org-wide across projects | ~5-12 min |

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
| A test project (no production traffic needed) | Real Gemini calls are made; quotas are not changed |
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
| `roles/cloudquotas.viewer` | Reading limits |
| `roles/monitoring.viewer` | Reading usage metrics |
| `roles/aiplatform.user` | Calling Gemini |

### 4. Model

Any callable Gemini model works, including the current text models on Dynamic Shared Quota (for example `gemini-2.5-flash`). Retired models (`gemini-1.5-*`) return 404.

### 5. Run and read the results

- `make live-preflight ...` must end with "All checks passed".
- `DRY_RUN=1 make live-single ...` shows every `gcloud` change.
- `make live-single ...` / `make live-dual ...` print `--- PASS`/`--- FAIL` per scenario plus `evidence:` lines (Envoy access log, snapshot values, responses).

## Scenarios

| ID | Check |
| :--- | :--- |
| R1, R2 | Reconcile against real GCP works; Cloud Monitoring usage series are readable |
| F1 | Best effort is forwarded to the primary region, and the upstream 2xx is counted |
| F2 | Non-Vertex host gets 404 |
| F3 | Best effort stops at the critical reserve (about 70% of the allowance): 429 with `Retry-After`, no upstream call |
| F4 | Critical is still forwarded while best effort is shed |
| F5 | Custom is rerouted: Envoy dials the fallback region with the rewritten path |
| M1 | (dual) The allowance is org-wide: project B's best effort is shed too |
| F6 | Metrics recorded the shed, forward and reroute decisions |
| B1 | Direct calls to Vertex AI (not through the governor) show up as bypass once Cloud Monitoring catches up (waits up to 10 min) |
| X1, X2 | Data plane down: Envoy fails open. Control plane down: the data plane keeps enforcing locally |

## What setup and teardown change

| Step | Changes |
| :--- | :--- |
| `setup.sh` | Enables `aiplatform`, `cloudquotas`, `monitoring` where disabled; writes `.state/` with a governor config (low allowance for the primary and fallback regions) |
| `teardown.sh` | Disables only the APIs setup enabled; removes the Envoy container and `.state/`. Also restores a quota preference left by runs from before milestone 1 |

## Troubleshooting

| Symptom | Cause / fix |
| :--- | :--- |
| `B1` times out with no usage for the model | Cloud Monitoring has no quota usage metric for this model (possible on Dynamic Shared Quota). Bypass subtraction then cannot see bypass traffic; report the model so the metric source can be adapted |
| `404 Publisher model ... not found` | The model is retired or not available in the region |
| `API requires a quota project` | ADC user credentials need a quota project; `run.sh` sets `GOOGLE_CLOUD_QUOTA_PROJECT` to project A |
| Scripts and Go test use different identities | With `GQG_SA`: `gcloud auth application-default login --impersonate-service-account=$GQG_SA`. Without: re-run `gcloud auth application-default login` as the CLI account |
| `no valid identity for the scripts` with `GQG_SA` | You can't impersonate the SA yet: run `create-sa.sh`, wait 1-2 min |
| A command hangs, or fails with `API ... not enabled on project` | Calls as the SA are charged to project A; re-run `create-sa.sh` to enable the needed APIs |
| `port ... in use` | Stop whatever listens on 10000, 9901, 50051 or 50052 |
| `a previous run was not torn down` | `make live-teardown` |
| Teardown couldn't restore a quota preference (old runs only) | State is kept; re-run `make live-teardown`, or use the printed `gcloud` command |

Options: `GQG_SA` (service account to impersonate), `DRY_RUN=1` (print changes only), `GQG_KEEP=1` (skip teardown for debugging), `GQG_REGION_PRIMARY` / `GQG_REGION_FALLBACK` (defaults `us-central1` / `europe-west1`), `GQG_ALLOWANCE_RPM` (default `20`), `GQG_DIRECT_CALLS` (direct Vertex calls for B1, default `10`), `GQG_REQUEST_BODY` (custom request JSON).
