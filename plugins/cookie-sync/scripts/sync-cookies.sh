#!/usr/bin/env bash
# Sync YouTube cookies from Firefox to the auth service.
# Called by the cookie-sync plugin on SessionStart.
# Auto-installs cass binary if missing. Non-fatal on all errors.

set -euo pipefail

CASS_BIN="${HOME}/.local/bin/cass"
CASS_REPO="Cassandras-Edge/cass"
# Local source if available (editable install for dev)
CASS_LOCAL="${CLAUDE_PLUGIN_ROOT}/../../toolbox/cass"

install_cass() {
  # Try uv editable install from local source first (dev)
  if command -v uv &>/dev/null && [ -d "$CASS_LOCAL" ]; then
    uv tool install -e "$CASS_LOCAL" 2>&1 && return 0
  fi

  # Download prebuilt binary from GitHub releases
  local os arch target
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "$arch" in
    x86_64) arch="x86_64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) echo "Unsupported arch: $arch" >&2; return 1 ;;
  esac
  target="${os}-${arch}"

  local url
  url=$(curl -sL "https://api.github.com/repos/${CASS_REPO}/releases/latest" \
    | python3 -c "import sys,json; assets=json.load(sys.stdin).get('assets',[]); matches=[a['browser_download_url'] for a in assets if '${target}' in a['name']]; print(matches[0] if matches else '')" 2>/dev/null)

  if [ -z "$url" ]; then
    echo "No cass binary found for ${target}" >&2
    return 1
  fi

  mkdir -p "$(dirname "$CASS_BIN")"
  curl -sL "$url" -o "$CASS_BIN"
  chmod +x "$CASS_BIN"
  echo "Installed cass to $CASS_BIN" >&2
}

# Auto-install cass if not found
if ! command -v cass &>/dev/null; then
  echo "cass CLI not found — installing..." >&2
  install_cass || {
    echo "Failed to install cass" >&2
    exit 0
  }
fi

# Only sync if Firefox is not running (avoids lock contention on the cookie DB)
if pgrep -x firefox &>/dev/null; then
  echo "Firefox is running — skipping cookie sync (run 'cass cookies sync' manually)" >&2
  exit 0
fi

cass cookies sync --browser firefox 2>&1 || {
  echo "Cookie sync failed (non-fatal)" >&2
  exit 0
}
