#!/usr/bin/env bash
# Ensure the cass binary is installed in the plugin's persistent bin/ directory.
# Downloads from GitHub releases on first run or when version changes.
# The bin/ directory is auto-added to PATH by Claude Code's plugin system.

set -euo pipefail

CASS_REPO="Cassandras-Edge/cass"
WANT_VERSION="0.2.0"
BIN_DIR="${CLAUDE_PLUGIN_DATA}/bin"
VERSION_FILE="${CLAUDE_PLUGIN_DATA}/.cass-version"
CASS_BIN="${BIN_DIR}/cass"

# Already installed and correct version?
if [ -x "$CASS_BIN" ] && [ -f "$VERSION_FILE" ] && [ "$(cat "$VERSION_FILE")" = "$WANT_VERSION" ]; then
  exit 0
fi

# Detect platform
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)       ARCH="x86_64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)            echo "Unsupported arch: $ARCH" >&2; exit 0 ;;
esac

TARGET="${OS}-${ARCH}"
ASSET_NAME="cass-${TARGET}"

# Windows gets .exe suffix
if [ "$OS" = "mingw"* ] || [ "$OS" = "msys"* ] || [ "${OS,,}" = "windows_nt" ]; then
  ASSET_NAME="cass-windows-amd64.exe"
fi

# Download from GitHub release
URL="https://github.com/${CASS_REPO}/releases/download/v${WANT_VERSION}/${ASSET_NAME}"
echo "Installing cass v${WANT_VERSION} for ${TARGET}..." >&2

mkdir -p "$BIN_DIR"
if curl -sL --fail "$URL" -o "$CASS_BIN"; then
  chmod +x "$CASS_BIN"
  echo "$WANT_VERSION" > "$VERSION_FILE"
  echo "cass v${WANT_VERSION} installed to ${BIN_DIR}" >&2
else
  echo "Failed to download cass from ${URL}" >&2
  rm -f "$CASS_BIN"
  exit 0
fi
