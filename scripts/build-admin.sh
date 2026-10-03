#!/usr/bin/env bash
#
# Builds the admin console (admin-ui/, React + Vite) into
# internal/web/adminui/dist, which `go build` then embeds. This is the one build
# recipe: server CI, GoReleaser, the manager's workflows and the docs screenshot
# pipeline all call it (only the Dockerfile inlines it, for layer caching).
#
# Usage:
#   scripts/build-admin.sh              # npm ci + check + build
#   scripts/build-admin.sh --build-only # npm ci + build (release/consumer pipelines)
#
# Needs Node 24 (admin-ui/.nvmrc). The build itself fails on any CSP violation.
set -euo pipefail

cd "$(dirname "$0")/../admin-ui"

npm ci --no-audit --no-fund
if [ "${1:-}" != "--build-only" ]; then
  npm run check
fi
npm run build
