#!/usr/bin/env bash
#
# Seed a demo audiobook library with public-domain LibriVox recordings hosted on
# the Internet Archive. Intended for a public demo instance (see demo mode in
# config.yaml: `demo.enabled`). It writes an `<Author>/<Title>/NN - <file>.mp3`
# (+ `cover.jpg`) tree — the standard folder-per-book convention the scanner
# auto-detects on startup (each `<Title>` directory holds one book's audio, and
# multiple titles group under a shared author folder).
#
# Folder/title/author and the chapter file list are resolved live from the
# archive.org metadata API, so you only curate item identifiers (below). Re-runs
# are idempotent (existing files are skipped) and a failed item is skipped, not
# fatal.
#
# Requires: bash, curl, python3.
#
# Usage:
#   scripts/seed-librivox.sh [DEST] [identifier ...]
#     DEST          target library root (default ./demo-library or $DEST)
#     identifier... archive.org item ids to seed (default: the curated list below)
#
# Examples:
#   scripts/seed-librivox.sh /data/demo-library
#   DRY_RUN=1 scripts/seed-librivox.sh           # list what would be downloaded
#   scripts/seed-librivox.sh ./demo-library pride_and_prejudice_0711_librivox
#   MAX_FILES=3 scripts/seed-librivox.sh ./lib   # cap chapter files per book
#                                                # (small/fast seeds for screenshots)
set -euo pipefail

DEST_DEFAULT="${DEST:-./demo-library}"
DEST="$DEST_DEFAULT"
DRY_RUN="${DRY_RUN:-0}"
MAX_FILES="${MAX_FILES:-0}"   # >0 = download at most this many chapter files per book

# Curated archive.org identifiers (LibriVox public-domain audiobooks). Override by
# passing identifiers as arguments after DEST. Verify ids with DRY_RUN=1.
# Grouped by author so the `<Author>/<Title>/` tree shows 2–3 books per author
# (where LibriVox has them). Books group under one author folder only when their
# archive.org `creator` strings match exactly, so keep an author's items
# consistent (e.g. all "Sir Arthur Conan Doyle", not a mix with "Arthur Conan
# Doyle"). Sun Tzu stays single — LibriVox has no other Sun Tzu work.
DEFAULT_IDS=(
  # Jane Austen
  solo_pride_librivox                     # Pride and Prejudice (version 2)
  persuasion_0905_librivox                # Persuasion
  northanger_abbey_librivox               # Northanger Abbey
  # Sir Arthur Conan Doyle
  adventures_sherlock_holmes_rg_librivox  # The Adventures of Sherlock Holmes (version 2)
  hound_baskervilles_librivox             # The Hound of the Baskervilles
  memoirs_holmes_0709_librivox            # The Memoirs of Sherlock Holmes
  # Lewis Carroll
  alice_in_wonderland_librivox            # Alice's Adventures in Wonderland
  looking-glass_librivox                  # Through the Looking-Glass
  # Jack London
  callofthewild_tc_1010_librivox          # The Call of the Wild
  white_fang_librivox                     # White Fang
  scarlet_plague_0907_librivox            # The Scarlet Plague
  # Charles Dickens
  christmas_carol_1111_librivox           # A Christmas Carol
  tale_two_cities_librivox                # A Tale of Two Cities
  oliver_twist_librivox                   # Oliver Twist
  # Sun Tzu
  art_of_war_librivox                     # The Art of War
)

# First non-flag arg overrides DEST; remaining args override the identifier list.
IDS=()
if [ "$#" -gt 0 ]; then
  DEST="$1"
  shift
  IDS=("$@")
fi
if [ "${#IDS[@]}" -eq 0 ]; then
  IDS=("${DEFAULT_IDS[@]}")
fi

for bin in curl python3; do
  command -v "$bin" >/dev/null 2>&1 || { echo "error: $bin is required" >&2; exit 1; }
done

mkdir -p "$DEST"
echo "Seeding ${#IDS[@]} LibriVox item(s) into: $DEST"
[ "$DRY_RUN" = "1" ] && echo "(dry run — no files will be downloaded)"

