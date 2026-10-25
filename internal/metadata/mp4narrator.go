package metadata

import (
	"encoding/binary"
	"io"
	"math"
	"strings"
)

// The iTunes narrator item (\xa9nrt, as Audible and most audiobook tools write
// it) is read here: dhowden/tag skips the atoms it has no name for and ffprobe
// does not map this one, so without its own walk no narrator an m4b's tags name
// reaches the index (#177). The walk is bounded: it follows only
// moov > udta > meta > ilst (or moov > meta > ilst), looks at a bounded number of
// boxes in each container, never reads a box larger than its parent, and reads
// at most maxNarratorData bytes of the value.

const (
	// maxMP4Boxes is how many sibling boxes one container's walk looks at.
	maxMP4Boxes = 4096
	// maxNarratorData caps the narrator value read (a name, or a few).
	maxNarratorData = 4096
	// mp4UTF8 is the iTunes data box's type for UTF-8 text.
	mp4UTF8 = 1
)

// readMP4Narrator returns the \xa9nrt item of the MP4 r holds (size bytes long),
// trimmed; "" when there is none, it is not text, or the file is not an MP4 or is
// malformed.
func readMP4Narrator(r io.ReaderAt, size int64) string {
	moov, moovEnd, ok := findMP4Box(r, 0, size, "moov")
	if !ok {
		return ""
	}
	meta, metaEnd, ok := findMP4Path(r, moov, moovEnd, "udta", "meta")
	if !ok {
		meta, metaEnd, ok = findMP4Box(r, moov, moovEnd, "meta")
	}
	if !ok {
		return ""
	}
	data, dataEnd, ok := findMP4Path(r, metaChildren(r, meta, metaEnd), metaEnd, "ilst", "\xa9nrt", "data")
	if !ok || dataEnd-data < 8 || dataEnd-data-8 > maxNarratorData {
		return ""
	}
	buf := make([]byte, dataEnd-data)
	if _, err := r.ReadAt(buf, data); err != nil {
		return ""
	}
	// Four bytes of version and type (the low 24 bits), four of locale, the value.
	if binary.BigEndian.Uint32(buf[:4])&0xFFFFFF != mp4UTF8 {
		return ""
	}
	return tagText(strings.ToValidUTF8(string(buf[8:]), ""))
}

// metaChildren is where a meta box's children start: after its four bytes of
// version and flags (ISO's full box, as iTunes writes it), or straight away in
// the QuickTime form, whose first child (hdlr) follows the header.
func metaChildren(r io.ReaderAt, body, end int64) int64 {
	var h [8]byte
	if end-body < 8 {
		return body
	}
	if _, err := r.ReadAt(h[:], body); err != nil || string(h[4:8]) == "hdlr" {
		return body
	}
	return body + 4
}

// findMP4Path finds the box at the end of names, each one among the children of
// the one before it (the first among the boxes between start and end), returning
// its body's start and its end.
func findMP4Path(r io.ReaderAt, start, end int64, names ...string) (body, boxEnd int64, ok bool) {
	body, boxEnd = start, end
	for _, name := range names {
		if body, boxEnd, ok = findMP4Box(r, body, boxEnd, name); !ok {
			return 0, 0, false
		}
	}
	return body, boxEnd, true
}

// findMP4Box finds the first box named want among the boxes between start and
// end (one container's children), returning its body's start and its end.
func findMP4Box(r io.ReaderAt, start, end int64, want string) (body, boxEnd int64, ok bool) {
	at := start
	for range maxMP4Boxes {
		name, childBody, next, read := nextMP4Box(r, at, end)
		if !read {
			return 0, 0, false
		}
		if name == want {
			return childBody, next, true
		}
		at = next
	}
	return 0, 0, false
}

// nextMP4Box reads the box header at at, inside a container ending at end: its
// name, where its body starts and where the box ends. A size of 1 is the 64-bit
// size that follows the name, 0 runs to the container's end; a box shorter than
// its header or longer than what is left of its container, or one that can't be
// read, is not ok.
func nextMP4Box(r io.ReaderAt, at, end int64) (name string, body, next int64, ok bool) {
	var h [16]byte
	if end-at < 8 {
		return "", 0, 0, false
	}
	if _, err := r.ReadAt(h[:8], at); err != nil {
		return "", 0, 0, false
	}
	size := int64(binary.BigEndian.Uint32(h[:4]))
	name, body = string(h[4:8]), at+8
	switch size {
	case 0:
		size = end - at
	case 1:
		if end-at < 16 {
			return "", 0, 0, false
		}
		if _, err := r.ReadAt(h[8:], at+8); err != nil {
			return "", 0, 0, false
		}
		large := binary.BigEndian.Uint64(h[8:])
		if large > math.MaxInt64 {
			return "", 0, 0, false
		}
		size, body = int64(large), at+16
	}
	if size < body-at || size > end-at {
		return "", 0, 0, false
	}
	return name, body, at + size, true
}
