#!/usr/bin/env bash
# Shared helpers for the live integration test scripts. Source, don't execute.

set -euo pipefail

LIVE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${LIVE_DIR}/../.." && pwd)"
STATE_DIR="${LIVE_DIR}/.state"
STATE_FILE="${STATE_DIR}/env"

# Defaults; override via environment.
: "${GQG_REGION_PRIMARY:=us-central1}"
: "${GQG_REGION_FALLBACK:=europe-west1}"
: "${GQG_LOWERED_RPM:=10}"
: "${GQG_ENVOY_CONTAINER:=gqg-live-envoy}"
: "${DRY_RUN:=0}"

readonly QUOTA_SERVICE="aiplatform.googleapis.com"
readonly RPM_QUOTA_ID="GenerateContentRequestsPerMinutePerProjectPerRegionPerBaseModel"
readonly REQUIRED_APIS=(aiplatform.googleapis.com cloudquotas.googleapis.com monitoring.googleapis.com)

log()  { printf '[live] %s\n' "$*" >&2; }
warn() { printf '[live] WARNING: %s\n' "$*" >&2; }
die()  { printf '[live] ERROR: %s\n' "$*" >&2; exit 1; }

# run executes a mutating command, or prints it when DRY_RUN=1.
run() {
  if [[ "${DRY_RUN}" == "1" ]]; then
    { printf '[dry-run]'; printf ' %q' "$@"; printf '\n'; } >&2
    return 0
  fi
  "$@"
}

require_tools() {
  local t
  for t in gcloud docker go jq; do
    command -v "$t" >/dev/null 2>&1 || die "'$t' is required but not installed"
  done
}

require_adc() {
  [[ "${DRY_RUN}" == "1" ]] && return 0
  gcloud auth application-default print-access-token >/dev/null 2>&1 \
    || die "Application Default Credentials are missing or expired. Run: gcloud auth application-default login"
}

require_free_ports() {
  local p
  for p in 10000 9901 50051 50052; do
    if (exec 3<>"/dev/tcp/127.0.0.1/${p}") 2>/dev/null; then
      die "port ${p} is already in use; stop whatever is listening on it"
    fi
  done
}

# org_of PROJECT prints the organization ID that PROJECT belongs to.
org_of() {
  gcloud projects get-ancestors "$1" --format=json \
    | jq -r '.[] | select(.type=="organization") | .id' | head -n1
}

# Preference IDs must be lowercase letters, digits and hyphens.
preference_id() {
  local region="$1" model="$2"
  printf 'gqg-live-rpm-%s-%s' "${region}" "${model}" | tr 'A-Z._' 'a-z--' | cut -c1-63
}

# current_rpm_limit PROJECT REGION MODEL prints the effective RPM limit for the
# (region, base_model) dimension, or nothing if the quota has no such entry.
current_rpm_limit() {
  local project="$1" region="$2" model="$3"
  gcloud beta quotas info describe "${RPM_QUOTA_ID}" \
    --service="${QUOTA_SERVICE}" --project="${project}" --format=json \
    | jq -r --arg r "${region}" --arg m "${model}" '
        .dimensionsInfos[]?
        | select((.dimensions.region // "") == $r and (.dimensions.base_model // "") == $m)
        | .details.value' | head -n1
}

# set_rpm_preference PROJECT REGION MODEL VALUE creates or updates the quota
# preference for (region, base_model). Quota preferences cannot be deleted;
# teardown restores the original value instead.
set_rpm_preference() {
  local project="$1" region="$2" model="$3" value="$4"
  local pref; pref="$(preference_id "${region}" "${model}")"
  local common=(--service="${QUOTA_SERVICE}" --project="${project}" --quota-id="${RPM_QUOTA_ID}"
                --preferred-value="${value}" --dimensions="region=${region},base_model=${model}"
                --allow-high-percentage-quota-decrease --allow-quota-decrease-below-usage
                --justification="gemini-quota-governor live integration test")
  if [[ "${DRY_RUN}" != "1" ]] && gcloud beta quotas preferences describe "${pref}" --project="${project}" >/dev/null 2>&1; then
    run gcloud beta quotas preferences update "${pref}" "${common[@]}" --quiet
  else
    run gcloud beta quotas preferences create --preference-id="${pref}" "${common[@]}" --quiet
  fi
}

save_state() { mkdir -p "${STATE_DIR}"; printf '%s=%q\n' "$1" "$2" >> "${STATE_FILE}"; }

load_state() {
  [[ -f "${STATE_FILE}" ]] || return 1
  # shellcheck disable=SC1090
  source "${STATE_FILE}"
}