# Parses an archive.org metadata JSON document (stdin). Prints the destination
# folder name on line 1, then one chapter file name per subsequent line, choosing
# a single mp3 derivation (preferring higher bitrate) ordered by track number.
PARSE_PY='
import sys, json, re
data = json.load(sys.stdin)
meta = data.get("metadata", {})
title = meta.get("title") or sys.argv[1]
creator = meta.get("creator") or "LibriVox"
if isinstance(creator, list):
    creator = creator[0] if creator else "LibriVox"
def slug(s):
    s = re.sub(r"[\\/:*?\"<>|]+", " ", str(s)).strip()
    # Collapse whitespace and strip leading/trailing dots/spaces so a metadata
    # value can never become "." / ".." and escape the library root once it is
    # used as a path segment.
    return re.sub(r"\s+", " ", s).strip(" .")[:120].strip(" .")
author = slug(creator) or "LibriVox"
book = slug(title) or sys.argv[1]
# "<Author>/<Title>" — the scanner treats each title directory (which holds the
# audio) as one book and groups titles by the same author under one folder.
print(author + "/" + book)
files = data.get("files", [])
chosen = []
for fmt in ("128Kbps MP3", "64Kbps MP3", "VBR MP3"):
    chosen = [f for f in files if f.get("format") == fmt]
    if chosen:
        break
if not chosen:
    chosen = [f for f in files if f.get("name", "").lower().endswith(".mp3")]
def track_key(f):
    t = str(f.get("track", "")).split("/")[0]
    return (0, int(t)) if t.isdigit() else (1, f.get("name", ""))
chosen.sort(key=track_key)
for f in chosen:
    print(f["name"])
'

seed_one() {
  local id="$1" meta tmp folder out idx i name target
  echo "==> $id"
  if ! meta="$(curl -fsSL "https://archive.org/metadata/$id")" || [ -z "$meta" ]; then
    echo "    skip: could not fetch metadata"
    return 0
  fi
  tmp="$(mktemp)"
  if ! printf '%s' "$meta" | python3 -c "$PARSE_PY" "$id" > "$tmp"; then
    echo "    skip: could not parse metadata"
    rm -f "$tmp"
    return 0
  fi
  folder="$(head -n1 "$tmp")"
  if [ -z "$folder" ]; then
    echo "    skip: empty folder name"
    rm -f "$tmp"
    return 0
  fi
  out="$DEST/$folder"
  i=0
  while IFS= read -r name; do
    [ -z "$name" ] && continue
    if [ "$MAX_FILES" -gt 0 ] && [ "$i" -ge "$MAX_FILES" ]; then
      break
    fi
    # Defend against archive.org file names that carry path segments: keep only
    # the basename and reject traversal so a download can't escape the book folder.
    name="${name##*/}"
    case "$name" in ""|.|..) echo "    skip: unsafe file name"; continue ;; esac
    i=$((i + 1))
    printf -v idx '%03d' "$i"
    target="$out/$idx - $name"
    if [ "$DRY_RUN" = "1" ]; then
      echo "    would fetch: $folder/$idx - $name"
      continue
    fi
    mkdir -p "$out"
    if [ -f "$target" ]; then
      echo "    exists: $idx - $name"
      continue
    fi
    echo "    fetch: $idx - $name"
    curl -fSL --retry 3 -o "$target" "https://archive.org/download/$id/$name"
  done < <(tail -n +2 "$tmp")
  rm -f "$tmp"

  if [ "$i" -eq 0 ]; then
    echo "    warning: no mp3 chapters found"
  fi
  if [ "$DRY_RUN" != "1" ] && [ "$i" -gt 0 ] && [ ! -f "$out/cover.jpg" ]; then
    curl -fSL --retry 3 -o "$out/cover.jpg" "https://archive.org/services/img/$id" \
      || echo "    (no cover)"
  fi
}

for id in "${IDS[@]}"; do
  seed_one "$id"
done

echo
echo "Done. Point a library at this directory and set demo.library to its name;"
echo "the scanner auto-detects each <Author>/<Title>/ folder as one book. e.g. in"
echo "config.yaml:"
echo "  libraries:"
echo "    - { name: \"Demo\", root: \"$DEST\" }"
echo "  demo: { enabled: true, library: \"Demo\" }"
