package metadata

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/dhowden/tag"
)

// id3Frame is an ID3v2.3 frame: id, size, no flags, data.
func id3Frame(id string, data []byte) []byte {
	out := []byte(id)
	out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
	out = append(out, 0, 0)
	return append(out, data...)
}

// syncsafe is n as ID3v2's four 7-bit bytes.
func syncsafe(n int) []byte {
	return []byte{byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
}

// id3File is an MP3-ish file: an ID3v2.3 tag (unsynchronised when unsync) with
// a title, an artist and a picture of picSize bytes, then padding, then audio.
func id3File(unsync bool, picSize, padding, audio int) []byte {
	rng := rand.New(rand.NewPCG(1, 2))
	pic := make([]byte, picSize)
	for i := range pic {
		pic[i] = byte(rng.IntN(256))
	}
	body := id3Frame("TIT2", append([]byte{0}, "A Title"...))
	body = append(body, id3Frame("TPE1", append([]byte{0}, "An Author"...))...)
	body = append(body, id3Frame("APIC", append(append([]byte{0}, "image/jpeg\x00\x03\x00"...), pic...))...)
	body = append(body, make([]byte, padding)...)
	flags := byte(0)
	if unsync {
		flags = 0x80
		var out []byte
		for _, b := range body {
			out = append(out, b)
			if b == 0xff {
				out = append(out, 0)
			}
		}
		body = out
	}
	file := append([]byte{'I', 'D', '3', 3, 0, flags}, syncsafe(len(body))...)
	file = append(file, body...)
	for range audio {
		file = append(file, byte(rng.IntN(256)))
	}
	return file
}

// tagFiles writes the files the reader is checked over and returns their paths.
func tagFiles(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{
		"unsync.mp3":     id3File(true, 150_000, 4096, 50_000),
		"plain.mp3":      id3File(false, 90_000, 0, 20_000),
		"small.mp3":      id3File(true, 100, 10, 300),
		"truncated.mp3":  id3File(true, 100_000, 0, 0)[:60_000], // the header claims more than the file holds
		"tiny.mp3":       []byte("ID3"),
		"empty.mp3":      nil,
		"notag.mp3":      bytes.Repeat([]byte{0xff, 0xfb, 0x90, 0x00}, 5000),
		"badversion.mp3": append([]byte{'I', 'D', '3', 9, 0, 0, 0, 0, 1, 0}, make([]byte, 500)...),
	}
	var paths []string
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	matches, _ := filepath.Glob(filepath.Join("..", "..", "testdata", "*", "*", "*", "*"))
	more, _ := filepath.Glob(filepath.Join("..", "..", "testdata", "*", "*", "*", "*", "*"))
	paths = append(paths, filepath.Join("..", "..", "testdata", "chaptered.m4b"))
	for _, p := range append(matches, more...) {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			paths = append(paths, p)
		}
	}
	return paths
}

// tagSummary is what dhowden read, for comparing two reads.
func tagSummary(md tag.Metadata, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	s := fmt.Sprintf("%s %s %q %q %q %q %q", md.Format(), md.FileType(), md.Title(), md.Album(), md.Artist(), md.Composer(), md.Genre())
	if p := md.Picture(); p != nil {
		s += fmt.Sprintf(" picture %s %d %x", p.MIMEType, len(p.Data), p.Data[:min(16, len(p.Data))])
	}
	return s + fmt.Sprint(" ", len(md.Raw()))
}

// dhowden reads exactly the same tags through a tagReader as from the file.
func TestTagReaderReadsTagsAsTheFile(t *testing.T) {
	t.Parallel()
	read := 0
	for _, p := range tagFiles(t) {
		want := func() string {
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			return tagSummary(tag.ReadFrom(f))
		}()
		got := func() string {
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			return tagSummary(tag.ReadFrom(newTagReader(f)))
		}()
		if got != want {
			t.Errorf("%s:\n file:      %s\n tagReader: %s", p, want, got)
		}
		if want[:6] != "error:" {
			read++
		}
	}
	if read < 4 {
		t.Fatalf("only %d of the files had tags dhowden read: the check compares little", read)
	}
}

// The unsynchronised tag is read as the tag it is (the fixture is well formed).
func TestTagReaderUnsynchronisedTag(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "unsync.mp3")
	if err := os.WriteFile(p, id3File(true, 200_000, 100, 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	md, err := tag.ReadFrom(newTagReader(f))
	if err != nil {
		t.Fatal(err)
	}
	if md.Title() != "A Title" || md.Artist() != "An Author" || md.Picture() == nil || len(md.Picture().Data) != 200_000 {
		t.Fatalf("read %s", tagSummary(md, nil))
	}
}

// Any run of reads and seeks returns what the same run on the file returns:
// the bytes, the counts, the end of the file and the positions.
func TestTagReaderReadSeekMatchesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, data := range map[string][]byte{
		"tag.mp3":   id3File(true, 100_000, 2000, 30_000),
		"short.mp3": id3File(false, 100_000, 0, 0)[:40_000],
		"plain.bin": bytes.Repeat([]byte("abcdefgh"), 4000),
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		a, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.Close() })
		b, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close() })
		r := newTagReader(b)
		rng := rand.New(rand.NewPCG(3, 4))
		size := int64(len(data))
		for step := range 3000 {
			var op string
			var wantN, gotN int64
			var wantErr, gotErr error
			var wantBuf, gotBuf []byte
			switch rng.IntN(5) {
			case 0:
				off := rng.Int64N(size + 100)
				op = fmt.Sprintf("seek start %d", off)
				wantN, wantErr = a.Seek(off, io.SeekStart)
				gotN, gotErr = r.Seek(off, io.SeekStart)
			case 1:
				off := rng.Int64N(2000) - 1000
				op = fmt.Sprintf("seek current %d", off)
				wantN, wantErr = a.Seek(off, io.SeekCurrent)
				gotN, gotErr = r.Seek(off, io.SeekCurrent)
			case 2:
				off := -rng.Int64N(300)
				op = fmt.Sprintf("seek end %d", off)
				wantN, wantErr = a.Seek(off, io.SeekEnd)
				gotN, gotErr = r.Seek(off, io.SeekEnd)
			default:
				n := rng.IntN(70_000) + 1
				if rng.IntN(2) == 0 {
					n = rng.IntN(16) + 1
				}
				op = fmt.Sprintf("read %d", n)
				wantBuf, gotBuf = make([]byte, n), make([]byte, n)
				var wn, gn int
				wn, wantErr = io.ReadFull(a, wantBuf)
				gn, gotErr = io.ReadFull(r, gotBuf)
				wantN, gotN = int64(wn), int64(gn)
				wantBuf, gotBuf = wantBuf[:wn], gotBuf[:gn]
			}
			if wantN != gotN || (wantErr == nil) != (gotErr == nil) || !bytes.Equal(wantBuf, gotBuf) {
				t.Fatalf("%s step %d, %s: file %d %v, tagReader %d %v (bytes equal %v)", name, step, op,
					wantN, wantErr, gotN, gotErr, bytes.Equal(wantBuf, gotBuf))
			}
			if wantErr != nil && gotErr != nil && (wantErr == io.EOF) != (gotErr == io.EOF) {
				t.Fatalf("%s step %d, %s: file error %v, tagReader error %v", name, step, op, wantErr, gotErr)
			}
		}
	}
}
