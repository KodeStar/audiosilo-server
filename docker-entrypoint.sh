#!/bin/sh
# Run the server as PUID:PGID (default 1000:1000) so SQLite can write the data dir
# no matter how the host-mounted volume is owned - the usual NAS pain point. The
# container starts as root only to chown /data (and AUDIOSILO_BACKUP_DIR's folder
# and the backups in it, when it is set) and drop privileges; the server itself
# never runs as root. On NAS setups set PUID/PGID to the owner of your appdata
# (Unraid: PUID=99 PGID=100; Synology: often 1026:100; or run `id` for your user).
#
# A chown can fail without saying so (a NAS share with ACLs, a read-only mount), so
# the entrypoint then checks, as the user the server runs as, that the data folder
# and the backups folder can be written and that each other folder mounted into the
# container (the audiobooks) can be read. For one that can't, it prints the folder,
# the user, the folder's owner and the fix (server#368). It warns and carries on:
# the server's own start-up error says the same about the data folder. Each check
# of a mounted folder is given $WAIT seconds: a share whose server is gone is said
# not to answer, and never holds the start.
#
# If the container is started with an explicit non-root user (docker `--user` /
# compose `user:`), we skip the chown/drop, run the same checks and exec as that
# user.
set -e

say() { printf 'audiosilo: %s\n' "$*" >&2; }

# The --data folder the command names (the image's CMD says /data).
data_dir() {
  d=/data
  while [ $# -gt 0 ]; do
    case "$1" in
      --data | -data)
        if [ $# -gt 1 ]; then d="$2"; shift; fi
        ;;
      --data=* | -data=*) d="${1#*=}" ;;
    esac
    shift
  done
  printf '%s' "$d"
}

# $1 without its trailing slashes ("/backups/" is "/backups", as mountinfo names it).
trim() {
  p="$1"
  while [ "$p" != "/" ] && [ "${p%/}" != "$p" ]; do p="${p%/}"; done
  printf '%s' "$p"
}

