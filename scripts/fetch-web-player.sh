#!/usr/bin/env bash
# Populate internal/web/player/ with the prebuilt web-player export so a release
# build can bake it into the binary via `-tags embedplayer`. The export comes from
# the pinned web image the frontend CI publishes (the same one the Dockerfile bakes
# in), so the server and bundled player ship as one known-compatible artifact.
#
# Usage:  WEB_IMAGE=ghcr.io/kodestar/audiosilo-web:<tag> scripts/fetch-web-player.sh
# Skipped for local snapshot builds (goreleaser --skip=before); the committed
# internal/web/player/.gitkeep keeps the embed compiling without a real player.
set -euo pipefail

WEB_IMAGE="${WEB_IMAGE:-ghcr.io/kodestar/audiosilo-web:latest}"
DEST="internal/web/player"

echo "fetch-web-player: populating $DEST from $WEB_IMAGE"
rm -rf "$DEST"
mkdir -p "$DEST"

# The web image is `FROM scratch` (see audiosilo-frontend/Dockerfile.web) with no
# ENTRYPOINT/CMD, so a bare `docker create` is rejected ("no command specified").
# Supply a placeholder command: `docker create` only records it in the container
# config — the container is never started (we just `docker cp` out of its layer), so
# the command is never executed and need not exist in the image.
cid="$(docker create "$WEB_IMAGE" /audiosilo-web-export)"
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true' EXIT
# The web image holds the static export at /web (built with baseUrl=/web).
docker cp "$cid:/web/." "$DEST/"

if [ ! -f "$DEST/index.html" ]; then
  echo "fetch-web-player: ERROR — $DEST/index.html missing after copy" >&2
  exit 1
fi
echo "fetch-web-player: done ($(find "$DEST" -type f | wc -l | tr -d ' ') files)"
