#!/bin/sh
# Control Vault two-OS-account smoke test in one disposable container.
# See tests/compose/vault-smoke.md. It never touches host accounts, keys, or
# Control configuration; the only host writes are the temporary build
# directory and the report directory printed at the end.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
image=${CONTROL_VAULT_SMOKE_IMAGE:-control-vault-smoke:local}
mode=docker
binary=
keep_image=false
scenarios="clean migrate"

usage() {
    echo "Usage: sh tests/compose/vault-smoke.sh [--local-build | --binary PATH] [--scenario clean|migrate|all] [--keep-image]" >&2
    exit 2
}

while [ $# -gt 0 ]; do
    case $1 in
        --local-build) mode=local ;;
        --binary) [ $# -ge 2 ] || usage; mode=prebuilt; binary=$2; shift ;;
        --keep-image) keep_image=true ;;
        --scenario)
            [ $# -ge 2 ] || usage
            case $2 in all) scenarios="clean migrate" ;; clean|migrate) scenarios=$2 ;; *) usage ;; esac
            shift ;;
        *) usage ;;
    esac
    shift
done

work=$(mktemp -d "${TMPDIR:-/tmp}/control-vault-smoke.XXXXXX")
out=${CONTROL_VAULT_SMOKE_OUT:-$work/report}
mkdir -p "$out"
container=
cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ -n "$container" ]; then docker rm --force "$container" >/dev/null 2>&1 || true; fi
    if [ "$keep_image" != true ]; then docker image rm "$image" >/dev/null 2>&1 || true; fi
    rm -rf "$work/prebuilt"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM

case $(docker info --format '{{.Architecture}}') in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "unsupported Docker architecture" >&2; exit 1 ;;
esac

target=runtime-built
set --
if [ "$mode" != docker ]; then
    target=runtime-prebuilt
    mkdir -p "$work/prebuilt"
    if [ "$mode" = local ]; then
        (cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o "$work/prebuilt/control" ./cmd/control)
    else
        cp "$binary" "$work/prebuilt/control"
    fi
    set -- --build-context "prebuilt=$work/prebuilt"
fi

docker build --quiet --file "$root/tests/compose/vault-smoke.Dockerfile" --target "$target" --tag "$image" "$@" "$root"

# No network, no privileges, no host mounts. Root inside keeps only what the
# administrator role needs: create and chown fixture directories, switch to
# the synthetic accounts, audit their files, and stop their processes. Each
# scenario gets a fresh container.
status=0
for scenario in $scenarios; do
    container=control-vault-smoke-$$-$scenario
    result=0
    docker run --name "$container" --rm --init \
        --network none \
        --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
        --cap-add SETUID --cap-add SETGID --cap-add KILL \
        --security-opt no-new-privileges \
        --pids-limit 512 --memory 1g \
        "$image" "$scenario" >"$out/smoke-$scenario.log" 2>&1 || result=$?
    container=
    grep -v '^REPORT ' "$out/smoke-$scenario.log" || true
    grep '^REPORT ' "$out/smoke-$scenario.log" | sed 's/^REPORT //' >"$out/report-$scenario.json" || true
    echo "Vault smoke $scenario: exit $result, report $out/report-$scenario.json"
    if [ "$result" -ne 0 ]; then status=$result; fi
done
echo "Vault smoke reports in $out, exit $status"
exit "$status"
