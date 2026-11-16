#!/bin/sh
# Installs a restricted child through a worker's CONNECT proxy using the
# generated command exactly as a person would, then checks routing, isolation,
# proxy loss and recovery, service restart, and signed retirement.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
project=${CONTROL_TEST_PROJECT:-control-test-proxy-$$}
gateway_url=https://gateway.control.test:7443
proxy_url=http://172.31.250.10:8888
managed=${CONTROL_TEST_MANAGED_PROXY:-false}

compose() {
    docker compose --project-name "$project" -f "$root/compose.proxy.yaml" "$@"
}

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$status" -ne 0 ]; then
        compose logs --no-color gateway bridge || true
        control machines proxies || true
        if [ -n "${bridge_id:-}" ]; then proxy_settings --fail || true; fi
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

jq_filter() {
    # The bridge is deliberately stopped during managed restart checks.
    compose exec -T gateway jq "$@"
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
    control machines proxies | jq_filter -e '[.[] | select(.name == "bridge") | .endpoints[] | select(.state == "ready")] | length == 1'
}

proxy_without_dependents() {
    control machines proxies | jq_filter -e '[.[] | select(.name == "bridge") | .endpoints[] | select(.managed and .state == "ready" and .dependents.invitations == 0 and .dependents.children == 0 and .dependents.retiring == 0)] | length == 1'
}

proxy_settings() {
    compose exec -T gateway curl --silent --show-error --max-time 30 "$@" \
        -H 'Authorization: Bearer control-compose-superuser-only-test-key-0001' \
        -H 'Content-Type: application/json' "$gateway_url/v1/fleet/nodes/$bridge_id/proxy"
}

disable_proxy() {
    proxy_settings "$@" -X PUT --data '{"enabled":false}'
}

child_online() {
    control machines | jq_filter -e '[.[] | select(.name == "child-01" and .online)] | length == 1'
}

child_offline() {
    control machines | jq_filter -e '[.[] | select(.name == "child-01" and (.online | not))] | length == 1'
}

bridge_online() {
    control machines | jq_filter -e '[.[] | select(.name == "bridge" and .online)] | length == 1'
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
bridge_id=$(control machines | jq_filter -r '.[] | select(.name == "bridge") | .id')
if [ "$managed" = true ]; then
    settings=$(proxy_settings --fail)
    printf '%s\n' "$settings" | jq_filter -e '.supported and (.enabled | not)' >/dev/null || fail "new worker unexpectedly hosts a managed proxy"
    request=$(printf '%s\n' "$settings" | jq_filter -c '.interfaces[] | select(.address == "172.31.250.10") | {enabled:true, interface:.name, address:.address, port:8888, sources:["172.31.250.0/24"]}')
    [ -n "$request" ] || fail "worker did not report its restricted network interface"
    proxy_settings --fail -X PUT --data "$request" | jq_filter -e '.enabled' >/dev/null || fail "explicit proxy enable was not confirmed"
    wait_for "explicitly enabled managed listener without dependencies" proxy_without_dependents
else
    wait_for "bridge proxy readiness" proxy_ready
fi

case "$(compose exec -T child uname -m | tr -d '\r')" in
    x86_64) arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *) fail "unsupported test architecture" ;;
esac
if control machines add child-02 --platform "linux/$arch" --via bridge --proxy-url http://10.9.9.9:1 >/dev/null 2>&1; then
    fail "unadvertised proxy endpoint accepted"
fi
link=$(control machines add child-01 --platform "linux/$arch" --via bridge --json)
printf '%s\n' "$link" | jq_filter -e --arg proxy "$proxy_url" '.gatewayProxy.url == $proxy and (.gatewayProxy.worker | length == 64)' >/dev/null || fail "selection not echoed: $link"
command=$(printf '%s\n' "$link" | jq_filter -r .command)
case "$command" in
    *"--proxy '$proxy_url' --noproxy ''"*) ;;
    *) fail "command does not pin the proxy: $command" ;;
esac