# Whether $1 is an absolute folder other than the container's root, however that
# is spelled ("//", "/.", "/data/..", a link to it): never chown the whole system.
usable_path() {
  case "$1" in
    /*) ;;
    *) return 1 ;;
  esac
  case "$1/" in
    */../*) return 1 ;; # may step back up to /, even before the folder exists
  esac
  if timeout "$WAIT" test "$1" -ef /; then return 1; fi
  return 0
}

# The fix for folder $1 the server's user can't $2 ("read" or "read and write").
# $3 is set when this entrypoint failed to change the folder's owner.
explain() {
  dir="$1" access="$2" chowned="$3"
  owner=$(timeout "$WAIT" stat -c '%u:%g' "$dir" 2>/dev/null || true)
  if [ "$access" = "read" ]; then verb="read"; else verb="write to"; fi
  if [ -n "$owner" ]; then
    say "WARNING: the server can't $verb $dir: it runs as user $RUN_AS and the folder belongs to $owner."
  else
    say "WARNING: the server can't $verb $dir: it runs as user $RUN_AS."
  fi
  if [ -n "$chowned" ]; then
    say "  Changing its owner to $RUN_AS failed (a NAS share with ACLs or a read-only mount refuses it)."
  fi
  give="give user $RUN_AS $access access to the folder on the host"
  case "${owner%%:*}" in
    "${RUN_AS%%:*}" | "")
      # The server's user owns it (its mode or an ACL refuses it, as
      # fsaccess.Hint says), or the owner can't be read.
      say "  Fix: $give."
      ;;
    0)
      # Never advise running the server as root.
      say "  Fix: $give, for example by making $RUN_AS its owner."
      ;;
    *)
      if [ -n "$AS_ROOT" ]; then
        say "  Fix: set PUID=${owner%%:*} and PGID=${owner#*:} (the folder's owner) on the container, or $give."
      else
        say "  Fix: run the container as the folder's owner (user: \"$owner\"), or $give."
      fi
      ;;
  esac
}

# Whether $1 is a folder, asked within $WAIT.
is_dir() { timeout "$WAIT" test -d "$1"; }

# Whether the server's user can make a file in folder $1, within $WAIT. The file
# is mktemp's, never a name known ahead: in a shared folder a link planted at
# such a name would have the check empty the file it points at.
can_write() {
  as_user timeout "$WAIT" sh -c 't=$(mktemp "$1/.audiosilo-write-check.XXXXXX") && rm -f "$t"' sh "$1" >/dev/null 2>&1
}

# Whether the server's user can list folder $1 (a file mounted in passes): the
# first entry is enough, however many the folder holds. Bounded: a hard-mounted
# share whose server is gone can block for minutes, and a slow answer counts as
# unreadable rather than holding the start.
can_read() {
  as_user timeout "$WAIT" find "$1" -mindepth 1 -maxdepth 1 -print -quit >/dev/null 2>&1
}

# The folders mounted into the container that may hold books: not the system's
# (the rule of the server's setup hints, library.MountHints), not a pseudo
# filesystem, not the data or backups folder.
library_mounts() {
  [ -r /proc/self/mountinfo ] || return 0
  awk '{ for (i = 6; i <= NF; i++) if ($i == "-") { print $(i + 1), $5; break } }' /proc/self/mountinfo |
    while read -r fstype point; do
      case "$fstype" in
        proc | sysfs | devtmpfs | devpts | tmpfs | ramfs | cgroup | cgroup2 | mqueue | securityfs | debugfs | \
          tracefs | pstore | bpf | fusectl | configfs | hugetlbfs | autofs | binfmt_misc | nsfs | rpc_pipefs | \
          efivarfs | selinuxfs | squashfs | overlay) continue ;;
      esac
      point=$(printf '%b' "$point") # mountinfo escapes a space as \040
      case "$point" in
        / | /proc | /sys | /tmp | /dev | /run | /boot | /snap | /proc/* | /sys/* | /dev/* | /run/* | /etc/* | /boot/* | /snap/*) continue ;;
        "$DATA" | "$DATA"/*) continue ;;
      esac
      if [ -n "$BACKUPS" ] && [ "$point" = "$BACKUPS" ]; then continue; fi
      printf '%s\n' "$point"
    done
  return 0
}

# Says why folder $1 failed a check that ended with status $2. One cut off by
# timeout (BusyBox ends it with SIGTERM, 143; GNU's timeout says 124) is a share
# that didn't answer, not a folder that refuses the server's user: no owner
# advice for it. $3 and $4 are explain's access and failed chown.
report() {
  case "$2" in
    124 | 143)
      say "WARNING: $1 didn't answer within $WAIT seconds: a network share that is slow, or whose server is gone."
      ;;
    *) explain "$1" "$3" "$4" ;;
  esac
}

# Checks that the server's user can write to folder $1 ($2: its chown failed). A
# folder that isn't there yet is the server's to make.
check_write() {
  rc=0
  is_dir "$1" || rc=$?
  case "$rc" in
    0) can_write "$1" || rc=$? ;;
    124 | 143) ;;
    *) return 0 ;;
  esac
  if [ "$rc" -ne 0 ]; then report "$1" "$rc" "read and write" "$2"; fi
}

# Checks each folder as the server's user and explains the ones it can't use.
check_folders() {
  check_write "$DATA" "$DATA_CHOWN_FAILED"
  if [ -n "$BACKUPS" ]; then check_write "$BACKUPS" "$BACKUPS_CHOWN_FAILED"; fi
  library_mounts | while read -r m; do
    rc=0
    can_read "$m" || rc=$?
    if [ "$rc" -ne 0 ]; then
      report "$m" "$rc" "read" ""
      say "  Until then, a library in that folder shows as offline."
    fi
  done
  return 0
}

# Ends the watcher BusyBox's timeout leaves behind each command for up to a second.
# As PID 1 this script is the watchers' parent, and the server it becomes reaps
# no child it didn't start: one still alive at the hand-over would stay under it
# as a zombie for good. Waiting on pgrep is what lets this shell collect them.
# (Not PID 1, under docker's --init: that init reaps them, and nothing is done.)
reap_watchers() {
  [ "$$" = 1 ] || return 0
  pkill -P 1 -x timeout 2>/dev/null || true
  n=0
  while [ "$n" -lt 50 ] && pgrep -P 1 -x timeout >/dev/null 2>&1; do n=$((n + 1)); done
  return 0
}

WAIT=5 # seconds a check of a mounted folder may take
DATA=$(trim "$(data_dir "$@")")
BACKUPS=$(trim "${AUDIOSILO_BACKUP_DIR:-}")
if ! usable_path "$BACKUPS"; then BACKUPS=""; fi
DATA_CHOWN_FAILED=""
BACKUPS_CHOWN_FAILED=""

if [ "$(id -u)" = "0" ]; then
  PUID="${PUID:-1000}"
  PGID="${PGID:-1000}"
  RUN_AS="$PUID:$PGID"
  AS_ROOT=1
  as_user() { su-exec "$RUN_AS" "$@"; }
  if usable_path "$DATA"; then
    chown -R "$RUN_AS" "$DATA" 2>/dev/null || DATA_CHOWN_FAILED=1
  fi
  if [ -n "$BACKUPS" ]; then
    # The folder and AudioSilo's own backup files in it, never the rest (it may be
    # a share other apps keep their backups in too) and never what a link in it
    # points at: files only, and chown -h. Each step bounded, as the checks are:
    # the share's server may be gone, and the server starts without its backups.
    timeout "$WAIT" mkdir -p "$BACKUPS" 2>/dev/null || true
    { timeout "$WAIT" chown "$RUN_AS" "$BACKUPS" &&
      timeout "$WAIT" find "$BACKUPS/" -maxdepth 1 -type f \( -name 'audiosilo-*.db' -o -name '.audiosilo-*' \) \
        -exec chown -h "$RUN_AS" {} + ; } 2>/dev/null || BACKUPS_CHOWN_FAILED=1
  fi
  check_folders
  reap_watchers
  exec su-exec "$RUN_AS" "$@"
fi

RUN_AS="$(id -u):$(id -g)"
AS_ROOT=""
as_user() { "$@"; }
check_folders
reap_watchers
exec "$@"
