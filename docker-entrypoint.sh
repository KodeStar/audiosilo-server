#!/bin/sh
# Run the server as PUID:PGID (default 1000:1000) so SQLite can write the data dir
# no matter how the host-mounted volume is owned - the usual NAS pain point. The
# container starts as root only to chown /data (and AUDIOSILO_BACKUP_DIR when it is
# set) and drop privileges; the server itself never runs as root. On NAS setups set
# PUID/PGID to the owner of your appdata (Unraid: PUID=99 PGID=100; Synology: often
# 1026:100; or run `id` for your user).
#
# A chown can fail without saying so (a NAS share with ACLs, a read-only mount), so
# the entrypoint then checks, as the user the server runs as, that the data folder
# and the backups folder can be written and that each other folder mounted into the
# container (the audiobooks) can be read. For one that can't, it prints the folder,
# the user, the folder's owner and the fix (server#368). It warns and carries on:
# the server's own start-up error says the same about the data folder.
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

# Whether $1 is an absolute folder other than / (never chown the whole system).
usable_path() {
  case "$1" in
    / | "") return 1 ;;
    /*) return 0 ;;
  esac
  return 1
}

# The fix for folder $1 the server's user can't $2 ("read" or "read and write").
# $3 is set when this entrypoint failed to change the folder's owner.
explain() {
  dir="$1" access="$2" chowned="$3"
  owner=$(stat -c '%u:%g' "$dir" 2>/dev/null || true)
  if [ "$access" = "read" ]; then verb="read"; else verb="write to"; fi
  if [ -n "$owner" ]; then
    say "WARNING: the server can't $verb $dir: it runs as user $RUN_AS and the folder belongs to $owner."
  else
    say "WARNING: the server can't $verb $dir: it runs as user $RUN_AS."
  fi
  if [ -n "$chowned" ]; then
    say "  Changing its owner to $RUN_AS failed (a NAS share with ACLs or a read-only mount refuses it)."
  fi
  case "$owner" in
    "$RUN_AS" | "")
      say "  Fix: give user $RUN_AS $access access to the folder on the host."
      ;;
    0:*)
      # Never advise running the server as root.
      say "  Fix: give user $RUN_AS $access access to the folder on the host, for example by making $RUN_AS its owner."
      ;;
    *)
      if [ -n "$AS_ROOT" ]; then
        say "  Fix: set PUID=${owner%%:*} and PGID=${owner#*:} (the folder's owner) on the container, or give user $RUN_AS $access access to the folder on the host."
      else
        say "  Fix: run the container as the folder's owner (user: \"$owner\"), or give user $RUN_AS $access access to the folder on the host."
      fi
      ;;
  esac
}

# Whether the server's user can make a file in folder $1.
can_write() {
  as_user sh -c 't="$1/.audiosilo-write-check.$$"; : >"$t" && rm -f "$t"' sh "$1" >/dev/null 2>&1
}

# Whether the server's user can list folder $1.
can_read() {
  as_user ls "$1" >/dev/null 2>&1
}

# The folders mounted into the container that may hold books: not the system's
# (the rule of the server's setup hints, library.MountHints), not a pseudo
# filesystem, not the data or backups folder.
library_mounts() {
  [ -r /proc/self/mountinfo ] || return 0
  awk '{ for (i = 6; i <= NF; i++) if ($i == "-") { print $(i + 1), $5; break } }' /proc/self/mountinfo |
    while read -r fstype point; do
      point=$(printf '%b' "$point") # mountinfo escapes a space as \040
      case "$fstype" in
        proc | sysfs | devtmpfs | devpts | tmpfs | ramfs | cgroup | cgroup2 | mqueue | securityfs | debugfs | \
          tracefs | pstore | bpf | fusectl | configfs | hugetlbfs | autofs | binfmt_misc | nsfs | rpc_pipefs | \
          efivarfs | selinuxfs | squashfs | overlay) continue ;;
      esac
      case "$point" in
        / | /proc | /sys | /tmp | /dev | /run | /boot | /snap | /proc/* | /sys/* | /dev/* | /run/* | /etc/* | /boot/* | /snap/*) continue ;;
        "$DATA" | "$DATA"/*) continue ;;
      esac
      if [ -n "$BACKUPS" ] && [ "$point" = "$BACKUPS" ]; then continue; fi
      if [ -d "$point" ]; then printf '%s\n' "$point"; fi
    done
  return 0
}

# Checks each folder as the server's user and explains the ones it can't use.
check_folders() {
  if [ -d "$DATA" ] && ! can_write "$DATA"; then
    explain "$DATA" "read and write" "$DATA_CHOWN_FAILED"
  fi
  if [ -n "$BACKUPS" ] && [ -d "$BACKUPS" ] && ! can_write "$BACKUPS"; then
    explain "$BACKUPS" "read and write" "$BACKUPS_CHOWN_FAILED"
  fi
  library_mounts | while read -r m; do
    if ! can_read "$m"; then
      explain "$m" "read" ""
      say "  Until it can, a library in that folder shows as offline."
    fi
  done
  return 0
}

DATA=$(data_dir "$@")
BACKUPS=""
if usable_path "${AUDIOSILO_BACKUP_DIR:-}"; then BACKUPS="$AUDIOSILO_BACKUP_DIR"; fi
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
    mkdir -p "$BACKUPS" 2>/dev/null || true
    chown -R "$RUN_AS" "$BACKUPS" 2>/dev/null || BACKUPS_CHOWN_FAILED=1
  fi
  check_folders
  exec su-exec "$RUN_AS" "$@"
fi

RUN_AS="$(id -u):$(id -g)"
AS_ROOT=""
as_user() { "$@"; }
check_folders
exec "$@"
