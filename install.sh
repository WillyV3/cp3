#!/bin/sh
# cp3 installer — curl -fsSL https://raw.githubusercontent.com/WillyV3/cp3/main/install.sh | sh
# Fetches the latest GitHub release binary for this OS/arch into ~/.local/bin.
set -eu

REPO="WillyV3/cp3"
BIN_DIR="${CP3_BIN_DIR:-$HOME/.local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac
case "$os" in linux|darwin) ;; *) echo "unsupported OS: $os" >&2; exit 1 ;; esac

tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | grep -o '"tag_name": *"[^"]*"' | cut -d'"' -f4)
[ -n "$tag" ] || { echo "could not resolve latest release" >&2; exit 1; }
version=${tag#v}

url="https://github.com/$REPO/releases/download/$tag/cp3_${version}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$url" | tar -xz -C "$tmp"
mkdir -p "$BIN_DIR"
install -m 0755 "$tmp/cp3" "$BIN_DIR/cp3"

echo "cp3 $version installed to $BIN_DIR/cp3"
case ":$PATH:" in *":$BIN_DIR:"*) ;; *) echo "note: add $BIN_DIR to your PATH" ;; esac
echo "next: cp3 setup   (wires Claude Code; a local network auto-starts on first use)"
