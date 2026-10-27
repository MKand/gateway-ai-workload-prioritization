#!/usr/bin/env bash
# Undoes everything setup.sh changed. Safe to run more than once and safe to
# run after a failed or interrupted setup.
#
#   1. Restores project A's RPM quota preference to its original value.
#   2. Disables only the APIs that setup.sh enabled.
#   3. Removes the Envoy container used by the test.
#   4. Deletes test/live/.state (generated config + state).
#
# If restoring the quota fails, the state is kept so you can re-run teardown.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

failed=0

# Envoy container (started by the Go test). Harmless if absent.
if docker ps -a --format '{{.Names}}' | grep -qx "${GQG_ENVOY_CONTAINER}"; then
  log "removing container ${GQG_ENVOY_CONTAINER}"
  run docker rm -f "${GQG_ENVOY_CONTAINER}" >/dev/null
fi

if ! load_state; then
  log "no state found (${STATE_FILE}); nothing else to tear down"
  exit 0
fi

# 1. Quota preference.
if [[ "${PREFERENCE_SET:-0}" == "1" ]]; then
  log "restoring ${PROJECT_A} ${REGION_PRIMARY}/${MODEL} RPM preference to ${ORIGINAL_RPM}"
  if set_rpm_preference "${PROJECT_A}" "${REGION_PRIMARY}" "${MODEL}" "${ORIGINAL_RPM}"; then
    log "quota preference restored (it may take a few minutes to apply)"
  else
    warn "could not restore the quota preference; re-run teardown or restore manually:
      gcloud beta quotas preferences update $(preference_id "${REGION_PRIMARY}" "${MODEL}") \\
        --service=${QUOTA_SERVICE} --project=${PROJECT_A} --quota-id=${RPM_QUOTA_ID} \\
        --dimensions=region=${REGION_PRIMARY},base_model=${MODEL} --preferred-value=${ORIGINAL_RPM}"
    failed=1
  fi
fi

# 2. APIs enabled by setup (format of each value: "<project> <api>").
for var in $(compgen -v ENABLED_API_ || true); do
  read -r project api <<<"${!var}"
  log "disabling ${api} on ${project} (it was disabled before the test)"
  run gcloud services disable "${api}" --project="${project}" --quiet || { warn "failed to disable ${api} on ${project}"; failed=1; }
done

# 3/4. Local state.
if (( failed )); then
  die "teardown incomplete; state kept in ${STATE_DIR}"
fi
rm -rf "${STATE_DIR}"
log "teardown complete"
