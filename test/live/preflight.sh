#!/usr/bin/env bash
# "cond && pass ... || fail ..." is intentional: pass() only prints and cannot fail.
# shellcheck disable=SC2015
# Read-only check that everything the live tests need is in place.
# Changes nothing. Prints PASS/FAIL per item and how to fix each failure.
#
# Usage: test/live/preflight.sh PROJECT_A [PROJECT_B] [MODEL]

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
set +e  # keep going after failures; we report them all

PROJECT_A="${1:-}" PROJECT_B="${2:-}" MODEL="${3:-}"
[[ -n "${PROJECT_A}" ]] || die "usage: $0 PROJECT_A [PROJECT_B] [MODEL]"
# Allow "preflight.sh A MODEL" (single mode) when the 2nd arg looks like a model.
if [[ -n "${PROJECT_B}" && -z "${MODEL}" && "${PROJECT_B}" == gemini* ]]; then MODEL="${PROJECT_B}"; PROJECT_B=""; fi

# Permissions the live test needs on every project (see test/live/README.md).
readonly PERMISSIONS=(  # what LIVE_TEST_ROLES grant and the tests use
  resourcemanager.projects.get
  serviceusage.services.list serviceusage.services.enable serviceusage.services.disable serviceusage.services.use
  cloudquotas.quotas.get cloudquotas.quotas.update
  monitoring.timeSeries.list
  aiplatform.endpoints.predict
)

fails=0
pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n       fix: %s\n' "$1" "$2"; fails=$((fails + 1)); }
info() { printf '  \033[33mINFO\033[0m %s\n' "$1"; }

echo "Tools"
for t in gcloud docker go jq; do
  if command -v "$t" >/dev/null 2>&1; then pass "$t installed"; else fail "$t missing" "install $t"; fi
done
gcloud beta --help >/dev/null 2>&1 && pass "gcloud beta component" || fail "gcloud beta component missing" "gcloud components install beta"
docker info >/dev/null 2>&1 && pass "docker daemon reachable" || fail "docker daemon not reachable" "start Docker / add yourself to the docker group"

echo "Identity"
CLI_ACCOUNT="$(test_identity)"
if [[ -n "${GQG_SA:-}" ]]; then
  info "impersonating service account ${GQG_SA} (GQG_SA)"
  ADC_FIX="gcloud auth application-default login --impersonate-service-account=${GQG_SA}"
  CLI_FIX="ensure you can impersonate ${GQG_SA} (roles/iam.serviceAccountTokenCreator via create-sa.sh); new grants take 1-2 min to propagate"
else
  ADC_FIX="gcloud auth application-default login   (sign in as ${CLI_ACCOUNT:-the CLI account})"
  CLI_FIX="gcloud auth login <account>   (or: gcloud config set account <account>)"
fi
if [[ -n "${CLI_ACCOUNT}" ]] && gcloud auth print-access-token >/dev/null 2>&1; then
  pass "scripts act as: ${CLI_ACCOUNT}"
else
  fail "no valid identity for the scripts (${CLI_ACCOUNT:-none})" "${CLI_FIX}"
fi
ADC_TOKEN="$(gcloud auth application-default print-access-token 2>/dev/null)"
if [[ -n "${ADC_TOKEN}" ]]; then
  ADC_ACCOUNT="$(curl -s "https://oauth2.googleapis.com/tokeninfo?access_token=${ADC_TOKEN}" | jq -r '.email // empty')"
  if [[ -z "${ADC_ACCOUNT}" ]]; then
    # Service account tokens carry no email; read the impersonation target from the ADC file.
    adc_file="${GOOGLE_APPLICATION_CREDENTIALS:-${CLOUDSDK_CONFIG:-$HOME/.config/gcloud}/application_default_credentials.json}"
    ADC_ACCOUNT="$(jq -r '.service_account_impersonation_url // empty' "${adc_file}" 2>/dev/null | sed -n 's|.*/serviceAccounts/\([^:]*\):generateAccessToken|\1|p')"
  fi
  pass "Go test (ADC) acts as: ${ADC_ACCOUNT:-unknown}"
  if [[ -n "${ADC_ACCOUNT}" && -n "${CLI_ACCOUNT}" && "${ADC_ACCOUNT}" != "${CLI_ACCOUNT}" ]]; then
    fail "scripts (${CLI_ACCOUNT}) and Go test (${ADC_ACCOUNT}) use different identities" "${ADC_FIX}"
  fi
else
  fail "Application Default Credentials missing or expired" "${ADC_FIX}"
fi

PROJECTS=("${PROJECT_A}")
[[ -n "${PROJECT_B}" ]] && PROJECTS+=("${PROJECT_B}")
CLI_TOKEN="$(gcloud auth print-access-token 2>/dev/null)"  # honours GQG_SA impersonation
declare -A ORGS

