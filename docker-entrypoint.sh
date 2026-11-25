#!/bin/sh
# Run the server as PUID:PGID (default 1000:1000) so SQLite can write the data dir
# no matter how the host-mounted volume is owned - the usual NAS pain point. The
# container starts as root only to chown /data (and AUDIOSILO_BACKUP_DIR's folder
# and the backups in it, when it is set) and drop privileges; the server itself
# never runs as root. The chown of /data stops at a folder mounted inside it (a
# backups share at /data/backups keeps its owner, server#382); a file mounted
# inside it (a config.yaml) is chowned. On NAS setups set PUID/PGID to the owner
# of your appdata (Unraid: PUID=99 PGID=100; Synology: often 1026:100; or run
# `id` for your user).
#
# A chown can fail without saying so (a NAS share with ACLs, a read-only mount), so
# the entrypoint runs audiosilo checkdirs as the server user. It checks for write
# access to the data and backups folders and read access to mounted content.
# When a check fails it prints the path,
# the user, the folder's owner and the fix (server#368). It warns and carries on:
# the server's own start-up error says the same about the data folder. Each check
# of a mounted folder is given $WAIT seconds: a share whose server is gone is said
# not to answer, and never holds the start.
#
# If the container is started with an explicit non-root user (docker `--user` /
# compose `user:`), we skip the chown/drop, run the same checks and exec as that
# user, with AUDIOSILO_STARTED_AS_USER=1 so the server's own fix names `user:`
# too, not PUID/PGID (internal/fsaccess).
#
# `docker-entrypoint.sh healthcheck` is the image's HEALTHCHECK: it runs
# `audiosilo --data <folder> healthcheck` as the server's user on the data
# folder the start resolved from the command's --data, recorded in $DATA_RECORD
# since a check never sees the start's command (/data when there is no record).
set -e

DATA_RECORD=/tmp/audiosilo-data-dir

if [ "$#" = 1 ] && [ "$1" = healthcheck ]; then
  d=""
  IFS= read -r d 2>/dev/null <"$DATA_RECORD" || true # the record ends in no newline; IFS= keeps its spaces
  [ -n "$d" ] || d=/data
  # Docker starts a check as root, past this script: drop to the server's user,
  # whose access to config.yaml root may not have (an NFS share with root_squash).
  set --
  if [ "$(id -u)" = 0 ]; then set -- su-exec "${PUID:-1000}:${PGID:-1000}"; fi
  exec "$@" audiosilo --data "$d" healthcheck
fi

