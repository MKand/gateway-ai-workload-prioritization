#!/usr/bin/env bash
# Dual-project live integration test (per-project tracking + org totals):
# setup -> test -> teardown.
# Usage: test/live/run-dual.sh PROJECT_A PROJECT_B MODEL
exec "$(dirname "${BASH_SOURCE[0]}")/run.sh" dual "$@"
