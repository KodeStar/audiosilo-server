#!/usr/bin/env bash
#
# Seed a demo audiobook library with public-domain LibriVox recordings hosted on
# the Internet Archive. Intended for a public demo instance (see demo mode in
# config.yaml: `demo.enabled`). It writes a `books_in_folder` layout — one folder
# per book (`<Author> - <Title>/NN - <file>.mp3` + `cover.jpg`) — which the
# scanner indexes on startup.
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
set -euo pipefail

DEST_DEFAULT="${DEST:-./demo-library}"
DEST="$DEST_DEFAULT"
DRY_RUN="${DRY_RUN:-0}"

# Curated archive.org identifiers (LibriVox public-domain audiobooks). Override by
# passing identifiers as arguments after DEST. Verify ids with DRY_RUN=1.
DEFAULT_IDS=(
  solo_pride_librivox                     # Pride and Prejudice — Jane Austen
  adventures_sherlock_holmes_rg_librivox  # The Adventures of Sherlock Holmes — Conan Doyle
  alice_in_wonderland_librivox            # Alice's Adventures in Wonderland — Lewis Carroll
  callofthewild_tc_1010_librivox          # The Call of the Wild — Jack London
  christmas_carol_1111_librivox           # A Christmas Carol — Charles Dickens
  art_of_war_librivox                     # The Art of War — Sun Tzu
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
    return re.sub(r"\s+", " ", s)[:120]
print(slug(creator) + " - " + slug(title))
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
echo "Done. Point a library at this directory (layout: books_in_folder) and set"
echo "demo.library to its name, e.g. in config.yaml:"
echo "  libraries:"
echo "    - { name: \"Demo\", root: \"$DEST\", layout: \"books_in_folder\" }"
echo "  demo: { enabled: true, library: \"Demo\" }"
