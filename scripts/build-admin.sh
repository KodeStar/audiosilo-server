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
#   scripts/build-admin.sh --vite-only  # npm ci + vite build, no typecheck (server CI's
#                                       # Go job: its admin-ui job runs the full recipe)
#
# Needs Node 24 (admin-ui/.nvmrc). Every mode fails on any CSP violation (a vite plugin).
set -euo pipefail

mode="${1:-}"
case "$mode" in
  '' | --build-only | --vite-only) ;;
  *) echo "usage: scripts/build-admin.sh [--build-only | --vite-only]" >&2; exit 2 ;;
esac

cd "$(dirname "$0")/../admin-ui"

npm ci --no-audit --no-fund
case "$mode" in
  --vite-only) exec npx vite build ;;
  --build-only) ;;
  *) npm run check ;;
esac
npm run build
