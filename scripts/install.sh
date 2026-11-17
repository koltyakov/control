#!/bin/sh
set -eu
umask 077

if [ "${1:-}" = '--help' ]; then
  echo 'Install Control from GitHub Releases. Requires curl and sha256sum or shasum. Optional: CONTROL_RELEASE_REPO, CONTROL_VERSION, CONTROL_INSTALL_DIR.'
  exit 0
fi
command -v curl >/dev/null 2>&1 || { echo 'curl is required' >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256; }
else
  echo 'sha256sum or shasum is required' >&2
  exit 1
fi
repo="${CONTROL_RELEASE_REPO:-koltyakov/control}"
case "$repo" in *[!a-zA-Z0-9_./-]*|'') echo 'Invalid release repository' >&2; exit 1;; esac
case "$(uname -s)" in Linux) os=linux;; Darwin) os=darwin;; *) echo 'Unsupported OS' >&2; exit 1;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64;; arm64|aarch64) arch=arm64;; *) echo 'Unsupported architecture' >&2; exit 1;; esac
releases="https://github.com/$repo/releases"
tag="${CONTROL_VERSION:-}"
if [ -z "$tag" ]; then
  location="$(curl -fsS -o /dev/null -w '%{redirect_url}' "$releases/latest")"
  case "$location" in
    "$releases/tag/"*) tag="${location#"$releases/tag/"}";;
    *) echo 'Could not resolve latest release tag; set CONTROL_VERSION to a release tag' >&2; exit 1;;
  esac
fi
# Keep URL escapes from GitHub's redirect, such as %2B for a plus sign.
case "$tag" in ''|[!a-zA-Z0-9]*|*[!a-zA-Z0-9_.+%-]*) echo 'Invalid version' >&2; exit 1;; esac
base="$releases/download/$tag"
asset="control_${os}_${arch}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
expected=
while read -r digest name || [ -n "$digest" ]; do
  [ "$name" = "$asset" ] || continue
  [ -z "$expected" ] || { echo 'Duplicate binary checksum' >&2; exit 1; }
  expected="$digest"
done < "$tmp/checksums.txt"
[ -n "$expected" ] || { echo 'Binary checksum missing' >&2; exit 1; }
curl -fsSL "$base/$asset" -o "$tmp/control"
actual="$(sha256 < "$tmp/control")"
[ "$expected" = "${actual%% *}" ] || { echo 'Binary checksum mismatch' >&2; exit 1; }
chmod 700 "$tmp/control"
"$tmp/control" install-self
echo 'Next: control setup --gateway https://YOUR_GATEWAY --name main --client opencode'
echo 'If control is not on PATH, use ~/.local/bin/control or your CONTROL_INSTALL_DIR.'
