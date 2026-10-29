// Command mkfixtures crafts the in-process probe's odd-shaped fixtures (ID3
// variants, M4B chapter variants, hand-built Matroska) that ffmpeg and lame
// don't write, from the files scripts/gen-probe-fixtures.sh made in the same
// folder (lame-vbr.mp3, lame-cbr-info.mp3, lame-cbr-noxing.mp3,
// lame-cbr32-noxing.mp3, m4b-aac-chap.m4b); each is written as craft-<name>. The script runs it:
//
//	go run ./cmd/mkfixtures internal/metadata/testdata/probe
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"unicode/utf16"
)

var dir string

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func read(name string) []byte {
	b, err := os.ReadFile(filepath.Join(dir, name))
	must(err)
	return b
}

func write(name string, parts ...[]byte) {
	must(os.WriteFile(filepath.Join(dir, "craft-"+name), bytes.Join(parts, nil), 0o644)) //nolint:gosec // G306: a checked-in test fixture
}

// ---- ID3

func syncsafe(n int) []byte {
	return []byte{byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
}

func be32(n int) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, uint32(n)); return b }

func frame(ver int, id string, data []byte, flags uint16) []byte {
	var h []byte
	switch ver {
	case 2:
		h = append([]byte(id), byte(len(data)>>16), byte(len(data)>>8), byte(len(data)))
		return append(h, data...)
	case 3:
		h = append([]byte(id), be32(len(data))...)
	default:
		h = append([]byte(id), syncsafe(len(data))...)
	}
	h = append(h, byte(flags>>8), byte(flags))
	return append(h, data...)
}

func latin1(s string) []byte {
	var b []byte
	for _, r := range s {
		b = append(b, byte(r))
	}
	return b
}