wait_for "proxy activation after invitation" proxy_ready
child "curl --fail --silent --max-time 10 --proxy $proxy_url --noproxy '' $gateway_url/healthz" >/dev/null || fail "proxy path is unavailable"
# TLS verification stays on: a wrong trust store fails through the proxy.
if child "mkdir -p /tmp/no-ca && curl --fail --silent --max-time 10 --cacert /etc/ssl/certs/ISRG_Root_X1.pem --capath /tmp/no-ca --proxy $proxy_url $gateway_url/healthz"; then
    fail "gateway TLS was not verified"
fi

# Inherited bypass variables must not route installer or node traffic directly.
child "export NO_PROXY='*' no_proxy='*'; $command" || fail "generated installer failed"
wait_for "child online through the proxy" child_online
child "grep -q '\"gatewayProxy\"' /root/.control/node.json" || fail "profile lacks gatewayProxy"
control call child-01 node.describe >/dev/null || fail "authorized work through the proxy failed"
if [ "$managed" = true ]; then
    status=$(disable_proxy --output /dev/null --write-out '%{http_code}')
    [ "$status" = 409 ] || fail "proxy disable with a dependent child returned $status instead of 409"
    child "curl --fail --silent --max-time 10 --proxy $proxy_url --noproxy '' $gateway_url/healthz" >/dev/null || fail "refused disable disturbed the listener"
fi

# Proxy loss disconnects without a direct fallback; recovery reconnects.
if [ "$managed" = true ]; then
    # Repeat to expose nondeterministic interface assignment across restarts.
    for restart in 1 2 3 4 5; do
        compose stop bridge
        wait_for "child offline after managed proxy loss $restart" child_offline
        compose start bridge
        wait_for "bridge online after restart $restart" bridge_online
        proxy_settings --fail | jq_filter -e '.settings as $saved | any(.interfaces[]; .name == $saved.interface and .address == $saved.address)' >/dev/null ||
            fail "restarted bridge changed the saved proxy interface/address pairing"
        wait_for "child reconnected through the proxy after restart $restart" child_online
    done
else
    compose exec -T bridge pkill -x tinyproxy
    wait_for "child offline after proxy loss" child_offline
    compose exec -T -d bridge sh -c 'tinyproxy -d -c /tmp/tinyproxy.conf'
fi
wait_for "child reconnected through the proxy" child_online

# A service restart keeps the saved route, not the invitation ticket.
child "/root/bin/control --config /root/.control/node.json service stop"
wait_for "child stopped" child_offline
child "/root/bin/control --config /root/.control/node.json service start"
wait_for "child restarted through the proxy" child_online

# Signed retirement reads still work through the proxy after revocation.
if [ "$managed" = true ]; then
    pending=$(control machines add child-02 --platform "linux/$arch" --via bridge --json)
    pending_id=$(printf '%s\n' "$pending" | jq_filter -r .id)
fi
control machines unregister child-01
# The bracket keeps pgrep from matching this shell's own command line.
stopped() { ! child "pgrep -f '[/]root/bin/control'"; }
wait_for "child retired" stopped
if [ "$managed" = true ]; then
    wait_for "pending invitation retains the shared listener" proxy_ready
    child "curl --fail --silent --max-time 10 --proxy $proxy_url --noproxy '' $gateway_url/healthz" >/dev/null || fail "pending invitation lost its listener"
    control machines revoke "$pending_id"
    wait_for "removing final dependency leaves the managed listener enabled" proxy_without_dependents
    child "curl --fail --silent --max-time 10 --proxy $proxy_url --noproxy '' $gateway_url/healthz" >/dev/null || fail "removing final dependency disabled the listener"
    disable_proxy --fail | jq_filter -e '.enabled == false' >/dev/null || fail "explicit proxy disable was not confirmed"
    if child "curl --fail --silent --max-time 3 --proxy $proxy_url --noproxy '' $gateway_url/healthz"; then
        fail "managed listener still accepts after explicit disable"
    fi
fi
echo "PASS: gateway proxy installation, routing, recovery, restart, and retirement"
