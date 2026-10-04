#!/bin/sh
set -eu
umask 077

if [ "${1:-}" = '--help' ]; then
  echo 'Install Control from GitHub Releases. Optional: CONTROL_RELEASE_REPO, CONTROL_VERSION, CONTROL_INSTALL_DIR.'
  exit 0
fi
repo="${CONTROL_RELEASE_REPO:-koltyakov/control}"
case "$repo" in *[!a-zA-Z0-9_./-]*|'') echo 'Invalid release repository' >&2; exit 1;; esac
case "$(uname -s)" in Linux) os=linux;; Darwin) os=darwin;; *) echo 'Unsupported OS' >&2; exit 1;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64;; arm64|aarch64) arch=arm64;; *) echo 'Unsupported architecture' >&2; exit 1;; esac
base="https://github.com/$repo/releases/latest/download"
if [ -n "${CONTROL_VERSION:-}" ]; then
  case "$CONTROL_VERSION" in *[!a-zA-Z0-9_.+-]*) echo 'Invalid version' >&2; exit 1;; esac
  base="https://github.com/$repo/releases/download/$CONTROL_VERSION"
fi
asset="control_${os}_${arch}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
download() {
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"; else wget -qO "$2" "$1"; fi
}
download "$base/checksums.txt" "$tmp/checksums.txt"
download "$base/$asset" "$tmp/control"
expected="$(awk -v file="$asset" '$2 == file {print $1}' "$tmp/checksums.txt")"
if command -v sha256sum >/dev/null 2>&1; then actual="$(sha256sum "$tmp/control" | awk '{print $1}')"; else actual="$(shasum -a 256 "$tmp/control" | awk '{print $1}')"; fi
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then echo 'Binary checksum mismatch' >&2; exit 1; fi
chmod 700 "$tmp/control"
"$tmp/control" install-self
echo 'Next: control setup --gateway https://YOUR_GATEWAY --name main --client opencode'
echo 'If control is not on PATH, use ~/.local/bin/control or your CONTROL_INSTALL_DIR.'