func utf16bom(s string) []byte {
	b := []byte{0xff, 0xfe}
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

func text(enc byte, s string) []byte {
	switch enc {
	case 0:
		return append([]byte{0}, latin1(s)...)
	case 1:
		return append([]byte{1}, utf16bom(s)...)
	default:
		return append([]byte{3}, s...)
	}
}

func txxx(enc byte, desc, val string) []byte {
	switch enc {
	case 1:
		b := append([]byte{1}, utf16bom(desc)...)
		b = append(b, 0, 0)
		return append(b, utf16bom(val)...)
	case 0:
		b := append([]byte{0}, latin1(desc)...)
		b = append(b, 0)
		return append(b, latin1(val)...)
	}
	b := append([]byte{3}, desc...)
	b = append(b, 0)
	return append(b, val...)
}

func tag(ver int, flags byte, frames ...[]byte) []byte {
	body := bytes.Join(frames, nil)
	body = append(body, make([]byte, 64)...) // padding
	h := append([]byte("ID3"), byte(ver), 0, flags)
	h = append(h, syncsafe(len(body))...)
	return append(h, body...)
}

func unsync(b []byte) []byte {
	var o []byte
	for i, c := range b {
		o = append(o, c)
		if c == 0xff && (i+1 == len(b) || b[i+1]&0xe0 == 0xe0 || b[i+1] == 0) {
			o = append(o, 0)
		}
	}
	return o
}

func chap(id string, start, end int, sub ...[]byte) []byte {
	b := append([]byte(id), 0)
	b = append(b, be32(start)...)
	b = append(b, be32(end)...)
	b = append(b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	for _, s := range sub {
		b = append(b, s...)
	}
	return b
}

func id3v1(title, artist, album, year string, genre byte) []byte {
	b := make([]byte, 128)
	copy(b, "TAG")
	copy(b[3:33], title)
	copy(b[33:63], artist)
	copy(b[63:93], album)
	copy(b[93:97], year)
	b[127] = genre
	return b
}

func ape(items map[string]string) []byte {
	var body []byte
	for k, v := range items {
		item := make([]byte, 8)
		binary.LittleEndian.PutUint32(item, uint32(len(v)))
		item = append(item, k...)
		item = append(item, 0)
		item = append(item, v...)
		body = append(body, item...)
	}
	foot := func(hdr bool) []byte {
		f := []byte("APETAGEX")
		x := make([]byte, 24)
		binary.LittleEndian.PutUint32(x[0:], 2000)
		binary.LittleEndian.PutUint32(x[4:], uint32(len(body)+32))
		binary.LittleEndian.PutUint32(x[8:], uint32(len(items)))
		fl := uint32(0x80000000)
		if hdr {
			fl |= 0x20000000
		}
		binary.LittleEndian.PutUint32(x[12:], fl)
		return append(f, x...)
	}
	return bytes.Join([][]byte{foot(true), body, foot(false)}, nil)
}

func mp3s() {
	vbr := read("lame-vbr.mp3")
	cbr := read("lame-cbr-info.mp3")
	cbrNoX := read("lame-cbr-noxing.mp3")
	write("id3v23-full.mp3", tag(3, 0,
		frame(3, "TIT2", text(1, "Kapitel 1 – Anfang"), 0),
		frame(3, "TALB", text(0, "Café Book"), 0),
		frame(3, "TPE1", text(0, "Ann Author"), 0),
		frame(3, "TPE2", text(1, "Ann Author"), 0),
		frame(3, "TCOM", text(0, "Nick Narr"), 0),
		frame(3, "TCON", text(0, "(101)"), 0),
		frame(3, "TYER", text(0, "2004"), 0),
		frame(3, "TDAT", text(0, "1205"), 0),
		frame(3, "TXXX", txxx(1, "SERIES", "The Series #3"), 0),
		frame(3, "TXXX", txxx(0, "NARRATOR", "Ned Reader"), 0),
		frame(3, "TXXX", txxx(0, "series-part", "3"), 0),
		frame(3, "COMM", append([]byte{0, 'e', 'n', 'g', 0}, latin1("a comment")...), 0),
		frame(3, "TIT2", text(0, "Second TIT2 ignored"), 0),
	), vbr)
	write("id3v22.mp3", tag(2, 0,
		frame(2, "TT2", text(0, "Two Two Title"), 0),
		frame(2, "TAL", text(0, "Two Two Album"), 0),
		frame(2, "TP1", text(0, "Two Artist"), 0),
		frame(2, "TCO", text(0, "Speech"), 0),
		frame(2, "TYE", text(0, "1999"), 0),
		frame(2, "TCM", text(0, "Comp Two"), 0),
	), vbr)
	// v2.4, whole-tag unsync, a 0xff byte in a value, multi-value TCON, TDRC.
	body := bytes.Join([][]byte{
		frame(4, "TIT2", text(0, "Unsync ÿ Title"), 0),
		frame(4, "TALB", text(3, "Unsync Album ÿ"), 0),
		frame(4, "TCON", append(text(3, "Fantasy"), append([]byte{0}, "Horror"...)...), 0),
		frame(4, "TDRC", text(3, "2012-03-04"), 0),
		frame(4, "TIT1", text(3, "Grouped, Book 5"), 0),
		frame(4, "TPE1", text(3, "Ünï Cödé 🎧"), 0),
	}, nil)
	ub := unsync(append(body, make([]byte, 32)...))
	h := append([]byte("ID3"), 4, 0, 0x80)
	h = append(h, syncsafe(len(ub))...)
	write("id3v24-unsync.mp3", h, ub, vbr)
	// v2.4 with a v2.3-style (not syncsafe) frame size over 0x7f, and a compressed frame.
	long := make([]byte, 0, 300)
	long = append(long, 3)
	for len(long) < 200 {
		long = append(long, "Long Album Name "...)
	}
	badsize := append([]byte("TALB"), be32(len(long))...)
	badsize = append(badsize, 0, 0)
	badsize = append(badsize, long...)
	var zb bytes.Buffer
	zw := zlib.NewWriter(&zb)
	raw := text(3, "Compressed Artist")
	_, err := zw.Write(raw)
	must(err)
	must(zw.Close())
	comp := append(be32(len(raw)), zb.Bytes()...)
	write("id3v24-oddframes.mp3", tag(4, 0, badsize, frame(4, "TPE1", comp, 0x0009), frame(4, "TIT2", text(3, "Odd Frames"), 0)), vbr)
	// CHAP out of order, one without a title sub-frame, a CTOC.
	write("chap-unordered.mp3", tag(3, 0,
		frame(3, "TIT2", text(0, "Chaptered"), 0),
		frame(3, "CTOC", append([]byte("toc\x00"), 3, 3, 'c', '2', 0, 'c', '1', 0, 'c', '0', 0), 0),
		frame(3, "CHAP", chap("c2", 4000, 5300, frame(3, "TIT2", text(1, "Third ✓"), 0)), 0),
		frame(3, "CHAP", chap("c0", 0, 2000, frame(3, "TIT2", text(0, "First"), 0)), 0),
		frame(3, "CHAP", chap("c1", 2000, 4000), 0),
	), vbr)
	write("two-id3.mp3", tag(3, 0, frame(3, "TALB", text(0, "First Tag Album"), 0)),
		tag(4, 0, frame(4, "TALB", text(3, "Second Tag Album"), 0), frame(4, "TPE1", text(3, "Second Tag Artist"), 0)), vbr)
	write("junk-vbr.mp3", tag(3, 0, frame(3, "TIT2", text(0, "Junk VBR"), 0)), make([]byte, 700), vbr)
	write("junk-cbr.mp3", tag(3, 0, frame(3, "TIT2", text(0, "Junk CBR"), 0)), make([]byte, 700), cbr)
	write("v1-and-v2.mp3", tag(3, 0, frame(3, "TIT2", text(0, "V2 Title"), 0)), cbrNoX, id3v1("V1 Title", "V1 Artist", "V1 Album", "1990", 101))
	write("v1-genre255.mp3", cbrNoX, id3v1("Only V1 é", "Artist   ", "Album", "abcd", 255))
	// Empty UTF-16 strings, as LibriVox's MP3s carry them: a COMM whose
	// description is a 0 where its BOM goes, a TXXX with no value bytes. FFmpeg
	// 8.1 reads both as "" (so the ID3v1 tag behind is not read); 7.1 drops both
	// frames and reads the ID3v1 tag.
	write("id3-empty-utf16.mp3", tag(3, 0,
		frame(3, "COMM", append([]byte{1, 'X', 'X', 'X', 0, 0}, utf16bom("https://example.org/")...), 0),
		frame(3, "TXXX", append([]byte{1}, append(utf16bom("SERIES"), 0, 0)...), 0),
	), cbrNoX, id3v1("V1 Behind Empty", "V1 Artist", "V1 Album", "2003", 101))
	write("ape.mp3", cbrNoX, ape(map[string]string{"Title": "Ape Title", "Artist": "Ape Artist", "Album": "Ape Album"}), id3v1("V1 With Ape", "", "", "", 12))
	// An attached picture (APIC, v2.2 PIC) on a CBR file with no Xing frame: its
	// stream's duration by the bitrate, in 1/90000, rounds up past the audio's at
	// 32 kbps over an odd byte count, and the format's duration is the longer
	// (estimate_timings_from_bit_rate). A picture of a type FFmpeg doesn't know
	// is no stream.
	cbr32 := read("lame-cbr32-noxing.mp3")
	if len(cbr32)%2 == 0 {
		cbr32 = cbr32[:len(cbr32)-1]
	}
	pic := func(mime string) []byte {
		d := append([]byte{0}, mime...)
		d = append(d, 0, 3)
		d = append(d, "Cover\x00"...)
		return append(d, png1x1...)
	}
	write("apic-cbr.mp3", tag(3, 0, frame(3, "TIT2", text(0, "Pictured"), 0), frame(3, "APIC", pic("image/png"), 0)), cbr32)
	write("apic-v22.mp3", tag(2, 0, frame(2, "PIC", append(append([]byte{0}, "PNG\x03\x00"...), png1x1...), 0)), cbr32)
	write("apic-unknown-type.mp3", tag(3, 0, frame(3, "APIC", pic("image/x-unknown"), 0)), cbr32)
	write("apic-none.mp3", tag(3, 0, frame(3, "TIT2", text(0, "No Picture"), 0)), cbr32)
	write("concat.mp3", vbr, vbr)
	write("truncated-vbr.mp3", vbr[:len(vbr)*6/10])
	write("truncated-cbr.mp3", cbrNoX[:len(cbrNoX)*6/10+77])
}

// png1x1 is a one-pixel PNG.
var png1x1 = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

// ---- MP4

// boxAt is where the n-th box of type typ starts its body (past the type).
func boxAt(b []byte, typ string, nth int) int {
	i := 0
	for k := 0; ; k++ {
		j := bytes.Index(b[i:], []byte(typ))
		if j < 0 {
			panic("no " + typ)
		}
		i += j
		if k == nth {
			return i + 4
		}
		i++
	}
}

// patchType renames the n-th box of type from to type to (same size).
func patchType(b []byte, from, to string, nth int) []byte {
	o := append([]byte(nil), b...)
	copy(o[boxAt(o, from, nth)-4:], to)
	return o
}

func m4bs() {
	src := read("m4b-aac-chap.m4b")
	write("m4b-textonly.m4b", patchType(src, "chpl", "free", 0))
	write("m4b-chplonly.m4b", patchType(src, "chap", "free", 0))
	write("m4b-noelst.m4b", patchType(src, "edts", "free", 0))
	// chpl titles that differ from the text track's (same lengths).
	o := append([]byte(nil), src...)
	ci := bytes.Index(o, []byte("chpl"))
	for _, p := range [][2]string{{"Opening", "OPENING"}, {"The End", "THE END"}} {
		j := bytes.Index(o[ci:], []byte(p[0]))
		copy(o[ci+j:], p[1])
	}
	write("m4b-chpl-differs.m4b", o)
	// A chpl with one more chapter than the text track (rebuilt at the end of moov).
	o = append([]byte(nil), src...)
	ci = bytes.Index(o, []byte("chpl")) - 4
	old := int(binary.BigEndian.Uint32(o[ci:]))
	var nb []byte
	nb = append(nb, 1, 0, 0, 0, 0, 0, 0, 0, 4)
	for _, c := range []struct {
		t int64
		s string
	}{{0, "c-one"}, {20000000, "c-two"}, {41230000, "c-three"}, {45000000, "c-four"}} {
		x := make([]byte, 8)
		binary.BigEndian.PutUint64(x, uint64(c.t))
		nb = append(nb, x...)
		nb = append(nb, byte(len(c.s)))
		nb = append(nb, c.s...)
	}
	box := append(be32(len(nb)+8), "chpl"...)
	box = append(box, nb...)
	if ci+old != len(o) {
		panic("chpl not last")
	}
	delta := len(box) - old
	o = append(o[:ci], box...)
	for _, name := range []string{"moov", "udta"} {
		k := bytes.LastIndex(o, []byte(name)) - 4
		binary.BigEndian.PutUint32(o[k:], binary.BigEndian.Uint32(o[k:])+uint32(delta))
	}
	write("m4b-chpl-more.m4b", o)
	// The audio track's mdhd duration shorter than its samples.
	o = append([]byte(nil), src...)
	mi := bytes.Index(o, []byte("mdhd"))
	d := binary.BigEndian.Uint32(o[mi+4+16:])
	binary.BigEndian.PutUint32(o[mi+4+16:], d-44100*2)
	write("m4b-mdhd-short.m4b", o)
	write("m4b-truncated.m4b", src[:len(src)*7/10])
	// The chapter text track (the second trak: its elst, mdhd and stts are the
	// second of each) cut short as some taggers write it, with a chpl beside it:
	// mov_fix_index keeps its samples up to the first that reaches the edit's
	// end, so a last sample at that end (zero length) is no chapter and the
	// chpl's last chapter stays open to the end of the file. With an edit of
	// length 0 (every sample at 0) only the first sample is kept.
	chapTrack := func(editMS uint32, durs [3]uint32) []byte {
		o := append([]byte(nil), src...)
		binary.BigEndian.PutUint32(o[boxAt(o, "elst", 1)+8:], editMS)
		binary.BigEndian.PutUint32(o[boxAt(o, "mdhd", 1)+16:], editMS)
		st := boxAt(o, "stts", 1)
		if binary.BigEndian.Uint32(o[st+4:]) != 3 {
			panic("the chapter track's stts is not three entries")
		}
		for i, d := range durs {
			binary.BigEndian.PutUint32(o[st+8+i*8+4:], d)
		}
		return o
	}
	write("m4b-chaptrack-trailing.m4b", chapTrack(4123, [3]uint32{2000, 2123, 0}))
	write("m4b-chaptrack-zero-edit.m4b", chapTrack(0, [3]uint32{0, 0, 0}))
}

// ---- Matroska

func vint(n int) []byte {
	switch {
	case n < 0x7f:
		return []byte{0x80 | byte(n)}
	case n < 0x3fff:
		return []byte{0x40 | byte(n>>8), byte(n)}
	case n < 0x1fffff:
		return []byte{0x20 | byte(n>>16), byte(n >> 8), byte(n)}
	default:
		return []byte{0x10 | byte(n>>24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
}

func el(id uint32, parts ...[]byte) []byte {
	var idb []byte
	switch {
	case id > 0xffffff:
		idb = []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	case id > 0xffff:
		idb = []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	case id > 0xff:
		idb = []byte{byte(id >> 8), byte(id)}
	default:
		idb = []byte{byte(id)}
	}
	body := bytes.Join(parts, nil)
	return bytes.Join([][]byte{idb, vint(len(body)), body}, nil)
}

func u(id uint32, v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return el(id, b)
}

func s(id uint32, v string) []byte { return el(id, []byte(v)) }

func f32(id uint32, v float32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, math.Float32bits(v))
	return el(id, b)
}

// mp3Frames splits a CBR MP3 with no tags into its frames.
func mp3Frames(b []byte) [][]byte {
	var out [][]byte
	for i := 0; i+4 <= len(b); {
		h := binary.BigEndian.Uint32(b[i:])
		if h&0xffe00000 != 0xffe00000 {
			i++
			continue
		}
		br := []int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}[h>>12&0xf]
		sr := []int{44100, 48000, 32000}[h>>10&3]
		pad := int(h >> 9 & 1)
		fs := 144000*br/sr + pad
		if fs < 4 || i+fs > len(b) {
			break
		}
		out = append(out, b[i:i+fs])
		i += fs
	}
	return out
}

func mkas() {
	frames := mp3Frames(read("lame-cbr-noxing.mp3"))
	var blocks []byte
	for i, fr := range frames {
		tc := int16(i * 1152 * 1000 / 44100) // ms
		sb := append([]byte{0x81, byte(uint16(tc) >> 8), byte(tc), 0x80}, fr...)
		blocks = append(blocks, el(0xA3, sb)...)
	}
	cluster := el(0x1F43B675, u(0xE7, 0), blocks)
	header := el(0x1A45DFA3, u(0x4286, 1), u(0x42F7, 1), u(0x42F2, 4), u(0x42F3, 8), s(0x4282, "matroska"), u(0x4287, 4), u(0x4285, 2))
	durMS := float32(len(frames)) * 1152 * 1000 / 44100
	info := func(withDur bool) []byte {
		p := [][]byte{u(0x2AD7B1, 1000000), s(0x4D80, "craft"), s(0x5741, "craft"), s(0x7BA9, "Info Title")}
		if withDur {
			p = append(p, f32(0x4489, durMS))
		}
		return el(0x1549A966, p...)
	}
	tracks := el(0x1654AE6B, el(0xAE, u(0xD7, 1), u(0x73C5, 12345), u(0x83, 2), s(0x86, "A_MPEG/L3"),
		el(0xE1, f32(0xB5, 44100), u(0x9F, 2))))
	disp := func(t string) []byte { return el(0x80, s(0x85, t), s(0x437C, "eng")) }
	chapters := el(0x1043A770, el(0x45B9, u(0x45BC, 1),
		el(0xB6, u(0x73C4, 101), u(0x91, 0), u(0x92, 1.3e9), disp("One"),
			el(0xB6, u(0x73C4, 999), u(0x91, 0.3e9), disp("Nested ignored"))),
		el(0xB6, u(0x73C4, 102), u(0x91, 1.3e9), disp("Two (no end)")),
		el(0xB6, u(0x73C4, 103), u(0x91, 1e9), u(0x92, 2e9), disp("Out of order dropped")),
		el(0xB6, u(0x73C4, 104), u(0x91, 3.3e9), u(0x92, 4.8e9), disp("Four first"), disp("Four last wins")),
	))
	simple := func(name, val, lang string, def bool, sub ...[]byte) []byte {
		p := [][]byte{s(0x45A3, name), s(0x4487, val)}
		if lang != "" {
			p = append(p, s(0x447A, lang))
		}
		if !def {
			p = append(p, u(0x4484, 0))
		}
		p = append(p, sub...)
		return el(0x67C8, p...)
	}
	tags := el(0x1254C367,
		el(0x7373, el(0x63C0, u(0x68CA, 50), s(0x63CA, "ALBUM")), simple("TITLE", "Album Target Title", "", true)),
		el(0x7373, el(0x63C0),
			simple("ARTIST", "Craft Artist", "", true),
			simple("TITLE", "Deutscher Titel", "ger", false),
			simple("NARRATOR", "Craft Narrator", "eng", true),
			simple("SERIES", "Craft Series", "", true, simple("PART", "2", "", true)),
			simple("GENRE", "Mystery", "", true),
			simple("DATE_RELEASED", "2015", "", true),
		),
		el(0x7373, el(0x63C0, u(0x63C5, 12345)), simple("TITLE", "Track tag ignored", "", true)),
	)
	seg := func(parts ...[]byte) []byte { return el(0x18538067, parts...) }
	write("mka-a.mka", header, seg(info(true), tracks, chapters, tags, cluster))
	write("mka-nodur.mka", header, seg(info(false), tracks, cluster))
	// Tags and Chapters after the cluster, found through a SeekHead.
	chap2 := el(0x1043A770, el(0x45B9, el(0xB6, u(0x73C4, 7), u(0x91, 0), u(0x92, 2.6e9), disp("Seek One")),
		el(0xB6, u(0x73C4, 8), u(0x91, 2.6e9), u(0x92, 5e9), disp("Seek Two"))))
	tags2 := el(0x1254C367, el(0x7373, el(0x63C0), simple("ALBUM", "Seek Album", "", true), simple("ARTIST", "Seek Artist", "", true)))
	// SeekHead with fixed-width positions (8-byte uints) so its size is known.
	build := func(posTags, posChap int) []byte {
		sk := func(id []byte, pos int) []byte { return el(0x4DBB, el(0x53AB, id), u(0x53AC, uint64(pos))) }
		return el(0x114D9B74, sk([]byte{0x12, 0x54, 0xC3, 0x67}, posTags), sk([]byte{0x10, 0x43, 0xA7, 0x70}, posChap))
	}
	sh := build(0, 0)
	pre := append(append(append([]byte{}, sh...), info(true)...), tracks...)
	pre = append(pre, cluster...)
	posChap := len(pre)
	posTags := posChap + len(chap2)
	sh = build(posTags, posChap)
	body := bytes.Join([][]byte{sh, info(true), tracks, cluster, chap2, tags2}, nil)
	write("mka-seekhead.mka", header, el(0x18538067, body))
}

func main() {
	dir = os.Args[1]
	mp3s()
	m4bs()
	mkas()
	fmt.Println("ok")
}
