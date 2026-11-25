#!/bin/sh
# Exercises docker-entrypoint.sh on the image's base (Alpine with su-exec), as
# root and as a --user, against folders the server's user can and can't use
# (server#368), with folders, a file and device nodes mounted inside the data folder (server#382),
# read-only mounts (server#383) and its healthcheck mode. Needs Docker and network access for
# `apk add su-exec`. Run from the repository root:
#
#   scripts/test-entrypoint.sh
#
# Not part of CI: run it when docker-entrypoint.sh changes.
set -eu

IMG=alpine:3.24
EP="$(pwd)/docker-entrypoint.sh"
[ -f "$EP" ] || { echo "run from the repository root" >&2; exit 2; }

RUN_ID="audiosilo-ep-test-$$"
context=$(mktemp -d)
touch "$context/volumes"
cleanup() {
  while IFS= read -r v; do docker volume rm -f "$v" >/dev/null 2>&1 || true; done <"$context/volumes"
  rm -rf "$context"
  docker image rm -f "$RUN_ID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Exercise the real checkdirs command. Only the long-running server and the
# healthcheck are replaced with a recorder of their arguments and identity.
arch=$(docker version --format '{{.Server.Arch}}')
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$context/checkdirs-bin" ./cmd/audiosilo
docker build -q -t "$RUN_ID" --build-arg IMG="$IMG" -f - "$context" >/dev/null <<'DOCKERFILE'
ARG IMG
FROM $IMG
RUN apk add --no-cache su-exec btrfs-progs
COPY checkdirs-bin /usr/local/bin/checkdirs-bin
RUN printf '#!/bin/sh\nif [ "$3" = checkdirs ]; then exec /usr/local/bin/checkdirs-bin "$@"; fi\necho "audiosilo $* as $(id -u):$(id -g)"\n' >/usr/local/bin/audiosilo && chmod 755 /usr/local/bin/audiosilo
DOCKERFILE

# vol NAME OWNER MODE: a volume whose root belongs to OWNER with MODE.
vol() {
  v="$RUN_ID-$1"
  docker volume create "$v" >/dev/null
  echo "$v" >>"$context/volumes"
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

# run DOCKER-ARGS...: the entrypoint printing the user the command runs as and
# whether it was told the container was started with --user.
run() {
  docker run --rm -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" "$@" "$RUN_ID" \
    sh /usr/local/bin/docker-entrypoint.sh \
    sh -c 'echo "ran as $(id -u):$(id -g) started_as_user=${AUDIOSILO_STARTED_AS_USER:-no}"' --data /data 2>&1 || true
}

# A chown refused as on a NAS share with ACLs: root without CAP_CHOWN.
NOCHOWN=--cap-drop=CHOWN

# 1. A fresh data volume: chowned, no warning, the server's user runs.
DATA=$(vol fresh 0:0 755)
out=$(run -v "$DATA:/data")
expect "fresh data folder" "$out" "ran as 1000:1000 started_as_user=no" "!WARNING"

# 2. PUID/PGID picked by the owner (Synology's 1026:100): no warning.
out=$(run -e PUID=1026 -e PGID=100 -v "$DATA:/data")
expect "PUID/PGID" "$out" "ran as 1026:100" "!WARNING"

# 3. A data folder whose chown is refused (an ACL share on a NAS), owned by
#    another user: the folder, both users, the failed chown, the fix.
NAS=$(vol nas 1026:100 700)
out=$(run "$NOCHOWN" -v "$NAS:/data")
expect "data folder of another owner" "$out" \
  "WARNING: the server can't write to /data: permission denied. The server runs as user 1000:1000 and the folder belongs to 1026:100\." \
  "Changing its owner to 1000:1000 failed" \
  "Set PUID=1026 and PGID=100 \(the folder's owner\) on the container, or give user 1000:1000 read and write access to the folder\." \
  "ran as 1000:1000"

# 4. A root-owned folder never gets PUID=0 advice.
ROOT=$(vol root 0:0 755)
out=$(run "$NOCHOWN" -v "$ROOT:/data")
expect "root-owned data folder" "$out" "belongs to 0:0" "making 1000:1000 its owner" "!PUID=0"

# 5. AUDIOSILO_BACKUP_DIR: chowned with the data folder, so backups can be made.
BK=$(vol backups 0:0 755)
out=$(run -e AUDIOSILO_BACKUP_DIR=/backups -v "$DATA:/data" -v "$BK:/backups")
expect "backups folder chowned" "$out" "!WARNING"
owner=$(docker run --rm -v "$BK:/v" "$RUN_ID" stat -c '%u:%g' /v)
expect "backups folder owner" "$owner" "^1000:1000$"

# 5b. Only the backups folder and AudioSilo's backups in it change owner: the
#     share may hold other apps' backups.
docker run --rm -v "$BK:/v" "$RUN_ID" sh -c \
  'mkdir -p /v/other && touch /v/other.tar /v/audiosilo-20261101-030000Z-scheduled.db && chown -R 0:0 /v'
run -e AUDIOSILO_BACKUP_DIR=/backups -v "$DATA:/data" -v "$BK:/backups" >/dev/null
owners=$(docker run --rm -v "$BK:/v" "$RUN_ID" sh -c \
  'stat -c "%n %u:%g" /v/other /v/other.tar /v/audiosilo-20261101-030000Z-scheduled.db')
expect "backups chown spares other files" "$owners" \
  "^/v/other 0:0$" "^/v/other.tar 0:0$" "^/v/audiosilo-20261101-030000Z-scheduled.db 1000:1000$"

# The write probe leaves existing names, including links, untouched and removes its temp file.
out=$(docker run --rm -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" "$RUN_ID" sh -ec '
  mkdir -p /data/backups && echo keep >/keep && ln -s /keep /data/backups/.audiosilo-write-check-123456 &&
    sh /usr/local/bin/docker-entrypoint.sh true --data /data &&
    test -L /data/backups/.audiosilo-write-check-123456 && cat /keep &&
    find /data/backups -name ".audiosilo-write-check-*" | wc -l' 2>&1 || true)
expect "backups write probe keeps existing files" "$out" "^keep$" "^[[:space:]]*1$" "!WARNING"

# 5c. Nor does what a link in the backups folder points at, whatever the link is
#     called: a share others can write to must not get root to re-own their pick.
docker run --rm -v "$BK:/v" "$RUN_ID" sh -c \
  'ln -s other.tar /v/audiosilo-link.db && ln -s other /v/.audiosilo-link && chown -R -h 0:0 /v'
run -e AUDIOSILO_BACKUP_DIR=/backups -v "$DATA:/data" -v "$BK:/backups" >/dev/null
owners=$(docker run --rm -v "$BK:/v" "$RUN_ID" sh -c \
  'stat -c "%n %u:%g" /v/other /v/other.tar /v/audiosilo-20261101-030000Z-scheduled.db')
expect "backups chown follows no link" "$owners" \
  "^/v/other 0:0$" "^/v/other.tar 0:0$" "^/v/audiosilo-20261101-030000Z-scheduled.db 1000:1000$"

# 5c'. Nor the folder a link at the backups folder's own name points at: the
#      server's user can leave one in its data folder for root to follow.
out=$(docker run --rm -e AUDIOSILO_BACKUP_DIR=/data/backups -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" "$RUN_ID" sh -ec '
  mkdir -p /data /target && ln -s /target /data/backups &&
    sh /usr/local/bin/docker-entrypoint.sh true --data /data && stat -c "%n %u:%g" /target' 2>&1 || true)
expect "backups folder link is not followed" "$out" "^/target 0:0$"

# A backups listing that outlasts the ownership pass's five seconds cannot
# hold startup. The stand-in records whether it reached the end of its wait.
out=$(docker run --rm -e AUDIOSILO_BACKUP_DIR=/backups -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" "$RUN_ID" sh -ec '
  mkdir -p /data /backups /test-bin
  cat >/test-bin/find << "SLOW_FIND"
#!/bin/sh
if [ "$1" = /backups/ ]; then
  echo started >/tmp/backup-walk
  sleep 8
  echo late >>/tmp/backup-walk
fi
exec /bin/busybox find "$@"
SLOW_FIND
  chmod +x /test-bin/find
  PATH=/test-bin:$PATH sh /usr/local/bin/docker-entrypoint.sh sh -c "cat /tmp/backup-walk && echo running" --data /data' 2>&1 || true)
expect "slow backups ownership walk is bounded" "$out" "^started$" "^running$" "!^late$"

# 5d. A trailing slash names the same folder: chowned, and not read again as a
#     folder of books.
out=$(run "$NOCHOWN" -e AUDIOSILO_BACKUP_DIR=/backups/ -v "$DATA:/data" -v "$NAS:/backups")
expect "backups folder with a trailing slash" "$out" "WARNING: the server can't write to /backups:" "!shows as offline"

# 6. A backups folder that refuses the chown is named the same way.
out=$(run "$NOCHOWN" -e AUDIOSILO_BACKUP_DIR=/backups -v "$DATA:/data" -v "$NAS:/backups")
expect "unwritable backups folder" "$out" "WARNING: the server can't write to /backups" "PUID=1026 and PGID=100"

# 6b. A read-only (:ro) mount: no owner can write to it, so no owner advice and
#     no failed chown, as the data folder, the backups folder and under --user.
out=$(run -v "$DATA:/data:ro")
expect "read-only data folder" "$out" \
  "WARNING: the server can't write to /data: read-only file system\." "mount it read and write \(in Docker, without :ro\)\." \
  "!belongs to" "!Changing its owner" "!PUID"
out=$(run -e AUDIOSILO_BACKUP_DIR=/backups -v "$DATA:/data" -v "$BK:/backups:ro")
expect "read-only backups folder" "$out" "WARNING: the server can't write to /backups: read-only file system\." "!belongs to"
out=$(run --user 1000:1000 -v "$DATA:/data:ro")
expect "read-only data folder under --user" "$out" "/data: read-only file system" "!user: \""

# 6c. A filesystem that is itself read-only under a mount that isn't (one the
#     host remounted read-only after errors): the same words, no owner advice.
out=$(docker run --rm --privileged -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" -v "$DATA:/data" "$RUN_ID" sh -c '
  mkdir -p /src /backups && mount -t tmpfs tmpfs /src && mount --bind /src /backups && mount -o remount,ro /src &&
    AUDIOSILO_BACKUP_DIR=/backups sh /usr/local/bin/docker-entrypoint.sh true --data /data' 2>&1 || true)
expect "read-only filesystem under a read and write mount" "$out" \
  "WARNING: the server can't write to /backups: read-only file system\." "!belongs to"

# The default backups folder is probed even when no override is configured.
out=$(run -v "$DATA:/data" -v "$BK:/data/backups:ro")
expect "read-only default backups" "$out" "can't write to /data/backups: read-only file system" "without :ro"

# 7. An audiobooks folder the server's user can't read (a library there is offline).
out=$(run -v "$DATA:/data" -v "$NAS:/audiobooks:ro")
expect "unreadable audiobooks" "$out" \
  "WARNING: the server can't read /audiobooks: permission denied. The server runs as user 1000:1000 and the folder belongs to 1026:100\." \
  "give user 1000:1000 read access" "shows as offline"
BOOKS=$(vol books 0:0 755)
out=$(run -v "$DATA:/data" -v "$BOOKS:/audiobooks:ro")
expect "readable audiobooks" "$out" "!WARNING"

# 8. Started with --user: no chown, the same checks, the fix names user: not PUID.
out=$(run --user 1000:1000 -v "$NAS:/data")
expect "--user" "$out" "ran as 1000:1000 started_as_user=1" \
  "Run the container as the folder's owner \(user: \"1026:100\"\)" "!PUID"

# 9. A folder the server's user owns but may not write to (its mode refuses it)
#    gets no PUID advice, whatever its group: the user is right already.
OWN=$(vol own 1000:100 500)
out=$(run --user 1000:1000 -v "$OWN:/data")
expect "own folder refused by its mode" "$out" \
  "the folder belongs to 1000:100\." "Give user 1000:1000 read and write access" "!PUID" "!user: "

# 10. Another spelling of / is never chowned (it would re-own the whole
#     container, the mounted books included): --data //, /. or /data/.., and the
#     same for AUDIOSILO_BACKUP_DIR.
for d in // /. /data/..; do
  out=$(docker run --rm -e AUDIOSILO_BACKUP_DIR="$d" -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" \
    -v "$DATA:/data" -v "$BOOKS:/audiobooks" "$RUN_ID" \
    sh /usr/local/bin/docker-entrypoint.sh sh -c 'stat -c "%n %u:%g" / /etc /audiobooks' --data "$d" 2>&1 || true)
  expect "the root spelled $d" "$out" "^/ 0:0$" "^/etc 0:0$" "^/audiobooks 0:0$"
done

# 11. No watcher of a bounded check is left behind for the server to carry as a
#     zombie (a shell builtin looks: nothing here may reap one).
# shellcheck disable=SC2016 # the container's shell expands these
leftover='for f in /proc/[0-9]*/comm; do read -r c <"$f" || continue; if [ "$c" = timeout ]; then echo LEFTOVER; fi; done; echo looked'
for u in "" "--user 1000:1000"; do
  # shellcheck disable=SC2086 # $u is zero or two words
  out=$(docker run --rm $u -e AUDIOSILO_BACKUP_DIR=/backups -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" \
    -v "$DATA:/data" -v "$BK:/backups" -v "$BOOKS:/audiobooks:ro" "$RUN_ID" \
    sh /usr/local/bin/docker-entrypoint.sh sh -c "$leftover" --data /data 2>&1 || true)
  expect "no watcher left ${u:-as root}" "$out" "^looked$" "!LEFTOVER"
done

# 12. A folder mounted inside the data folder (a backups share at /data/backups,
#     another app's files) keeps its owner and its files theirs; the data
#     folder's own files are chowned. Volumes share the host disk's device number,
#     so this is the mount point pruned by name (a device-number check alone would cross it), a
#     name with a space and a glob character too.
NEST=$(vol nest 1026:100 755)
ODD=$(vol odd 1026:100 755)
docker run --rm -v "$DATA:/v" "$RUN_ID" sh -c 'touch /v/audiosilo.db && chown 0:0 /v/audiosilo.db'
docker run --rm -v "$NEST:/v" -v "$ODD:/w" "$RUN_ID" sh -c 'touch /v/theirs.db /w/x && chown 1026:100 /v/theirs.db /w/x'
run -v "$DATA:/data" -v "$NEST:/data/backups" -v "$ODD:/data/odd [1]*" >/dev/null
owners=$(docker run --rm -v "$DATA:/data" -v "$NEST:/data/backups" -v "$ODD:/data/odd [1]*" "$RUN_ID" sh -c \
  'stat -c "%n %u:%g" /data/audiosilo.db /data/backups /data/backups/theirs.db "/data/odd [1]*" "/data/odd [1]*/x"')
expect "nested mount keeps its owner" "$owners" \
  "^/data/audiosilo.db 1000:1000$" "^/data/backups 1026:100$" "^/data/backups/theirs.db 1026:100$" \
  "^/data/odd \[1\]\* 1026:100$" "^/data/odd \[1\]\*/x 1026:100$"

# Mountinfo escapes a tab as \011. The following digit and literal backslash
# must survive decoding in both the root ownership walk and the user's check.
odd=$(printf '/data/tab\t7\\040')
out=$(run -v "$DATA:/data" -v "$ODD:$odd")
expect "startup with tab and octal digits" "$out" "ran as 1000:1000"
owner=$(docker run --rm -v "$ODD:/v" "$RUN_ID" stat -c '%u:%g' /v/x)
expect "tab and octal digits in a nested mount name" "$owner" "^1026:100$"

# On kernels with btrfs and loop devices, check that a nested subvolume's
# distinct device number does not keep its own data files from being prepared.
out=$(docker run --rm --privileged -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" "$RUN_ID" sh -ec '
  if ! grep -qw btrfs /proc/filesystems || ! losetup -f >/dev/null 2>&1; then
    echo "skip: kernel has no btrfs or loop device"; exit
  fi
  truncate -s 160M /disk && mkfs.btrfs -q /disk && mkdir /data &&
    mount -t btrfs -o loop /disk /data && btrfs subvolume create /data/nested &&
    touch /data/nested/ours && sh /usr/local/bin/docker-entrypoint.sh true --data /data &&
    stat -c "%n %u:%g" /data/nested/ours && umount /data' 2>&1 || true)
case "$out" in
  "skip: "*) echo "$out" ;;
  *) expect "data in a btrfs subvolume" "$out" "^/data/nested/ours 1000:1000$" ;;
esac

# 13. A file mounted inside the data folder (-v ./config.yaml:/data/config.yaml,
#     root's and 600 on the host) is chowned as it always was, so the server can
#     read it, while a folder mounted beside it keeps its owner. Both come from
#     another filesystem, so -xdev is crossed too.
FILEM=$(vol filem 0:0 755)
out=$(docker run --rm --privileged -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" -v "$FILEM:/data" "$RUN_ID" sh -c '
  mkdir -p /src /data/backups && touch /data/config.yaml && mount -t tmpfs tmpfs /src && mkdir /src/backups &&
    printf "listen: :8080\n" >/src/config.yaml && chmod 600 /src/config.yaml && touch /src/backups/theirs.db &&
    chown -R 1026:100 /src/backups && mount --bind /src/config.yaml /data/config.yaml &&
    mount --bind /src/backups /data/backups &&
    sh /usr/local/bin/docker-entrypoint.sh sh -c "cat /data/config.yaml && echo \"ran as \$(id -u):\$(id -g)\" &&
      stat -c \"%n %u:%g %a\" /data/config.yaml /data/backups /data/backups/theirs.db" --data /data' 2>&1 || true)
expect "file mounted in the data folder is chowned" "$out" \
  "^listen: :8080$" "ran as 1000:1000" "^/data/config.yaml 1000:1000 600$" \
  "^/data/backups 1026:100 " "^/data/backups/theirs.db 1026:100 " "WARNING: the server can't write to /data/backups" "!can't read /data/config.yaml"

# 14. A device node or a pipe mounted inside the data folder (a docker.sock, a
#     /dev/null) is no regular file: it keeps its owner, so a start never chowns
#     a host socket or device, while case 13's file is still chowned and its
#     folder still keeps its owner.
DEVM=$(vol devm 0:0 755)
out=$(docker run --rm --privileged -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" -v "$DEVM:/data" "$RUN_ID" sh -c '
  mkdir -p /src /data/backups && touch /data/config.yaml /data/null /data/pipe && mount -t tmpfs tmpfs /src &&
    mkdir /src/backups && touch /src/config.yaml && mknod /src/null c 1 3 && mkfifo /src/pipe &&
    chown -R 1026:100 /src && mount --bind /src/config.yaml /data/config.yaml &&
    mount --bind /src/backups /data/backups && mount --bind /src/null /data/null && mount --bind /src/pipe /data/pipe &&
    sh /usr/local/bin/docker-entrypoint.sh sh -c "stat -c \"%n %u:%g %F\" /data/config.yaml /data/backups /data/null /data/pipe" --data /data' 2>&1 || true)
expect "device and pipe mounted in the data folder keep their owner" "$out" \
  "^/data/config.yaml 1000:1000 regular" "^/data/backups 1026:100 directory$" \
  "^/data/null 1026:100 character special" "^/data/pipe 1026:100 fifo" "can't write to /data/backups" "!can't read /data/(null|pipe)"

# The parent is owned by the server but its mode denies writes. An unreadable
# read-only config mount has its own warning and cannot explain the parent.
out=$(docker run --rm --privileged -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" "$RUN_ID" sh -c '
  mkdir /data /src && touch /src/config /data/config.yaml && chmod 600 /src/config &&
    chown 1000:1000 /data && chmod 500 /data && mount --bind /src/config /data/config.yaml &&
    mount -o remount,bind,ro /data/config.yaml &&
    sh /usr/local/bin/docker-entrypoint.sh true --data /data' 2>&1 || true)
expect "unreadable read-only config mount" "$out" "can't write to /data:" "can't read /data/config.yaml" \
  "file belongs to 0:0" "!Changing its owner" "!shows as offline"

# 15. The healthcheck mode asks the data folder the start resolved from the
#     command's --data (any spelling, a relative one made absolute), /data when
#     no start recorded one, as the server's user. hc START DOCKER-ARGS...: a
#     start with the words of START ("" for none), then the healthcheck.
hc() {
  start="$1"
  shift
  docker run --rm -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" "$@" "$RUN_ID" sh -c '
    if [ -n "$1" ]; then sh /usr/local/bin/docker-entrypoint.sh true $1 >/dev/null 2>&1; fi
    sh /usr/local/bin/docker-entrypoint.sh healthcheck' sh "$start" 2>&1 || true
}
expect "healthcheck on --data /config" "$(hc "--data /config")" "^audiosilo --data /config healthcheck as 1000:1000$"
expect "healthcheck with no start recorded" "$(hc "")" "^audiosilo --data /data healthcheck as 1000:1000$"
expect "healthcheck on --data=/config/ as PUID:PGID" "$(hc "--data=/config/" -e PUID=1026 -e PGID=100)" \
  "^audiosilo --data /config healthcheck as 1026:100$"
expect "healthcheck on a relative --data" "$(hc "--data config" -w /srv)" "^audiosilo --data /srv/config healthcheck as"
expect "healthcheck on a relative --data from /" "$(hc "-data config")" "^audiosilo --data /config healthcheck as"
expect "healthcheck under --user" "$(hc "--data /config" --user 1026:100)" \
  "^audiosilo --data /config healthcheck as 1026:100$"

# 15b. A link left at the record's name is replaced, never written through: the
#      server's user could plant one for root to follow at the next start.
out=$(docker run --rm -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" "$RUN_ID" sh -c '
  echo keep >/etc/victim && ln -s /etc/victim /tmp/audiosilo-data-dir &&
    sh /usr/local/bin/docker-entrypoint.sh true --data /config >/dev/null 2>&1
  cat /etc/victim; [ -L /tmp/audiosilo-data-dir ] || echo "record $(cat /tmp/audiosilo-data-dir)"' 2>&1 || true)
expect "a link at the record is replaced" "$out" "^keep$" "^record /config$"

# 15c. A record that can't be replaced (a folder at its name) never stops the
#      start: the server still runs and the check falls back to /data.
out=$(docker run --rm -v "$EP:/usr/local/bin/docker-entrypoint.sh:ro" "$RUN_ID" sh -c '
  mkdir /tmp/audiosilo-data-dir &&
    sh /usr/local/bin/docker-entrypoint.sh audiosilo --data /config 2>/dev/null &&
    sh /usr/local/bin/docker-entrypoint.sh healthcheck' 2>&1 || true)
expect "a folder at the record keeps the start" "$out" "^audiosilo --data /config as 1000:1000$" \
  "^audiosilo --data /data healthcheck as 1000:1000$"

[ "$FAILS" -eq 0 ] || { echo "$FAILS failed"; exit 1; }
echo "all passed"
