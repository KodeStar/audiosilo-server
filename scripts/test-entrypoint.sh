#!/bin/sh
# Exercises docker-entrypoint.sh on the image's base (Alpine with su-exec), as
# root and as a --user, against folders the server's user can and can't use
# (server#368). Needs Docker and network access for `apk add su-exec`. Run from
# the repository root:
#
#   scripts/test-entrypoint.sh
#
# Not part of CI: run it when docker-entrypoint.sh changes.
set -eu

IMG=alpine:3.24
EP="$(pwd)/docker-entrypoint.sh"
[ -f "$EP" ] || { echo "run from the repository root" >&2; exit 2; }

RUN_ID="audiosilo-ep-test-$$"
VOLS=""
cleanup() {
  for v in $VOLS; do docker volume rm -f "$v" >/dev/null 2>&1 || true; done
  docker image rm -f "$RUN_ID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# A base with su-exec, as the image has it.
printf 'FROM %s\nRUN apk add --no-cache su-exec\n' "$IMG" | docker build -q -t "$RUN_ID" - >/dev/null

# vol NAME OWNER MODE: a volume whose root belongs to OWNER with MODE.
vol() {
  v="$RUN_ID-$1"
  docker volume create "$v" >/dev/null
  VOLS="$VOLS $v"
  docker run --rm -v "$v:/v" "$RUN_ID" sh -c "chown $2 /v && chmod $3 /v"
  printf '%s' "$v"
}

FAILS=0
# expect NAME OUTPUT PATTERN...: each grep -E PATTERN is in OUTPUT ("!PATTERN": is not).
expect() {
  name="$1" out="$2"
  shift 2
  for p in "$@"; do
    case "$p" in
      !*) if printf '%s\n' "$out" | grep -qE -- "${p#!}"; then bad="has $p"; else continue; fi ;;
      *) if printf '%s\n' "$out" | grep -qE -- "$p"; then continue; else bad="lacks $p"; fi ;;
    esac
    echo "FAIL $name: output $bad"
    printf '%s\n' "$out" | sed 's/^/    /'
    FAILS=$((FAILS + 1))
    return 0
  done
  echo "ok   $name"
}

# run DOCKER-ARGS...: the entrypoint printing the user the command runs as.
run() {
  docker run --rm -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" "$@" "$RUN_ID" \
    sh /usr/local/bin/docker-entrypoint.sh sh -c 'echo "ran as $(id -u):$(id -g)"' --data /data 2>&1 || true
}

# 1. A fresh data volume: chowned, no warning, the server's user runs.
DATA=$(vol fresh 0:0 755)
out=$(run -v "$DATA:/data")
expect "fresh data folder" "$out" "ran as 1000:1000" "!WARNING"

# 2. PUID/PGID picked by the owner (Synology's 1026:100): no warning.
out=$(run -e PUID=1026 -e PGID=100 -v "$DATA:/data")
expect "PUID/PGID" "$out" "ran as 1026:100" "!WARNING"

# 3. A data folder whose chown is refused (read-only here, an ACL share on a NAS),
#    owned by another user: the folder, both users, the failed chown, the fix.
NAS=$(vol nas 1026:100 700)
out=$(run -v "$NAS:/data:ro")
expect "data folder of another owner" "$out" \
  "WARNING: the server can't write to /data: it runs as user 1000:1000 and the folder belongs to 1026:100\." \
  "Changing its owner to 1000:1000 failed" \
  "Fix: set PUID=1026 and PGID=100 \(the folder's owner\) on the container, or give user 1000:1000 read and write access to the folder on the host\." \
  "ran as 1000:1000"

# 4. A root-owned folder never gets PUID=0 advice.
ROOT=$(vol root 0:0 755)
out=$(run -v "$ROOT:/data:ro")
expect "root-owned data folder" "$out" "belongs to 0:0" "making 1000:1000 its owner" "!PUID=0"

# 5. AUDIOSILO_BACKUP_DIR: chowned with the data folder, so backups can be made.
BK=$(vol backups 0:0 755)
out=$(run -e AUDIOSILO_BACKUP_DIR=/backups -v "$DATA:/data" -v "$BK:/backups")
expect "backups folder chowned" "$out" "!WARNING"
owner=$(docker run --rm -v "$BK:/v" "$RUN_ID" stat -c '%u:%g' /v)
expect "backups folder owner" "$owner" "^1000:1000$"

# 6. A backups folder that refuses the chown is named the same way.
out=$(run -e AUDIOSILO_BACKUP_DIR=/backups -v "$DATA:/data" -v "$NAS:/backups:ro")
expect "unwritable backups folder" "$out" "WARNING: the server can't write to /backups" "PUID=1026 and PGID=100"

# 7. An audiobooks folder the server's user can't read (a library there is offline).
out=$(run -v "$DATA:/data" -v "$NAS:/audiobooks:ro")
expect "unreadable audiobooks" "$out" \
  "WARNING: the server can't read /audiobooks: it runs as user 1000:1000 and the folder belongs to 1026:100\." \
  "give user 1000:1000 read access" "shows as offline"
BOOKS=$(vol books 0:0 755)
out=$(run -v "$DATA:/data" -v "$BOOKS:/audiobooks:ro")
expect "readable audiobooks" "$out" "!WARNING"

# 8. Started with --user: no chown, the same checks, the fix names user: not PUID.
out=$(run --user 1000:1000 -v "$NAS:/data")
expect "--user" "$out" "ran as 1000:1000" \
  "run the container as the folder's owner \(user: \"1026:100\"\)" "!PUID"

[ "$FAILS" -eq 0 ] || { echo "$FAILS failed"; exit 1; }
echo "all passed"
