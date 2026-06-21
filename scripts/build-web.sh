#!/usr/bin/env bash
#
# Dev helper: build the AudioSilo web player (the audiosilo-frontend Expo export)
# into a local directory you can point the server at with AUDIOSILO_WEB_DIR.
#
# The player is NOT vendored into this repo. In production the Docker image bakes a
# pinned web build into /app/web (see Dockerfile); this script is only for running
# the server against a locally-built player during development.
#
# Usage:
#   scripts/build-web.sh                 # builds into <frontend>/dist
#   DEST=/tmp/asilo-web scripts/build-web.sh
#   FRONTEND_DIR=/path/to/audiosilo-frontend scripts/build-web.sh
set -euo pipefail

FRONTEND_DIR="${FRONTEND_DIR:-$HOME/dev/audiosilo/audiosilo-frontend}"
DEST="${DEST:-$FRONTEND_DIR/dist}"
BASE_URL="/web"

if [ ! -d "$FRONTEND_DIR" ]; then
  echo "error: frontend not found at $FRONTEND_DIR (set FRONTEND_DIR)" >&2
  exit 1
fi

echo "Building web export from $FRONTEND_DIR (baseUrl=$BASE_URL) -> $DEST"
rm -rf "$DEST"
(
  cd "$FRONTEND_DIR"
  EXPO_BASE_URL="$BASE_URL" npx expo export --platform web --output-dir "$DEST" --clear
)

if [ ! -f "$DEST/index.html" ]; then
  echo "error: export produced no $DEST/index.html" >&2
  exit 1
fi

echo
echo "Done. Run the server against it with:"
echo "  AUDIOSILO_WEB_DIR=\"$DEST\" ./bin/audiosilo --data ./data"
