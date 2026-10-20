#!/usr/bin/env bash
# Renders scripts/og.html to site/og.png (1200x630) with headless Chrome.
# Pass "dark" to render the dark version instead.
set -euo pipefail
cd "$(dirname "$0")/.."
CHROME=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
OUT=${OUT:-site/og.png}
QUERY=
if [ "${1:-}" = dark ]; then QUERY='?dark'; fi
"$CHROME" --headless --disable-gpu --hide-scrollbars --force-device-scale-factor=1 \
  --window-size=1200,630 --screenshot="$PWD/$OUT" "file://$PWD/scripts/og.html$QUERY" 2>/dev/null
magick "$OUT" -strip -define png:compression-level=9 "$OUT"
file "$OUT"
