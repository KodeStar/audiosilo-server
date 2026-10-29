#!/usr/bin/env bash
# Regenerates the in-process probe's fixture corpus, internal/metadata/testdata/probe
# (server#261): a few seconds of audio in every shape the probe reads or leaves to
# ffprobe, then golden-<release>.json, an ffprobe release's answer for each
# (cmd/probediff -golden), for the local ffprobe and the Docker image's. The golden
# test reads them with no ffprobe installed; probe_live_test.go compares the
# installed ffprobe with the probe over the same files (and CI's probe-drift job
# with the image's ffprobe).
#
# Needs ffmpeg/ffprobe (7.x or 8.x, with libmp3lame, libvorbis and libopus), the
# lame CLI and metaflac (flac). Run from anywhere: scripts/gen-probe-fixtures.sh
# Regenerating rewrites every fixture (encoders differ by version), so commit the
# files and the goldens together, and review the goldens' diff.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=internal/metadata/testdata/probe
for tool in ffmpeg ffprobe lame metaflac go; do
  command -v "$tool" >/dev/null || { echo "gen-probe-fixtures: $tool is required" >&2; exit 1; }
done
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
rm -rf "$OUT"
mkdir -p "$OUT"
FF=(ffmpeg -hide_banner -loglevel error -y)
D=5.3 # seconds of audio (the chapters below end there)

# Sources: stereo 44.1 kHz (a tone under noise, so VBR encoders vary their frames), a
# quieter one for the lossless encoders (smaller files), and mono 22.05 kHz (MPEG-2).
"${FF[@]}" -f lavfi -i "sine=f=440:d=$D,aformat=channel_layouts=stereo" -f lavfi -i "anoisesrc=d=$D:a=0.05" \
  -filter_complex amix=inputs=2 -ar 44100 -c:a pcm_s16le "$TMP/src.wav"
"${FF[@]}" -f lavfi -i "sine=f=440:d=$D,aformat=channel_layouts=stereo" -ar 22050 -c:a pcm_s16le "$TMP/src-lossless.wav"
"${FF[@]}" -f lavfi -i "sine=f=300:d=4.1" -ar 22050 -ac 1 -c:a pcm_s16le "$TMP/src-mono22.wav"

cat >"$TMP/meta.txt" <<'M'
;FFMETADATA1
title=Chapter Title Track
album=The Spike Book (Unabridged)
artist=Jane Author
album_artist=Jane Author
composer=Nick Narrator
genre=Fantasy; Audiobook
date=2019-04-02
series=The Spike Series
series-part=3
narrator=Nora Reader
grouping=Grouped Series #2
comment=Some comment

[CHAPTER]
TIMEBASE=1/1000
START=0
END=2000
title=Opening

[CHAPTER]
TIMEBASE=1/1000
START=2000
END=4123
title=Middle part é

[CHAPTER]
TIMEBASE=1/1000
START=4123
END=5300
title=The End
M
cat >"$TMP/meta-nochap.txt" <<'M'
;FFMETADATA1
title=Plain Title
album=Plain Album
artist=Plain Artist
genre=Mystery
date=2021
M

