#!/bin/sh
# Installs a restricted child through a worker's CONNECT proxy using the
# generated command exactly as a person would, then checks routing, isolation,
# proxy loss and recovery, service restart, and signed retirement.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
project=${CONTROL_TEST_PROJECT:-control-test-proxy-$$}
gateway_url=https://gateway.control.test:7443
proxy_url=http://172.31.250.10:8888

compose() {
    docker compose --project-name "$project" -f "$root/compose.proxy.yaml" "$@"
}

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$status" -ne 0 ]; then
        compose logs --no-color gateway bridge || true
        compose exec -T child sh -c 'cat /root/.control/node.log 2>/dev/null | tail -n 80' || true
    fi
    compose down --volumes --remove-orphans || true
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

control() {
    compose exec -T -e CONTROL_GATEWAY=$gateway_url -e CONTROL_SUPERUSER_KEY=control-compose-superuser-only-test-key-0001 gateway control "$@"
}

child() {
    compose exec -T -e CONTROL_HOME=/root/.control -e CONTROL_INSTALL_DIR=/root/bin -e CONTROL_SERVICE_MODE=process child sh -c "$1"
}

jq_bridge() {
    compose exec -T bridge jq "$@"
}

wait_for() {
    description=$1
    shift
    attempts=0
    until "$@" >/dev/null 2>&1; do
        attempts=$((attempts + 1))
        if [ "$attempts" -ge 90 ]; then fail "timed out: $description"; fi
        sleep 1
    done
}

proxy_ready() {
    control machines proxies | jq_bridge -e '[.[] | select(.name == "bridge") | .endpoints[] | select(.state == "ready")] | length == 1'
}

child_online() {
    control machines | jq_bridge -e '[.[] | select(.name == "child-01" and .online)] | length == 1'
}

child_offline() {
    control machines | jq_bridge -e '[.[] | select(.name == "child-01" and (.online | not))] | length == 1'
}

compose up --build --wait gateway bridge
compose up --build -d child
trusted() { child "test -f /tmp/control-proxy-ready"; }
wait_for "child trust store" trusted

# The fixture enforces isolation: no name resolution and no route to the gateway.
if child "getent hosts gateway.control.test"; then fail "child resolves the gateway"; fi
gateway_ip=$(compose exec -T gateway sh -c "hostname -i" | tr -d '\r' | awk '{print $1}')
if child "curl --fail --silent --max-time 3 --resolve gateway.control.test:7443:$gateway_ip $gateway_url/healthz"; then
    fail "child reached the gateway directly"
fi
child "curl --fail --silent --max-time 10 --proxy $proxy_url --noproxy '' $gateway_url/healthz" >/dev/null || fail "proxy path is unavailable"
# TLS verification stays on: a wrong trust store fails through the proxy.
if child "mkdir -p /tmp/no-ca && curl --fail --silent --max-time 10 --cacert /etc/ssl/certs/ISRG_Root_X1.pem --capath /tmp/no-ca --proxy $proxy_url $gateway_url/healthz"; then
    fail "gateway TLS was not verified"
fi

wait_for "bridge proxy readiness" proxy_ready

case "$(compose exec -T child uname -m | tr -d '\r')" in
    x86_64) arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *) fail "unsupported test architecture" ;;
esac
if control machines add child-02 --platform "linux/$arch" --via bridge --proxy-url http://10.9.9.9:1 >/dev/null 2>&1; then
    fail "unadvertised proxy endpoint accepted"
fi
link=$(control machines add child-01 --platform "linux/$arch" --via bridge --json)
printf '%s\n' "$link" | jq_bridge -e --arg proxy "$proxy_url" '.gatewayProxy.url == $proxy and (.gatewayProxy.worker | length == 64)' >/dev/null || fail "selection not echoed: $link"
command=$(printf '%s\n' "$link" | jq_bridge -r .command)
case "$command" in
    *"--proxy '$proxy_url' --noproxy ''"*) ;;
    *) fail "command does not pin the proxy: $command" ;;
esac

# Inherited bypass variables must not route installer or node traffic directly.
child "export NO_PROXY='*' no_proxy='*'; $command" || fail "generated installer failed"
wait_for "child online through the proxy" child_online
child "grep -q '\"gatewayProxy\"' /root/.control/node.json" || fail "profile lacks gatewayProxy"
control call child-01 node.describe >/dev/null || fail "authorized work through the proxy failed"

# Proxy loss disconnects without a direct fallback; recovery reconnects.
compose exec -T bridge pkill -x tinyproxy
wait_for "child offline after proxy loss" child_offline
compose exec -T -d bridge sh -c 'tinyproxy -d -c /tmp/tinyproxy.conf'
wait_for "child reconnected through the proxy" child_online

# A service restart keeps the saved route, not the invitation ticket.
child "/root/bin/control --config /root/.control/node.json service stop"
wait_for "child stopped" child_offline
child "/root/bin/control --config /root/.control/node.json service start"
wait_for "child restarted through the proxy" child_online

# Signed retirement reads still work through the proxy after revocation.
control machines unregister child-01
# The bracket keeps pgrep from matching this shell's own command line.
stopped() { ! child "pgrep -f '[/]root/bin/control'"; }
wait_for "child retired" stopped
echo "PASS: gateway proxy installation, routing, recovery, restart, and retirement"
