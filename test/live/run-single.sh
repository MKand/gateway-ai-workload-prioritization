#!/usr/bin/env bash
# Single-project live integration test: setup -> test -> teardown.
# Usage: test/live/run-single.sh PROJECT_A MODEL
exec "$(dirname "${BASH_SOURCE[0]}")/run.sh" single "$@"
