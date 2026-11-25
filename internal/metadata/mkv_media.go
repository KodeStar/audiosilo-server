package metadata

import (
	"context"
	"iter"
	"net/http"
	"os"
	"path"
	"strings"
)

const (
	mkvAttachments  = 0x1941A469
	mkvAttachedFile = 0x61A7
	mkvFileName     = 0x466E
	mkvFileData     = 0x465C
)

// CoverMIME returns the image type identified by the bytes of an embedded
// picture. An attachment's declared type alone cannot select what we serve.
func CoverMIME(data []byte) (string, bool) {
	mime := http.DetectContentType(data)
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp":
		return mime, true
	}
	return "", false
}

// MatroskaAudioCodec reads the first audio track from the opened file. An
// unknown or incomplete header leaves the decision to the remuxer.
func MatroskaAudioCodec(ctx context.Context, f *os.File, size int64) (string, error) {
	b := newNamedBlockFile(f, size)
	s := newMKVState(b)
	why := b.boundedBy(ctx, 2<<20, func() {
		_ = s.headers(func(c ebmlElem, pos int64) bool {
			if c.id != mkvTracks {
				return false
			}
			s.level1(c, pos)
			return true
		})
	})
	// A walk that ended on its own read everything it needed: its answer
	// stands even when ctx ends just after.
	if why == "" && b.err == nil {
		return s.codec, nil
	}
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	return "", b.err
}

// MatroskaCover reads one image attachment from an opened library file.
func MatroskaCover(ctx context.Context, f *os.File, size int64) ([]byte, error) {
	b := newNamedBlockFile(f, size)
	var data []byte
	why := b.bounded(ctx, func() {
		pic := mkvPicture(b)
		if pic.size > 0 {
			data, _ = b.bytesAt(pic.data, int(pic.size))
		}
	})
	if why == "" && b.err == nil {
		return data, nil
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	// A read that failed is an error; a bound passed (a hostile layout) is no
	// cover.
	return nil, b.err
}

// mkvPicture finds the full-size cover by name, or the first image when the
// muxer used another name. A scan reads only the signature of the picture.
func mkvPicture(b *blockFile) (pic ebmlElem) {
	s := newMKVState(b)
	_ = s.headers(func(c ebmlElem, pos int64) bool {
		if c.id != mkvAttachments {
			return false
		}
		// Walked once: the SeekHead's entry for the same element is skipped.
		s.parsed[pos] = true
		for file := range mkvFields(b, c) {
			if file.id != mkvAttachedFile {
				continue
			}
			var data ebmlElem
			var name string
			for field := range mkvFields(b, file) {
				switch field.id {
				case mkvFileName:
					if field.size <= 512 {
						p, _ := b.bytesAt(field.data, int(field.size))
						name = string(p)
					}
				case mkvFileData:
					data = field
				}
			}
			if data.size <= 0 || data.size > 16<<20 {
				continue
			}
			prefix, ok := b.bytesAt(data.data, int(min(512, data.size)))
			if _, image := CoverMIME(prefix); !ok || !image {
				continue
			}
			if strings.EqualFold(strings.TrimSuffix(name, path.Ext(name)), "cover") {
				pic = data
				return true
			}
			if pic.size == 0 {
				pic = data
			}
		}
		return pic.size > 0
	})
	return pic
}

// mkvFields walks a master's headers without buffering its attachment bodies.
func mkvFields(b *blockFile, parent ebmlElem) iter.Seq[ebmlElem] {
	return func(yield func(ebmlElem) bool) {
		end := parent.data + parent.size
		for pos, n := parent.data, 0; pos < end && n < 4096; n++ {
			field, ok := elemAt(b, pos)
			if !ok || field.size < 0 || field.size > end-field.data || !yield(field) {
				return
			}
			pos = field.data + field.size
		}
	}
}
