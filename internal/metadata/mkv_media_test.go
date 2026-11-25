package metadata

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func TestCoverMIMESniffsTheBytes(t *testing.T) {
	t.Parallel()
	jpeg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	for name, tc := range map[string]struct {
		data []byte
		mime string
		ok   bool
	}{
		"jpeg":   {jpeg, "image/jpeg", true},
		"png":    {png, "image/png", true},
		"html":   {[]byte("<!doctype html><script>alert(1)</script>"), "", false},
		"script": {[]byte("alert(document.domain)"), "", false},
		"svg":    {[]byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), "", false},
	} {
		mime, ok := CoverMIME(tc.data)
		if ok != tc.ok || (ok && mime != tc.mime) {
			t.Errorf("%s: CoverMIME = %q, %v; want %q, %v", name, mime, ok, tc.mime, tc.ok)
		}
	}
}

// A Tags element cut off by the file's end (a truncated copy) leaves the
// probe unsure, so ffprobe answers, rather than answering without its tags.
func TestMKVElementPastTheEndIsUnsure(t *testing.T) {
	t.Parallel()
	el := func(id []byte, data ...[]byte) []byte {
		body := bytes.Join(data, nil)
		size := binary.BigEndian.AppendUint64(nil, uint64(len(body)))
		size[0] = 0x01 // an eight-byte size
		return bytes.Join([][]byte{id, size, body}, nil)
	}
	ebml := el([]byte{0x1A, 0x45, 0xDF, 0xA3}, el([]byte{0x42, 0x82}, []byte("matroska")))
	info := el([]byte{0x15, 0x49, 0xA9, 0x66},
		el([]byte{0x2A, 0xD7, 0xB1}, []byte{0x0F, 0x42, 0x40}),
		el([]byte{0x44, 0x89}, binary.BigEndian.AppendUint32(nil, math.Float32bits(4000))))
	tracks := el([]byte{0x16, 0x54, 0xAE, 0x6B}, el([]byte{0xAE},
		el([]byte{0x83}, []byte{2}), el([]byte{0x86}, []byte("A_MPEG/L3"))))
	whole := bytes.Join([][]byte{ebml, {0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, info, tracks}, nil)
	if _, err := probeMKV(newBlockFile(bytes.NewReader(whole), int64(len(whole)))); err != nil {
		t.Fatalf("the file before the cut: %v", err)
	}
	tags := el([]byte{0x12, 0x54, 0xC3, 0x67}, make([]byte, 40))
	cut := append(whole, tags[:len(tags)-30]...)
	if _, err := probeMKV(newBlockFile(bytes.NewReader(cut), int64(len(cut)))); err == nil {
		t.Fatal("a Tags element past the file's end was skipped, want the probe unsure")
	}
}
