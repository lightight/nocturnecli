#!/bin/sh
# Nocturne installer (macOS / Linux).
#   curl -fsSL https://nocturnecli.lol/install.sh | sh
#
# Env overrides:
#   NOCTURNE_REPO         GitHub owner/repo for the build fallback (default lightight/nocturnecli)
#   NOCTURNE_INSTALL_DIR  where to put the binary (default ~/.local/bin)
set -eu

BASE="__BASE__" # replaced by the server with its own URL when served
REPO="${NOCTURNE_REPO:-lightight/nocturnecli}"
INSTALL_DIR="${NOCTURNE_INSTALL_DIR:-$HOME/.local/bin}"
BIN="nocturne"

c_amber='\033[33m'; c_green='\033[32m'; c_blue='\033[34m'; c_dim='\033[2m'; c_off='\033[0m'
say() { printf "%b\n" "$1"; }
say "${c_amber}◗ Nocturne installer${c_off}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) say "unsupported architecture: $ARCH"; exit 1 ;;
esac
case "$OS" in
  darwin|linux) ;;
  *) say "unsupported OS: $OS (use install.ps1 on Windows)"; exit 1 ;;
esac

mkdir -p "$INSTALL_DIR"
DEST="$INSTALL_DIR/$BIN"
ASSET="nocturne_${OS}_${ARCH}"
ok=0

# 1) prebuilt binary served by the host
case "$BASE" in
  http*://*)
    say "${c_blue}→ downloading${c_off} $BASE/bin/$ASSET"
    if curl -fsSL "$BASE/bin/$ASSET" -o "$DEST" 2>/dev/null && [ -s "$DEST" ]; then
      chmod +x "$DEST"; ok=1
    fi ;;
esac

# 2) GitHub release
if [ "$ok" -eq 0 ]; then
  URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"
  say "${c_blue}→ downloading${c_off} $URL"
  if curl -fsSL "$URL" -o "$DEST" 2>/dev/null && [ -s "$DEST" ]; then
    chmod +x "$DEST"; ok=1
  fi
fi

# 3) build from source with Go
if [ "$ok" -eq 0 ] && command -v go >/dev/null 2>&1; then
  say "${c_dim}no prebuilt binary — building with go install${c_off}"
  GOBIN="$INSTALL_DIR" go install "github.com/${REPO}@latest" && ok=1
fi

if [ "$ok" -ne 1 ]; then
  say "could not install. Install Go (https://go.dev/dl) and re-run, or build manually."
  exit 1
fi
say "${c_green}✓ installed${c_off} $DEST"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) say "${c_amber}⚠ add $INSTALL_DIR to your PATH:${c_off}\n    export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac
say ""
say "Set your key:  ${c_dim}export NOCTURNE_API=noct_your_key${c_off}"
say "Then run:      ${c_amber}nocturne${c_off}"
