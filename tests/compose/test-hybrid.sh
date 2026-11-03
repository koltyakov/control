#!/bin/sh
# Installs a real Hybrid pair under systemd, using the generated command exactly
# as a person would: failed preflight, failure after redemption, rerun, retirement.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
project=${CONTROL_TEST_PROJECT:-control-test-hybrid-$$}

compose() {
    docker compose --project-name "$project" --profile hybrid -f "$root/compose.yaml" "$@"
}

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$status" -ne 0 ]; then
        compose logs --no-color gateway || true
        compose exec -T hybrid-host sh -c 'journalctl --no-pager -n 80; cat /var/lib/control/hybrid/*/node.log 2>/dev/null; cat /home/fixture/.config/control/node.log 2>/dev/null' || true
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
    compose exec -T -e CONTROL_GATEWAY=http://gateway:7330 -e CONTROL_SUPERUSER_KEY=control-compose-superuser-only-test-key-0001 gateway control "$@"
}

host() {
    compose exec -T hybrid-host sh -c "$1"
}

# The session a person's login would have, without granting linger up front.
as_user() {
    compose exec -T -u fixture -w /home/fixture -e HOME=/home/fixture -e USER=fixture -e XDG_RUNTIME_DIR=/run/user/1000 -e DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus hybrid-host bash -c "$1"
}

wait_for() {
    description=$1
    shift
    attempts=0
    until "$@" >/dev/null 2>&1; do
        attempts=$((attempts + 1))
        if [ "$attempts" -ge 60 ]; then fail "timed out: $description"; fi
        sleep 1
    done
}

machine_query() {
    control machines | compose exec -T hybrid-host jq -e --arg name "$1" "[.[] | select(.name == \$name$2)] | length > 0" >/dev/null
}

online() {
    machine_query "$1" ' and .online'
}

registered() {
    machine_query "$1" ''
}

compose up --build --wait gateway hybrid-host
host 'systemctl start user@1000.service'
wait_for "user manager" host 'test -S /run/user/1000/bus'

case "$(host 'uname -m' | tr -d '\r')" in
    x86_64) arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *) fail "unsupported test architecture" ;;
esac
link=$(control machines add workstation --platform "linux/$arch" --service hybrid --linger --json)
command=$(printf '%s\n' "$link" | sed -n 's/^[[:space:]]*"command": "\(.*\)",\{0,1\}$/\1/p' | sed 's/\\u0026/\&/g; s/\\"/"/g' | head -n 1)
[ -n "$command" ] || fail "invitation has no command: $link"

echo "== A busy local API port stops the installer before anything is installed"
compose exec -d -T hybrid-host socat TCP-LISTEN:7331,bind=127.0.0.1,reuseaddr,fork /dev/null
wait_for "port fixture" host 'bash -c "</dev/tcp/127.0.0.1/7331"'
if as_user "$command" >/tmp/hybrid-output.$$ 2>&1; then fail "installer ignored a busy port"; fi
grep -q 'in use' /tmp/hybrid-output.$$ || { cat /tmp/hybrid-output.$$; fail "busy port was not reported"; }
rm -f /tmp/hybrid-output.$$
host 'test ! -e /var/lib/control/hybrid/*/node.json' || fail "failed preflight installed the System companion"
if registered workstation-system; then fail "failed preflight registered the System companion"; fi
host 'pkill -x socat'
wait_for "port release" host '! bash -c "</dev/tcp/127.0.0.1/7331"'

echo "== A failure after redemption can be fixed by rerunning the same command"
as_user 'mkdir -p ~/.config/systemd/user && echo "[Service]" > ~/.config/systemd/user/control-node.service'
if as_user "$command" >/dev/null 2>&1; then fail "installer ignored an unmanaged service file"; fi
wait_for "System companion" online workstation-system
if online workstation; then fail "User should not be online yet"; fi
as_user 'rm ~/.config/systemd/user/control-node.service'
as_user "$command"
wait_for "User context" online workstation
wait_for "System companion after rerun" online workstation-system

echo "== Both contexts run with the expected accounts and settings"
as_user 'systemctl --user is-active --quiet control-node.service' || fail "User service is not active"
host 'systemctl list-units --state=active "control-node-*" | grep -q control-node-' || fail "System service is not active"
control exec workstation -- whoami | grep -Eq '"stdout": ?"fixture' || fail "User context has the wrong account"
control exec workstation-system -- whoami | grep -Eq '"stdout": ?"root' || fail "System context has the wrong account"
host 'loginctl show-user fixture -p Linger | grep -q Linger=yes' || fail "--linger was not applied"
host 'jq -e ".metricsIntervalSeconds == 60" /var/lib/control/hybrid/*/node.json' || fail "companion kept the default sampling interval"
host 'jq -e ".listen == \"127.0.0.1:7332\"" /var/lib/control/hybrid/*/node.json' || fail "companion uses the wrong API port"
[ "$(host 'stat -c %a /var/lib/control/hybrid/*/node.json' | tr -d '\r')" = 600 ] || fail "companion profile is readable by others"

echo "== A completed pair's command is spent"
if as_user "curl -fsS $(printf '%s\n' "$command" | sed -n "s/.*curl -fsSL \('[^']*'\).*/\1/p")" >/dev/null 2>&1; then fail "completed invitation is still downloadable"; fi

echo "== Retiring the companion removes its binaries and keeps its data"
host 'test -x /usr/local/lib/control/*/control' || fail "companion executable missing before retirement"
control machines unregister workstation-system
wait_for "companion executable removal" host 'test ! -e /usr/local/lib/control'
wait_for "companion unit removal" host '! ls /etc/systemd/system/control-node-* 2>/dev/null | grep -q .'
host 'test -d /var/lib/control/hybrid/*/work' || fail "retirement removed the companion work directory"
host 'test -f /var/lib/control/hybrid/*/state/identity.key' || fail "retirement removed the companion identity"
host 'test -z "$(ls -d /var/lib/control/hybrid/*/state/updates /var/lib/control/hybrid/*/state/uninstall-* 2>/dev/null)"' || fail "retirement kept companion binaries"
online workstation || fail "User context stopped with its companion"

echo "PASS: Hybrid installation under systemd"
