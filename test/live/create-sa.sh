#!/usr/bin/env bash
# One-time creation (or deletion) of a least-privilege service account for the
# live tests. Run it as a project owner. Test runs then impersonate the SA, so
# no keys are created.
#
# Usage:
#   test/live/create-sa.sh PROJECT_A [PROJECT_B]            # create SA in PROJECT_A, grant roles on both
#   test/live/create-sa.sh --delete PROJECT_A [PROJECT_B]   # remove role bindings and delete the SA
#
# Env: GQG_SA_NAME (default gqg-live-test), GQG_SA_USER (default: current gcloud account)

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# This script runs as the project owner, never as the test SA.
unset CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT

DELETE=0
if [[ "${1:-}" == "--delete" ]]; then DELETE=1; shift; fi
PROJECT_A="${1:-}" PROJECT_B="${2:-}"
[[ -n "${PROJECT_A}" ]] || die "usage: $0 [--delete] PROJECT_A [PROJECT_B]"

SA_NAME="${GQG_SA_NAME:-gqg-live-test}"
SA_EMAIL="${SA_NAME}@${PROJECT_A}.iam.gserviceaccount.com"
SA_USER="${GQG_SA_USER:-$(gcloud config get-value account 2>/dev/null)}"
[[ -n "${SA_USER}" ]] || die "no gcloud account; run: gcloud auth login"

PROJECTS=("${PROJECT_A}")
[[ -n "${PROJECT_B}" ]] && PROJECTS+=("${PROJECT_B}")

bind() { # verb project role
  run gcloud projects "$1-iam-policy-binding" "$2" --member="serviceAccount:${SA_EMAIL}" --role="$3" --condition=None --quiet >/dev/null
}

if (( DELETE )); then
  for p in "${PROJECTS[@]}"; do
    for role in "${LIVE_TEST_ROLES[@]}"; do
      bind remove "${p}" "${role}" 2>/dev/null || true
    done
  done
  run gcloud iam service-accounts delete "${SA_EMAIL}" --project="${PROJECT_A}" --quiet
  log "deleted ${SA_EMAIL} and its role bindings"
  exit 0
fi

# IAM API to create the SA; IAM Credentials API to impersonate it. Calls made
# as the SA are charged to its own project, so the APIs the scripts call must
# be enabled there too.
run gcloud services enable iam.googleapis.com iamcredentials.googleapis.com \
  cloudresourcemanager.googleapis.com cloudbilling.googleapis.com serviceusage.googleapis.com \
  --project="${PROJECT_A}"

if gcloud iam service-accounts describe "${SA_EMAIL}" --project="${PROJECT_A}" >/dev/null 2>&1; then
  log "${SA_EMAIL} already exists"
else
  run gcloud iam service-accounts create "${SA_NAME}" --project="${PROJECT_A}" \
    --display-name="Gemini Quota Governor live tests"
fi

for p in "${PROJECTS[@]}"; do
  for role in "${LIVE_TEST_ROLES[@]}"; do
    log "granting ${role} on ${p}"
    bind add "${p}" "${role}"
  done
done

log "allowing ${SA_USER} to impersonate ${SA_EMAIL}"
run gcloud iam service-accounts add-iam-policy-binding "${SA_EMAIL}" --project="${PROJECT_A}" \
  --member="user:${SA_USER}" --role=roles/iam.serviceAccountTokenCreator --condition=None --quiet >/dev/null

cat >&2 <<EOF

[live] Done. To run the tests as ${SA_EMAIL}:
  export GQG_SA=${SA_EMAIL}
  gcloud auth application-default login --impersonate-service-account=\$GQG_SA
  make live-preflight PROJECT_A=${PROJECT_A}${PROJECT_B:+ PROJECT_B=${PROJECT_B}} MODEL=<model>
(IAM changes can take a minute or two to propagate.)
EOF
