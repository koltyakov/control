#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
base=${CONTROL_TEST_PROJECT:-control-test-$$}
project=
mode=

compose() {
    if [ "$mode" = relay ]; then
        docker compose --project-name "$project" -f "$root/compose.yaml" -f "$root/compose.relay.yaml" "$@"
    else
        docker compose --project-name "$project" -f "$root/compose.yaml" "$@"
    fi
}

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ -n "$project" ]; then
        if [ "$status" -ne 0 ]; then compose logs --no-color || true; fi
        compose down --volumes --remove-orphans || true
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM

case "${1:-all}" in
    all) modes="webrtc relay" ;;
    webrtc|relay) modes=$1 ;;
    *) echo "Usage: sh tests/compose/test.sh [all|webrtc|relay]" >&2; exit 1 ;;
esac

for mode in $modes; do
    project="$base-$mode"
    compose up --build --abort-on-container-exit --exit-code-from tests tests
    compose down --volumes --remove-orphans
    project=
    # The core Go suite is transport-independent and only needs to run once.
    CONTROL_TEST_SUITE=e2e
    export CONTROL_TEST_SUITE
done
