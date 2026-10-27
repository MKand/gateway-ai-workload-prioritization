#!/usr/bin/env bash
# Runs a full live integration test: setup -> go test -> teardown.
# Teardown always runs (success, failure or Ctrl-C) unless GQG_KEEP=1.
#
# Usage: test/live/run.sh single PROJECT_A MODEL
#        test/live/run.sh dual   PROJECT_A PROJECT_B MODEL

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

mode="${1:-}"
case "${mode}" in
  single) [[ $# -eq 3 ]] || die "usage: $0 single PROJECT_A MODEL"
          setup_args=(--mode single --project-a "$2" --model "$3") ;;
  dual)   [[ $# -eq 4 ]] || die "usage: $0 dual PROJECT_A PROJECT_B MODEL"
          setup_args=(--mode dual --project-a "$2" --project-b "$3" --model "$4") ;;
  *)      die "usage: $0 single|dual ..." ;;
esac

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [[ "${GQG_KEEP:-0}" == "1" ]]; then
    warn "GQG_KEEP=1: skipping teardown. Run test/live/teardown.sh when done."
  else
    "${LIVE_DIR}/teardown.sh" || rc=1
  fi
  exit "${rc}"
}
trap cleanup EXIT INT TERM

"${LIVE_DIR}/setup.sh" "${setup_args[@]}"
[[ "${DRY_RUN}" == "1" ]] && { log "dry run: skipping go test"; exit 0; }
load_state

export GQG_CONFIG="${CONFIG}" GQG_PROJECT_A="${PROJECT_A}" GQG_PROJECT_B="${PROJECT_B}" \
       GQG_MODEL="${MODEL}" GQG_REGION_PRIMARY="${REGION_PRIMARY}" GQG_REGION_FALLBACK="${REGION_FALLBACK}" \
       GQG_LOWERED_RPM="${LOWERED_RPM}" GQG_ENVOY_CONTAINER
# User ADC needs a quota project for the Cloud Quotas / Monitoring APIs.
export GOOGLE_CLOUD_QUOTA_PROJECT="${PROJECT_A}"

log "running live tests (mode=${MODE}); this takes ~10-20 minutes because usage metrics lag"
cd "${REPO_ROOT}"
go test -tags live -count=1 -v -timeout 45m ./test/live/...
