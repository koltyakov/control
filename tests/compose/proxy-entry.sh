#!/bin/sh
# Roles for compose.proxy.yaml. Every role trusts only the generated test CA
# through the system store; certificate verification is never disabled.
set -eu

trust() {
    cp /certs/ca.pem /usr/local/share/ca-certificates/control-proxy-test.crt
    update-ca-certificates >/dev/null 2>&1
}

case "${1:?role required}" in
certs)
    if [ -f /certs/gateway.pem ]; then exit 0; fi
    cd /certs
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 \
        -keyout ca-key.pem -out ca.pem -subj '/CN=Control proxy test CA' \
        -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign' 2>/dev/null
    openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
        -keyout gateway-key.pem -out gateway.csr -subj '/CN=gateway.control.test' 2>/dev/null
    printf 'subjectAltName=DNS:gateway.control.test\nextendedKeyUsage=serverAuth\n' > gateway.ext
    openssl x509 -req -in gateway.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial -days 2 \
        -extfile gateway.ext -out gateway.pem 2>/dev/null
    # Test-only material in a throwaway volume.
    chmod 0644 ca.pem gateway.pem gateway-key.pem
    ;;
gateway)
    trust
    exec control gateway --listen 0.0.0.0:7443 --data /state --tls-cert /certs/gateway.pem --tls-key /certs/gateway-key.pem
    ;;
bridge)
    trust
    endpoint="{\"url\":\"http://${CONTROL_PROXY_ADDRESS:?proxy address required}:8888\",\"interface\":\"restricted\"}"
    if [ "${CONTROL_PROXY_MANAGED:-false}" = true ]; then
        # The test enables hosting through the owner-management API.
        endpoints="[]"
    else
    endpoints="[$endpoint]"
    cat > /tmp/tinyproxy.conf <<CONF
User nobody
Group nogroup
Port 8888
Listen ${CONTROL_PROXY_ADDRESS:?proxy address required}
Timeout 600
MaxClients 64
Allow ${CONTROL_PROXY_ALLOW:?allowed subnet required}
ConnectPort 7443
DisableViaHeader Yes
PidFile "/tmp/tinyproxy.pid"
CONF
    tinyproxy -d -c /tmp/tinyproxy.conf &
    fi
    cat > /state/config.json <<CONF
{
  "name": "bridge",
  "gateway": "https://gateway.control.test:7443",
  "listen": "127.0.0.1:7331",
  "dataDir": "/state",
  "workDir": "/work",
  "proxyEndpoints": $endpoints
}
CONF
    exec control node --config /state/config.json
    ;;
child)
    trust
    touch /tmp/control-proxy-ready
    exec sleep infinity
    ;;
*)
    echo "unknown role: $1" >&2
    exit 1
    ;;
esac
