#!/usr/bin/env bash
#
# Builds the ALL SHARE Windows agent, native library included.
#
# Produces dist/windows/allshare-agent.exe, which is what the installer ships.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist/windows"
VERSION="${ALLSHARE_VERSION:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}"

"$ROOT/build/build-native.sh"

mkdir -p "$DIST"
echo "Building allshare-agent.exe (version $VERSION)"
cd "$ROOT"
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
    CC="${CC:-x86_64-w64-mingw32-gcc}" \
    CXX="${CXX:-x86_64-w64-mingw32-g++}" \
    go build -trimpath \
        -ldflags "-s -w -X main.Version=$VERSION" \
        -o "$DIST/allshare-agent.exe" \
        ./agent/cmd/allshare-agent

echo "Done: $DIST/allshare-agent.exe"
ls -la "$DIST/allshare-agent.exe"
