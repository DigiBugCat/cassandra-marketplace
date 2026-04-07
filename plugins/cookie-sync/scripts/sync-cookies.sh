#!/usr/bin/env bash
# Sync YouTube cookies from Firefox to the auth service.
# Called by the cookie-sync plugin on SessionStart.
# Auto-installs cass CLI if missing. Non-fatal on all errors.

set -euo pipefail

CASS_REPO="https://github.com/Cassandras-Edge/cass.git"
# Local source if available (editable install for dev)
CASS_LOCAL="${CLAUDE_PLUGIN_ROOT}/../../toolbox/cass"

# Auto-install cass if not found
if ! command -v cass &>/dev/null; then
  echo "cass CLI not found — installing..." >&2
  if command -v uv &>/dev/null; then
    if [ -d "$CASS_LOCAL" ]; then
      uv tool install -e "$CASS_LOCAL" 2>&1 || {
        echo "Failed to install cass from local source" >&2
        exit 0
      }
    else
      uv tool install "cass @ git+${CASS_REPO}" 2>&1 || {
        echo "Failed to install cass from git" >&2
        exit 0
      }
    fi
  else
    echo "uv not found — can't auto-install cass" >&2
    exit 0
  fi
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