for p in "${PROJECTS[@]}"; do
  echo "Project ${p}"
  if ! gcloud projects describe "${p}" >/dev/null 2>&1; then
    fail "project not found or not visible to ${CLI_ACCOUNT:-the current account}" "check the project ID, or log in with an account that has access: gcloud auth login <account>"
    continue
  fi
  pass "project visible"

  org="$(org_of "${p}" 2>/dev/null)"
  ORGS["${p}"]="${org}"
  [[ -n "${org}" ]] && pass "organization ${org}" || fail "project is not in an organization" "the tests need an org (dual mode needs both projects in the same org)"

  billing="$(gcloud billing projects describe "${p}" --format='value(billingEnabled)' 2>/dev/null)"
  [[ "${billing}" == "True" ]] && pass "billing enabled" || fail "billing not enabled" "gcloud billing projects link ${p} --billing-account=<BILLING_ACCOUNT_ID>"

  perms_json="$(printf '%s\n' "${PERMISSIONS[@]}" | jq -R . | jq -sc '{permissions: .}')"
  granted="$(curl -s -X POST -H "Authorization: Bearer ${CLI_TOKEN}" -H "Content-Type: application/json" \
    "https://cloudresourcemanager.googleapis.com/v1/projects/${p}:testIamPermissions" -d "${perms_json}" | jq -r '.permissions[]?')"
  missing=()
  for perm in "${PERMISSIONS[@]}"; do grep -qx "${perm}" <<<"${granted}" || missing+=("${perm}"); done
  if (( ${#missing[@]} == 0 )); then
    pass "all ${#PERMISSIONS[@]} required permissions granted"
  else
    fail "missing permissions: ${missing[*]}" "grant the roles in test/live/README.md (Roles section), e.g. roles/owner on ${p}"
  fi

  enabled="$(gcloud services list --enabled --project="${p}" --format='value(config.name)' 2>/dev/null)"
  for api in "${REQUIRED_APIS[@]}"; do
    grep -qx "${api}" <<<"${enabled}" && pass "${api} enabled" || info "${api} disabled (setup.sh enables it; teardown.sh disables it again)"
  done
done

if [[ -n "${PROJECT_B}" && -n "${ORGS[${PROJECT_A}]:-}" && -n "${ORGS[${PROJECT_B}]:-}" ]]; then
  echo "Dual mode"
  [[ "${ORGS[${PROJECT_A}]}" == "${ORGS[${PROJECT_B}]}" ]] && pass "both projects in org ${ORGS[${PROJECT_A}]}" \
    || fail "projects are in different orgs" "use two projects from the same organization"
fi

echo "Model"
if grep -qx cloudquotas.googleapis.com <<<"$(gcloud services list --enabled --project="${PROJECT_A}" --format='value(config.name)' 2>/dev/null)"; then
  models="$(gcloud beta quotas info describe "${RPM_QUOTA_ID}" --service="${QUOTA_SERVICE}" --project="${PROJECT_A}" --format=json 2>/dev/null \
    | jq -r --arg r "${GQG_REGION_PRIMARY}" --arg f "${GQG_REGION_FALLBACK}" '
        [.dimensionsInfos[]? | select(.dimensions.base_model and (.details.value // 0) > 0)
         | {r: .dimensions.region, m: .dimensions.base_model, v: .details.value}] as $all
        | [$all[] | select(.r == $r) | .m] - ([$all[] | select(.r == $r) | .m] - [$all[] | select(.r == $f) | .m])
        | unique | .[]')"
  if [[ -n "${MODEL}" ]]; then
    grep -qx "${MODEL}" <<<"${models}" && pass "${MODEL} has a per-project RPM quota in ${GQG_REGION_PRIMARY} and ${GQG_REGION_FALLBACK}" \
      || fail "${MODEL} has no per-project RPM quota in both ${GQG_REGION_PRIMARY} and ${GQG_REGION_FALLBACK}" "pick one of: $(tr '\n' ' ' <<<"${models}")"
  else
    info "models with a per-project RPM quota in both regions: $(tr '\n' ' ' <<<"${models}")"
  fi
  info "check the model is callable; retired models (e.g. gemini-1.5-*) still have quota entries but return 404"
else
  info "cloudquotas API disabled on ${PROJECT_A}; model check skipped (setup.sh fails early if the model has no per-project quota)"
fi

echo "Local"
for port in 10000 9901 50051 50052; do
  if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then fail "port ${port} in use" "stop whatever listens on ${port}"; else pass "port ${port} free"; fi
done
[[ -f "${STATE_FILE}" ]] && fail "a previous run was not torn down" "test/live/teardown.sh" || pass "no leftover state"

echo
if (( fails == 0 )); then
  echo "All checks passed. Next: DRY_RUN=1 make live-single PROJECT_A=${PROJECT_A} MODEL=${MODEL:-<model>}"
else
  echo "${fails} check(s) failed. Fix them and re-run preflight."
  exit 1
fi
