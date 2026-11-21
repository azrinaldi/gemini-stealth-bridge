#!/usr/bin/env bash
# start.sh — Launch gemini-stealth-bridge (gsb) on port 20133 with isolated
# account storage in ~/.gemini-stealth-bridge/accounts/*.json.

set -euo pipefail
cd "$(dirname "$0")"

DIR="${BRIDGE_HOME:-$HOME/.gemini-stealth-bridge}"
ACCOUNTS_DIR="$DIR/accounts"
mkdir -p "$ACCOUNTS_DIR"
export BRIDGE_HOME="$DIR"
export PORT="${PORT:-20133}"
export BRIDGE_WEB_UI="${BRIDGE_WEB_UI:-1}"
needs_build=0
if [ ! -x "./gsb" ]; then
  needs_build=1
elif [ -n "$(find cmd internal \( -name '*.go' -o -name '*.html' \) -newer ./gsb -print -quit 2>/dev/null)" ]; then
  needs_build=1
fi

if [ "$needs_build" = "1" ]; then
  # A deployed tree often ships a prebuilt ./gsb without a Go toolchain. Refusing to
  # start in that case would take the daemon down on the next restart, and with
  # Restart=always systemd would retry forever. Prefer the existing binary and say
  # plainly that it is older than the sources.
  if ! command -v go >/dev/null 2>&1; then
    if [ -x "./gsb" ]; then
      echo "warning: sources are newer than ./gsb but no Go toolchain is installed; starting the existing binary" >&2
    else
      echo "error: ./gsb is missing and no Go toolchain is installed; build it elsewhere and copy it here" >&2
      exit 1
    fi
  else
    echo "building gsb..." >&2
    go build -o gsb ./cmd/gsb
  fi
fi

echo "starting gemini-stealth-bridge (gsb) on :$PORT (data: $DIR, web ui: http://127.0.0.1:$PORT/ui)"
exec ./gsb "$@"
