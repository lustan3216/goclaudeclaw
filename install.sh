#!/usr/bin/env sh
set -e

REPO="lustan3216/goclaudeclaw"
BINARY="goclaudeclaw"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

# detect OS and arch
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# get latest version
VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed 's/.*"tag_name": *"\(.*\)".*/\1/')
if [ -z "$VERSION" ]; then
  echo "Could not determine latest version"; exit 1
fi

# build download URL
TARBALL="${BINARY}_${VERSION#v}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${TARBALL}"

echo "Installing ${BINARY} ${VERSION} for ${OS}/${ARCH}..."

# download and install
mkdir -p "$INSTALL_DIR"
curl -fsSL "$URL" | tar -xz -C "$INSTALL_DIR" "$BINARY"
chmod +x "${INSTALL_DIR}/${BINARY}"

echo "Installed to ${INSTALL_DIR}/${BINARY}"
echo ""
echo "If ${INSTALL_DIR} is not in your PATH, add this to your shell config:"
echo "  export PATH=\"\$PATH:${INSTALL_DIR}\""
echo ""
echo "Run: goclaudeclaw --help"
