#!/usr/bin/env bash
#
# Builds the admin console (admin-ui/, React + Vite) into
# internal/web/adminui/dist, which `go build` then embeds. CI, the Dockerfile and
# GoReleaser run the same steps; use this locally before `go build` to get the
# real console instead of the "console not built" page.
#
# Usage:
#   scripts/build-admin.sh          # npm ci + check + build
#   SKIP_CHECK=1 scripts/build-admin.sh   # build only (faster)
#
# Needs Node 24 (admin-ui/.nvmrc).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT/admin-ui"

npm ci
if [ "${SKIP_CHECK:-0}" != "1" ]; then
  npm run check
fi
npm run build
echo "Admin console built into internal/web/adminui/dist. Now: go build ./cmd/audiosilo"
