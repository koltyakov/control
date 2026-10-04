#!/bin/sh
set -eu
cd /workspace

case "${CONTROL_TEST_SUITE:-all}" in
    all)
        go vet ./...
        go test -race ./... -count=1 -timeout=180s
        go build -o /out/control ./cmd/control
        ;;
    e2e) ;;
    *) echo "CONTROL_TEST_SUITE must be all or e2e" >&2; exit 1 ;;
esac

exec go test -race -tags=compose ./tests/compose -v -count=1 -timeout=180s
