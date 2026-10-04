#!/bin/sh
set -eu
binary="${CONTROL_INSTALL_DIR:-$HOME/.local/bin}/control"
if [ -f "$binary" ]; then
  "$binary" service uninstall
  rm -f "$binary"
fi
echo 'Control executable and user startup removed. Configuration, identities, and work files are retained.'
