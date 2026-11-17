#!/usr/bin/env bash
# Boots a release build of the server as the LinuxServer.io image runs it (--data
# only, so the headless first run and the self-signed TLS default) and checks
# that it answers: /healthz, the embedded web player at /web/, and the native
# SQLite engine named in its start log. release.yml runs it on the release's
# linux_amd64 archive; ci.yml on a snapshot build of every pull request, so a
# build that doesn't start shows before a tag (server#217).
#
#   scripts/smoke-boot.sh path/to/audiosilo [work-dir]
#
# The work dir (default: a new temporary one) holds the data folder and the log.
# SMOKE_PORT picks the port (default 18080).
set -euo pipefail

bin="${1:?usage: scripts/smoke-boot.sh path/to/audiosilo [work-dir]}"
work="${2:-$(mktemp -d)}"
port="${SMOKE_PORT:-18080}"
mkdir -p "$work"
log="$work/server.log"

# The one-time credentials banner (stdout) stays out of the log; ffmpeg and
# ffprobe are off so nothing downloads.
AUDIOSILO_BIND="127.0.0.1:$port" AUDIOSILO_UPDATE_CHECK=false \
  "$bin" --data "$work/data" --ffprobe "" --ffmpeg "" > /dev/null 2> "$log" &
server=$!
# The log (stderr) goes to the output on the way out, pass or fail.
trap 'kill "$server" 2>/dev/null || true; cat "$log" >&2' EXIT

curl -kfsS --retry 30 --retry-delay 1 --retry-connrefused "https://127.0.0.1:$port/healthz"
# Not grep -q: under pipefail its early exit can fail curl (SIGPIPE).
curl -kfsSL "https://127.0.0.1:$port/web/" | grep -i '<html' > /dev/null
# The start logs its engine.
grep -q 'database: sqlite.*engine=native' "$log"
echo "smoke-boot: $bin started and answered"
