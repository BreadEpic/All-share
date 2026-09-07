#!/usr/bin/env bash
#
# Packages the ALL SHARE client for download.
#
# There is no build step: the client is deliberately plain files that a browser
# opens straight off disk. This only zips them, with a README at the top so a
# user who extracts it knows what to double-click.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"
STAGE="$DIST/allshare-client"

rm -rf "$STAGE"
mkdir -p "$STAGE"
cp -r "$ROOT/client/." "$STAGE/"

cat > "$STAGE/START HERE.txt" <<'TXT'
ALL SHARE
Powered by MMC

To use ALL SHARE, open the file named:

    index.html

Double-clicking it opens ALL SHARE in your browser. Nothing needs to be
installed, and there is no server to run on this device.

The first time you open it, ALL SHARE asks for the address of your ALL SHARE
service. Your PC shows that address on its "Add a device" screen, just above
the pairing code.
TXT

cd "$DIST"
rm -f allshare-client.zip
if command -v zip >/dev/null 2>&1; then
    zip -qr allshare-client.zip allshare-client
    echo "Done: $DIST/allshare-client.zip"
else
    echo "Done: $STAGE (install 'zip' to produce an archive)"
fi
