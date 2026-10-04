#!/bin/sh
set -eu

case "${CONTROL_NODE_NAME:?node name required}" in
    source|worker|consumer) ;;
    *) echo "Unknown test node: $CONTROL_NODE_NAME" >&2; exit 1 ;;
esac
case "${CONTROL_RELAY_ONLY:-false}" in
    true|false) ;;
    *) echo "CONTROL_RELAY_ONLY must be true or false" >&2; exit 1 ;;
esac

cat > /state/config.json <<EOF
{
  "name": "$CONTROL_NODE_NAME",
  "gateway": "http://gateway:7330",
  "listen": "0.0.0.0:7331",
  "dataDir": "/state",
  "workDir": "/work",
  "labels": {"role": "$CONTROL_NODE_NAME"},
  "relayOnly": ${CONTROL_RELAY_ONLY:-false},
  "maxTasks": 2
}
EOF

exec control node --config /state/config.json