# enc NAME EXT SOURCE CODEC-ARGS...: NAME-chap (tags + chapters), NAME-tags, NAME-bare.
enc() {
  local name=$1 ext=$2 src=$3
  shift 3
  "${FF[@]}" -i "$src" -i "$TMP/meta.txt" -map 0:a -map_metadata 1 -map_chapters 1 "$@" "$OUT/$name-chap.$ext"
  "${FF[@]}" -i "$src" -i "$TMP/meta-nochap.txt" -map 0:a -map_metadata 1 -map_chapters -1 "$@" "$OUT/$name-tags.$ext"
  "${FF[@]}" -i "$src" -map 0:a -map_metadata -1 -map_chapters -1 "$@" "$OUT/$name-bare.$ext"
}
S=$TMP/src.wav
L=$TMP/src-lossless.wav
enc flac flac "$L" -c:a flac
enc vorbis ogg "$S" -c:a libvorbis -q:a 0
enc opus opus "$S" -c:a libopus -b:a 24k
enc oggopus ogg "$S" -c:a libopus -b:a 16k
enc oggflac ogg "$L" -c:a flac
enc mka-mp3 mka "$S" -c:a libmp3lame -b:a 32k
enc mka-aac mka "$S" -c:a aac -b:a 32k
enc mka-opus mka "$S" -c:a libopus -b:a 24k
enc mka-flac mka "$L" -c:a flac
enc mka-vorbis mka "$S" -c:a libvorbis -q:a 0
enc m4b-aac m4b "$S" -c:a aac -b:a 32k -f mp4
enc m4a-alac m4a "$L" -c:a alac -f ipod
enc mp3-v24 mp3 "$S" -c:a libmp3lame -q:a 7 -id3v2_version 4
enc mp3-v23 mp3 "$S" -c:a libmp3lame -b:a 32k -id3v2_version 3
enc mp3-cbr mp3 "$S" -c:a libmp3lame -b:a 48k -write_xing 0
enc mp3-vbrnoxing mp3 "$S" -c:a libmp3lame -q:a 6 -write_xing 0
enc mp3-v1 mp3 "$S" -c:a libmp3lame -b:a 32k -id3v2_version 0 -write_id3v1 1
enc mp4-mp3 m4a "$S" -c:a libmp3lame -b:a 32k -f mp4
# Mono MPEG-2 (lsf) MP3, with and without the Xing frame.
"${FF[@]}" -i "$TMP/src-mono22.wav" -i "$TMP/meta.txt" -map 0:a -map_metadata 1 -map_chapters 1 -c:a libmp3lame -q:a 7 "$OUT/lsf-vbr.mp3"
"${FF[@]}" -i "$TMP/src-mono22.wav" -c:a libmp3lame -b:a 16k -write_xing 0 "$OUT/lsf-cbr-noxing.mp3"
# lame's own files (MPEG-1, 44.1 kHz, as lame writes a book at a higher bitrate): VBR/CBR
# with and without the Xing/LAME frame, ABR, ID3 from lame.
lame --silent --resample 44.1 -V 7 "$S" "$OUT/lame-vbr.mp3"
lame --silent --resample 44.1 -V 7 -t "$S" "$OUT/lame-vbr-noxing.mp3"
lame --silent --resample 44.1 -b 48 "$S" "$OUT/lame-cbr-info.mp3"
lame --silent --resample 44.1 -b 48 -t "$S" "$OUT/lame-cbr-noxing.mp3"
lame --silent --resample 44.1 --abr 40 "$S" "$OUT/lame-abr.mp3"
lame --silent --resample 44.1 -V 8 --tt "Lame Title" --ta "Lame Artist" --tl "Lame Album" --ty 2001 --tg Speech --add-id3v2 "$S" "$OUT/lame-tags.mp3"
lame --silent --resample 44.1 -b 32 --tt "V1 Only Title" --ta "V1 Artist" --tl "V1 Album" --ty 1999 --id3v1-only "$S" "$OUT/lame-v1only.mp3"
# FLAC: Vorbis-comment chapters (CHAPTERxxx, one with no time, a second ARTIST), and a
# CUESHEET (which the probe leaves to ffprobe).
cp "$OUT/flac-tags.flac" "$OUT/craft-flac-chapters.flac"
metaflac --set-tag="ARTIST=Second Artist" --set-tag=CHAPTER001=00:00:00.000 --set-tag=CHAPTER001NAME=Intro \
  --set-tag=CHAPTER002=00:00:02.500 --set-tag=CHAPTER002NAME=Middle --set-tag=CHAPTER003=00:00:04.250 \
  --set-tag="CHAPTER004NAME=Orphan name" --set-tag="SERIES=Flac Series" --set-tag="NARRATOR=Flac Narrator" \
  "$OUT/craft-flac-chapters.flac"
cp "$OUT/flac-tags.flac" "$OUT/craft-flac-cuesheet.flac"
cat >"$TMP/cue.cue" <<'C'
FILE "craft-flac-cuesheet.flac" WAVE
  TRACK 01 AUDIO
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    INDEX 01 00:02:00
  TRACK 03 AUDIO
    INDEX 01 00:04:00
C
metaflac --no-utf8-convert --import-cuesheet-from="$TMP/cue.cue" "$OUT/craft-flac-cuesheet.flac"
# The hand-crafted shapes (ID3 variants, M4B chapter variants, Matroska), built from the
# files above.
go run ./cmd/mkfixtures "$OUT"
# The goldens: the local ffprobe's release, and the Docker image's (its Alpine base, read
# from the Dockerfile) when docker is here; SKIP_DOCKER=1 skips that.
go run ./cmd/probediff -golden "$OUT"
if [ -z "${SKIP_DOCKER:-}" ] && command -v docker >/dev/null; then
  base=$(awk '/^FROM alpine:/ {print $2}' Dockerfile | tail -1)
  arch=$(docker version --format '{{.Server.Arch}}')
  GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -o "$TMP/probediff" ./cmd/probediff
  docker run --rm -v "$TMP:/tool:ro" -v "$PWD/$OUT:/fx" "$base" \
    sh -c 'apk add --no-cache ffmpeg >/dev/null && /tool/probediff -golden /fx'
fi
du -sh "$OUT"
