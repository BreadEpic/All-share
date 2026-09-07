#!/usr/bin/env bash
#
# Builds the ALL SHARE native capture library for Windows.
#
# The library wraps Direct3D 11, DXGI Desktop Duplication, Media Foundation and
# WASAPI. It can be built on Windows with MSVC or cross-compiled from Linux with
# mingw-w64, which is what CI does — cross-compiling keeps the whole product
# buildable and testable on one machine.
#
# Audio needs libopus, which is fetched and built once into a cache. Opus is the
# only audio codec every browser decodes over WebRTC, so there is no
# dependency-free alternative that is not also a bad-sounding one. If libopus
# cannot be built, the library is still produced without audio support and the
# agent reports sound as unavailable rather than sending silence.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NATIVE="$ROOT/agent/native"
OUT="$NATIVE/build"
CACHE="${ALLSHARE_DEP_CACHE:-$ROOT/.deps}"

CXX="${CXX:-x86_64-w64-mingw32-g++}"
CC_="${CC:-x86_64-w64-mingw32-gcc}"
AR="${AR:-x86_64-w64-mingw32-ar}"
HOST="${MINGW_HOST:-x86_64-w64-mingw32}"

OPUS_VERSION="${OPUS_VERSION:-1.5.2}"
OPUS_URL="https://downloads.xiph.org/releases/opus/opus-${OPUS_VERSION}.tar.gz"
OPUS_PREFIX="$CACHE/opus-$OPUS_VERSION-$HOST"

if ! command -v "$CXX" >/dev/null 2>&1; then
    echo "error: $CXX not found." >&2
    echo "On Debian or Ubuntu: sudo apt-get install mingw-w64" >&2
    exit 1
fi

build_opus() {
    if [ -f "$OPUS_PREFIX/lib/libopus.a" ]; then
        echo "  using cached libopus $OPUS_VERSION"
        return 0
    fi
    echo "  building libopus $OPUS_VERSION (once; cached in $CACHE)"
    mkdir -p "$CACHE"
    local work="$CACHE/src"
    rm -rf "$work"
    mkdir -p "$work"

    if ! curl -sSL --fail -o "$work/opus.tar.gz" "$OPUS_URL"; then
        echo "  warning: could not download libopus; building without audio" >&2
        return 1
    fi
    tar xzf "$work/opus.tar.gz" -C "$work"
    (
        cd "$work/opus-$OPUS_VERSION"
        ./configure --host="$HOST" --prefix="$OPUS_PREFIX" \
            --disable-shared --enable-static \
            --disable-doc --disable-extra-programs --disable-hardening \
            >/dev/null 2>&1
        make -j"$(nproc 2>/dev/null || echo 4)" >/dev/null 2>&1
        make install >/dev/null 2>&1
    ) || { echo "  warning: libopus failed to build; building without audio" >&2; return 1; }
    rm -rf "$work"
    return 0
}

AUDIO_FLAGS=()
AUDIO_SOURCES=()
echo "Building the ALL SHARE capture library with $CXX"
if build_opus; then
    AUDIO_FLAGS=(-DALLSHARE_WITH_OPUS "-I$OPUS_PREFIX/include/opus")
    AUDIO_SOURCES=(audio.cpp)
    echo "  audio: enabled (Opus)"
else
    AUDIO_SOURCES=(audio.cpp)
    echo "  audio: disabled (libopus unavailable)"
fi

mkdir -p "$OUT"
SOURCES=(d3d.cpp duplication.cpp encoder.cpp session.cpp "${AUDIO_SOURCES[@]}")
OBJECTS=()

for source in "${SOURCES[@]}"; do
    object="$OUT/${source%.cpp}.o"
    echo "  compiling $source"
    "$CXX" -std=c++17 -O2 -Wall -Wextra -Wno-unused-parameter \
        -ffunction-sections -fdata-sections \
        "${AUDIO_FLAGS[@]+"${AUDIO_FLAGS[@]}"}" \
        -c "$NATIVE/$source" -o "$object"
    OBJECTS+=("$object")
done

echo "  archiving liballshare_capture.a"
"$AR" rcs "$OUT/liballshare_capture.a" "${OBJECTS[@]}"

# The Go build links against whatever is here, so libopus is copied in beside
# the capture library rather than left in a cache directory the linker cannot
# be expected to know about.
if [ -f "$OPUS_PREFIX/lib/libopus.a" ]; then
    cp "$OPUS_PREFIX/lib/libopus.a" "$OUT/"
    echo "  bundling libopus.a"
else
    # An empty archive keeps the link line valid when audio is unavailable.
    : > "$OUT/empty.c"
    "$CC_" -c "$OUT/empty.c" -o "$OUT/empty.o"
    "$AR" rcs "$OUT/libopus.a" "$OUT/empty.o"
    rm -f "$OUT/empty.c" "$OUT/empty.o"
    echo "  audio stubbed out"
fi

echo "Done: $OUT/liballshare_capture.a"
