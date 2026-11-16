#!/bin/sh
# Install Vigil on macOS: download the release binary for this Mac, put it in ~/.local/bin, and register
# the launchd service. Re-running it upgrades in place (the daemon waits for running rounds first).
#
#   curl -fsSL https://raw.githubusercontent.com/genkin-he/openvigil/main/scripts/install.sh | sh
#
# Environment: VIGIL_REPO (owner/name on GitHub), VIGIL_VERSION (tag, default latest), BIN_DIR.
set -eu

REPO="${VIGIL_REPO:-genkin-he/openvigil}"
VERSION="${VIGIL_VERSION:-latest}"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"

fail() { echo "vigil install: $*" >&2; exit 1; }

[ "$(uname -s)" = "Darwin" ] || fail "Vigil runs on macOS only (it is a launchd service)."
case "$(uname -m)" in
  arm64) ARCH=arm64 ;;
  x86_64) ARCH=amd64 ;;
  *) fail "unsupported CPU: $(uname -m)" ;;
esac
command -v claude >/dev/null 2>&1 || echo "note: the claude CLI (Claude Code) is not on PATH yet — Vigil's agents need it: https://claude.com/claude-code" >&2

# herdr (https://herdr.dev) is required: new tasks are talked through, and runs are taken over, in it.
HERDR_INSTALL="curl -fsSL https://herdr.dev/install.sh | sh"
export PATH="$HOME/.local/bin:$PATH" # where herdr's installer puts it
have_herdr() { command -v herdr >/dev/null 2>&1; }
ensure_herdr() {
  have_herdr && return 0
  echo "Vigil needs herdr (https://herdr.dev), a terminal workspace for coding agents. It is not installed."
  # curl | sh feeds this script on stdin, so ask on the terminal.
  if ! { : </dev/tty; } 2>/dev/null; then
    fail "herdr is not installed and there is no terminal to ask on. Install it with: $HERDR_INSTALL — then run this script again."
  fi
  printf "Install herdr now? [Y/n] " >/dev/tty
  ans=""
  read -r ans </dev/tty || ans=n
  case "$ans" in
    "" | y | Y | yes | Yes | YES) ;;
    *) fail "Vigil cannot be used without herdr. To install it later: $HERDR_INSTALL — then run this script again." ;;
  esac
  if command -v brew >/dev/null 2>&1; then
    brew install herdr || fail "brew install herdr failed"
  else
    curl -fsSL https://herdr.dev/install.sh | sh || fail "herdr install failed"
  fi
  have_herdr || fail "herdr was installed but is not on PATH; open a new terminal and run this script again."
  echo "herdr $(herdr --version 2>/dev/null | cut -d' ' -f2) installed."
}
ensure_herdr

if [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
fi
ASSET="vigil_darwin_$ARCH.tar.gz"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "Downloading $ASSET from $REPO ($VERSION)…"
curl -fsSL -o "$TMP/$ASSET" "$BASE/$ASSET" || fail "download failed: $BASE/$ASSET"
curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt" || fail "download failed: $BASE/checksums.txt"
(cd "$TMP" && grep " $ASSET\$" checksums.txt | shasum -a 256 -c -) >/dev/null || fail "checksum mismatch for $ASSET"

tar -xzf "$TMP/$ASSET" -C "$TMP"
mkdir -p "$BIN_DIR"
install -m 0755 "$TMP/vigil" "$BIN_DIR/vigil"
xattr -d com.apple.quarantine "$BIN_DIR/vigil" 2>/dev/null || true

"$BIN_DIR/vigil" install
echo
echo "Vigil $("$BIN_DIR/vigil" version | cut -d' ' -f2) is running. Console: http://127.0.0.1:7788"
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "Add $BIN_DIR to your PATH to use the vigil command." ;;
esac
