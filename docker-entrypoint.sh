#!/bin/sh
# Run the server as PUID:PGID (default 1000:1000) so SQLite can write the data dir
# no matter how the host-mounted volume is owned - the usual NAS pain point. The
# container starts as root only to chown /data (and AUDIOSILO_BACKUP_DIR's folder
# and the backups in it, when it is set) and drop privileges; the server itself
# never runs as root. The chown of /data stops at a folder mounted inside it (a
# backups share at /data/backups keeps its owner, server#382) and at any other
# mount or filesystem; a file mounted inside it (a config.yaml) is chowned. On
# NAS setups set PUID/PGID to the owner of your appdata (Unraid: PUID=99
# PGID=100; Synology: often 1026:100; or run `id` for your user).
#
# A chown can fail without saying so (a NAS share with ACLs, a read-only mount), so
# the entrypoint runs audiosilo checkdirs as the server user. It checks for write
# access to the data and backups folders and read access to mounted content.
# When a check fails it prints the path, the user, the folder's owner and the fix
# (server#368). It warns and carries on: the server's own start-up error says the
# same about the data folder. The checks share one time limit
# (fsaccess.DirCheckTimeout): a share whose server is gone is said not to answer,
# and never holds the start. The root-only ownership steps on the data folder
# itself and on the backups folder are each bounded by $WAIT seconds for the
# same reason; the walk of the data folder's contents is not (a large folder may
# need longer), and is skipped when the data folder itself didn't answer.
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

# What is under the data folder goes to the server's user (audiosilo chowndata,
# fsaccess.ChownTree): never through a link, never into a folder mounted inside
# it, a mount the walk itself would set off (an NFS export's child filesystem,
# an automount, a NAS's snapshot folder) or another filesystem, while a btrfs
# subvolume of the data folder is walked. A writable mounted regular file
# (config.yaml) is the only mount whose owner it changes. It works from open
# folders, so the server's user, who can write in the data folder, cannot
# swap in a link for root to follow.
chown_data() {
  audiosilo --data "$DATA" chowndata "$RUN_AS"
}

# The status passed to checkdirs is about this folder alone. A failed chown
# of a child (such as a mounted config file) says nothing about its parent.
# Bounded: 124 or 143 (timeout's) is a folder that didn't answer.
chown_folder() {
  timeout "$WAIT" find "$1" -maxdepth 0 \( ! -user "$PUID" -o ! -group "$PGID" \) -exec chown -h "$RUN_AS" {} +
}

# Whether folder $1 is reached through no link, its own name included: root's
# mkdir and chown resolve every name on the way, and the server's user could
# leave a link on it (in /data) to a system folder for root to hand over at the
# next start. Bounded, as a dead share's cd would block.
physical() {
  # shellcheck disable=SC2016 # the inner shell expands $1
  [ "$(timeout "$WAIT" sh -c 'cd -P "$1" && pwd -P' sh "$1" 2>/dev/null)" = "$1" ]
}

# Runs checkdirs behind the command prefix in "$@" (su-exec as the server user,
# or nothing when the container already runs as it). Advisory only: under set -e
# a check that could not run (a crash, a killed process) would otherwise end
# this script before the server starts.
check_folders() {
  set -- "$@" audiosilo --data "$DATA" checkdirs
  if [ -n "$DATA_CHOWN_FAILED" ]; then set -- "$@" --data-chown-failed; fi
  if [ -n "$BACKUPS_CHOWN_FAILED" ]; then set -- "$@" --backups-chown-failed; fi
  "$@" || true
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

WAIT=5 # seconds each root-only ownership step may take
DATA=$(trim "$(data_dir "$@")")
record_data_dir "$DATA"
DATA_CHOWN_FAILED=""
BACKUPS_CHOWN_FAILED=""

if [ "$(id -u)" = "0" ]; then
  PUID="${PUID:-1000}"
  PGID="${PGID:-1000}"
  RUN_AS="$PUID:$PGID"
  # Only an AUDIOSILO_BACKUP_DIR folder gets the ownership pass; checkdirs
  # resolves the backups folder (env, config.yaml or the default) itself.
  BACKUPS=$(trim "${AUDIOSILO_BACKUP_DIR:-}")
  if ! usable_path "$BACKUPS"; then BACKUPS=""; fi
  if usable_path "$DATA"; then
    rc=0
    chown_folder "$DATA" 2>/dev/null || rc=$?
    if [ "$rc" != 0 ]; then DATA_CHOWN_FAILED=1; fi
    # A data folder that didn't answer in time isn't walked: the walk would hold
    # the start on it.
    case "$rc" in 124 | 143) ;; *) chown_data 2>/dev/null || true ;; esac
  fi
  if [ -n "$BACKUPS" ]; then
    # The folder and AudioSilo's own backup files in it, never the rest (it may be
    # a share other apps keep their backups in too) and never what a link in it
    # points at: files only, and chown -h. The folder itself is changed only
    # when no name on its path is a link (physical), and with chown -h: in a
    # folder the server's user can write (/data/backups) it could leave a link
    # there to a system folder for root to hand it at the next start. Each step
    # bounded, as the checks are: the share's server may be gone, and the server
    # starts without its backups.
    timeout "$WAIT" mkdir -p "$BACKUPS" 2>/dev/null || true
    if physical "$BACKUPS" && timeout "$WAIT" chown -h "$RUN_AS" "$BACKUPS" 2>/dev/null; then
      timeout "$WAIT" find "$BACKUPS" -maxdepth 1 -type f \( -name 'audiosilo-*.db' -o -name '.audiosilo-*' \) \
        -exec chown -h "$RUN_AS" {} + 2>/dev/null || true
    else
      BACKUPS_CHOWN_FAILED=1
    fi
  fi
  check_folders su-exec "$RUN_AS"
  reap_watchers
  exec su-exec "$RUN_AS" "$@"
fi

export AUDIOSILO_STARTED_AS_USER=1
check_folders
reap_watchers
exec "$@"
