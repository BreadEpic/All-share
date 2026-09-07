#!/usr/bin/env bash
#
# Builds the ALL SHARE rendezvous server for common deployment targets.
#
# The server is pure Go with no cgo, so it cross-compiles anywhere and produces
# a single static binary with no runtime dependencies.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist/server"
VERSION="${ALLSHARE_VERSION:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}"

mkdir -p "$DIST"
cd "$ROOT"

build() {
    local os="$1" arch="$2" suffix="${3:-}"
    echo "Building allshare-server for $os/$arch"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X main.Version=$VERSION" \
        -o "$DIST/allshare-server-$os-$arch$suffix" \
        ./server/cmd/allshare-server
}

build linux amd64
build linux arm64
build darwin arm64
build windows amd64 .exe

echo "Done:"
ls -la "$DIST"
