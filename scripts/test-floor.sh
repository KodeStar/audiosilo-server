#!/usr/bin/env bash
# The per-package test floor (server#220): every Go package under internal/ and
# pkg/ has at least one *_test.go, or one of its files carries a line
#
#   // no-tests: <why>
#
# (a test helper package, one exercised through another package's tests). Lists
# each package that has neither and fails. CI runs it in the lint job.
set -euo pipefail
cd "$(dirname "$0")/.."

missing=0
while read -r pkg dir; do
  [ -n "$pkg" ] || continue
  if grep -qsE '^// no-tests: [^[:space:]]' "$dir"/*.go; then continue; fi
  echo "test floor: $pkg has no tests (add a *_test.go, or a '// no-tests: <why>' line)" >&2
  missing=$((missing + 1))
done < <(go list -f '{{if and (eq (len .TestGoFiles) 0) (eq (len .XTestGoFiles) 0)}}{{.ImportPath}} {{.Dir}}{{end}}' ./internal/... ./pkg/...)

if [ "$missing" -gt 0 ]; then
  echo "test floor: $missing package(s) without tests" >&2
  exit 1
fi
echo "test floor: every package has tests or says why not"
