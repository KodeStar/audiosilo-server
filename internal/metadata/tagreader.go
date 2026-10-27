package metadata

import (
	"errors"
	"io"
	"os"
)

// tagBufferSize is how much of an ID3v2 tag tagReader reads in one go.
const tagBufferSize = 64 << 10

// id3HeaderSize is an ID3v2 tag's header: "ID3", version, flags, size.
const id3HeaderSize = 10

// tagReader is the file dhowden/tag reads a file's tags from. It hands back the
// file's own bytes, exactly as reading the file would, but reads an ID3v2 tag
// (up to the end its header gives) through a buffer: dhowden reads an
// unsynchronised tag, which is how LibriVox and many other MP3s are written, a
// byte at a time, one read system call per byte (about 3,500 per MP3, and most
// of a scan's own CPU). Past the tag's end, and in any file that doesn't start
// with one, every read goes straight to the file, so nothing more is read than
// dhowden asks for there (an MP4's atoms are read header by header and skipped).
type tagReader struct {
	f   *os.File
	pos int64 // the reader's position

	limit    int64  // the ID3v2 tag's end: reads before it go through buf
	buf      []byte // the bytes at bufStart, read ahead
	bufStart int64
}

// newTagReader returns a tagReader over f, read from its start.
func newTagReader(f *os.File) *tagReader {
	r := &tagReader{f: f}
	hdr := make([]byte, id3HeaderSize)
	if n, _ := f.ReadAt(hdr, 0); n == id3HeaderSize && string(hdr[:3]) == "ID3" {
		// The size is four 7-bit bytes (syncsafe), the tag's length after its header.
		size := int64(hdr[6]&0x7f)<<21 | int64(hdr[7]&0x7f)<<14 | int64(hdr[8]&0x7f)<<7 | int64(hdr[9]&0x7f)
		r.limit = id3HeaderSize + size
		// The tag's first block, read now from the start: dhowden reads the file's
		// first bytes, seeks back to the start and reads the header again, all
		// from this one read. A failed read is tried again by the first Read.
		_ = r.fill()
	}
	return r
}

// Read reads from the reader's position, as (*os.File).Read would.
func (r *tagReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos < r.limit {
		if r.pos < r.bufStart || r.pos >= r.bufStart+int64(len(r.buf)) {
			if err := r.fill(); err != nil {
				return 0, err
			}
		}
		// Never past the tag's end: the bytes after it are read directly.
		n := copy(p[:min(int64(len(p)), r.limit-r.pos)], r.buf[r.pos-r.bufStart:])
		r.pos += int64(n)
		return n, nil
	}
	n, err := r.f.ReadAt(p, r.pos)
	r.pos += int64(n)
	if n > 0 && errors.Is(err, io.EOF) {
		err = nil // the next read reports the end, as a file's Read does
	}
	return n, err
}

// fill reads the buffer at the reader's position, up to the tag's end: an error
// only when it read nothing.
func (r *tagReader) fill() error {
	want := min(int64(tagBufferSize), r.limit-r.pos)
	if int64(cap(r.buf)) < want {
		r.buf = make([]byte, want)
	}
	n, err := r.f.ReadAt(r.buf[:want], r.pos)
	r.buf, r.bufStart = r.buf[:n], r.pos
	if n == 0 {
		return err
	}
	return nil
}

// Seek sets the reader's position, as (*os.File).Seek would.
func (r *tagReader) Seek(offset int64, whence int) (int64, error) {
	var at int64
	switch whence {
	case io.SeekStart:
		at = offset
	case io.SeekCurrent:
		at = r.pos + offset
	case io.SeekEnd:
		info, err := r.f.Stat()
		if err != nil {
			return 0, err
		}
		at = info.Size() + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if at < 0 {
		return 0, errors.New("seek to a negative position") // the position stays, as a file's
	}
	r.pos = at
	return at, nil
}