# Records the data folder $1, made absolute, in $DATA_RECORD: removed first and
# made anew with noclobber (O_EXCL), so a link left at its name is never written
# through. One that can't be written (a read-only /tmp) leaves the check on /data.
record_data_dir() {
  case "$1" in /*) d="$1" ;; *) d="$(pwd)" && d="${d%/}/$1" ;; esac
  rm -f "$DATA_RECORD" 2>/dev/null || true # one left in place (a folder) fails the O_EXCL create below
  (set -C && printf '%s' "$d" >"$DATA_RECORD") 2>/dev/null || true
}

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
  # A timeout is not proof that the path is safe. Only a definite "different"
  # (including a folder that does not yet exist) allows preparation.
  if timeout "$WAIT" test "$1" -ef /; then return 1; else [ "$?" = 1 ]; fi
}

# Prune real nested mounts by name. An unmounted btrfs subvolume in /data is
# part of the data tree even though it has its own device number. A writable
# mounted regular file (config.yaml) is the only mount we change ownership on.
chown_data() {
  # Compare the mount table's physical paths even for --data /alias or /data/.
  # The marker keeps trailing newlines in the name through command substitution.
  [ ! -L "$DATA" ] || return 0
  physical=$(CDPATH='' cd -P "$DATA" && printf '%s.' "$PWD") || return 1
  physical=${physical%.}
  [ "$physical" != / ] || return 1
  # Keep the table escaped until each record has been read. If it can't be
  # read, don't walk a tree whose boundaries we can't establish.
  points=$(awk '{ for (i = 7; i <= NF; i++) if ($i == "-") {
    print (($6 ~ /(^|,)ro(,|$)/ || $(i+3) ~ /(^|,)ro(,|$)/) ? "ro" : "rw"), $5; break
  } }' /proc/self/mountinfo) || return 1
  [ -n "$points" ] || return 1
  set -- "$physical" -mindepth 1
  while read -r mode encoded; do
    # POSIX/BusyBox printf %b takes exactly the mountinfo octal after an
    # extra zero. Decode once, so a literal backslash followed by digits stays literal.
    case "$encoded" in
      *\\*)
        point=$(printf '%b.' "$(printf '%s' "$encoded" | sed 's/\\/\\0/g')") || return 1
        point=${point%.}
        ;;
      *) point=$encoded ;;
    esac
    case "$point" in "$physical"/*) ;; *) continue ;; esac
    glob=$(printf '%s.' "$point" | sed 's/[][*?\\]/\\&/g')
    set -- "$@" -path "${glob%.}"
    if [ "$mode" != ro ]; then set -- "$@" ! -type f; fi
    set -- "$@" -prune -o
  done <<EOF
$points
EOF
  find "$@" \( ! -user "$PUID" -o ! -group "$PGID" \) -exec chown -h "$RUN_AS" {} +
}

# The status passed to checkdirs is about this folder alone. A failed chown
# of a child (such as a mounted config file) says nothing about its parent.
chown_folder() {
  find "$1" -maxdepth 0 \( ! -user "$PUID" -o ! -group "$PGID" \) -exec chown -h "$RUN_AS" {} +
}

check_folders() {
  set -- audiosilo --data "$DATA" checkdirs
  if [ -n "$DATA_CHOWN_FAILED" ]; then set -- "$@" --data-chown-failed; fi
  if [ -n "$BACKUPS_CHOWN_FAILED" ]; then set -- "$@" --backups-chown-failed; fi
  as_user "$@"
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
record_data_dir "$DATA"
BACKUPS=$(trim "${AUDIOSILO_BACKUP_DIR:-}")
if ! usable_path "$BACKUPS"; then BACKUPS=""; fi
DATA_CHOWN_FAILED=""
BACKUPS_CHOWN_FAILED=""

if [ "$(id -u)" = "0" ]; then
  PUID="${PUID:-1000}"
  PGID="${PGID:-1000}"
  RUN_AS="$PUID:$PGID"
  as_user() { su-exec "$RUN_AS" "$@"; }
  if usable_path "$DATA"; then
    chown_folder "$DATA" 2>/dev/null || DATA_CHOWN_FAILED=1
    chown_data 2>/dev/null || true
  fi
  if [ -n "$BACKUPS" ]; then
    # The folder and AudioSilo's own backup files in it, never the rest (it may be
    # a share other apps keep their backups in too) and never what a link in it
    # points at: files only, and chown -h. Each step bounded, as the checks are:
    # the share's server may be gone, and the server starts without its backups.
    timeout "$WAIT" mkdir -p "$BACKUPS" 2>/dev/null || true
    if timeout "$WAIT" chown "$RUN_AS" "$BACKUPS" 2>/dev/null; then
      timeout "$WAIT" find "$BACKUPS/" -maxdepth 1 -type f \( -name 'audiosilo-*.db' -o -name '.audiosilo-*' \) \
        -exec chown -h "$RUN_AS" {} + 2>/dev/null || true
    else
      BACKUPS_CHOWN_FAILED=1
    fi
  fi
  check_folders
  reap_watchers
  exec su-exec "$RUN_AS" "$@"
fi

as_user() { "$@"; }
export AUDIOSILO_STARTED_AS_USER=1
check_folders
reap_watchers
exec "$@"
