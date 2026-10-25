package metadata

import (
	"encoding/binary"
	"errors"
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

var errMP4Box = errors.New("malformed mp4 box")

// readMP4Narrator returns the \xa9nrt item of the MP4 r holds, trimmed; "" when
// there is none, it is not text, or the file is not an MP4 or is malformed.
func readMP4Narrator(r io.ReadSeeker) string {
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return ""
	}
	moov, moovEnd, ok := findMP4Box(r, 0, end, "moov")
	if !ok {
		return ""
	}
	meta, metaEnd, ok := findMP4Box(r, moov, moovEnd, "udta")
	if ok {
		meta, metaEnd, ok = findMP4Box(r, meta, metaEnd, "meta")
	}
	if !ok {
		if meta, metaEnd, ok = findMP4Box(r, moov, moovEnd, "meta"); !ok {
			return ""
		}
	}
	ilst, ilstEnd, ok := findMP4Box(r, metaChildren(r, meta, metaEnd), metaEnd, "ilst")
	if !ok {
		return ""
	}
	item, itemEnd, ok := findMP4Box(r, ilst, ilstEnd, "\xa9nrt")
	if !ok {
		return ""
	}
	data, dataEnd, ok := findMP4Box(r, item, itemEnd, "data")
	if !ok || dataEnd-data < 8 || dataEnd-data-8 > maxNarratorData {
		return ""
	}
	buf := make([]byte, dataEnd-data)
	if _, err := r.Seek(data, io.SeekStart); err != nil {
		return ""
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		return ""
	}
	// Four bytes of version and type (the low 24 bits), four of locale, the value.
	if binary.BigEndian.Uint32(buf[:4])&0xFFFFFF != mp4UTF8 {
		return ""
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(buf[8:]), ""))
}

// metaChildren is where a meta box's children start: after its four bytes of
// version and flags (ISO's full box, as iTunes writes it), or straight away in
// the QuickTime form, whose first child (hdlr) follows the header.
func metaChildren(r io.ReadSeeker, body, end int64) int64 {
	var h [8]byte
	if end-body < 8 {
		return body
	}
	if _, err := r.Seek(body, io.SeekStart); err != nil {
		return body
	}
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return body
	}
	if string(h[4:8]) == "hdlr" {
		return body
	}
	return body + 4
}

// findMP4Box finds the first box named want among the boxes between start and
// end (one container's children), returning its body's start and its end.
func findMP4Box(r io.ReadSeeker, start, end int64, want string) (body, boxEnd int64, ok bool) {
	at := start
	for range maxMP4Boxes {
		if at >= end {
			return 0, 0, false
		}
		name, body, next, err := nextMP4Box(r, at, end)
		if err != nil {
			return 0, 0, false
		}
		if name == want {
			return body, next, true
		}
		at = next
	}
	return 0, 0, false
}

// nextMP4Box reads the box header at at, inside a container ending at end: its
// name, where its body starts and where the box ends. A size of 1 is the 64-bit
// size that follows the name, 0 runs to the container's end; a box shorter than
// its header or longer than what is left of its container is errMP4Box.
func nextMP4Box(r io.ReadSeeker, at, end int64) (name string, body, next int64, err error) {
	var h [16]byte
	if end-at < 8 {
		return "", 0, 0, errMP4Box
	}
	if _, err := r.Seek(at, io.SeekStart); err != nil {
		return "", 0, 0, err
	}
	if _, err := io.ReadFull(r, h[:8]); err != nil {
		return "", 0, 0, err
	}
	size := int64(binary.BigEndian.Uint32(h[:4]))
	name, body = string(h[4:8]), at+8
	switch size {
	case 0:
		size = end - at
	case 1:
		if end-at < 16 {
			return "", 0, 0, errMP4Box
		}
		if _, err := io.ReadFull(r, h[8:]); err != nil {
			return "", 0, 0, err
		}
		large := binary.BigEndian.Uint64(h[8:])
		if large > math.MaxInt64 {
			return "", 0, 0, errMP4Box
		}
		size, body = int64(large), at+16
	}
	if size < body-at || size > end-at {
		return "", 0, 0, errMP4Box
	}
	return name, body, at + size, nil
}
