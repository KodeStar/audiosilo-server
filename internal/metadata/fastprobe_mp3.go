package metadata

// The in-process probe's MP3 reader (fastprobe.go): ID3v2 (several tags in a
// row, the extended header, unsynchronisation, compressed frames, CHAP), ID3v1
// and the duration from a Xing/Info or VBRI header, else a constant bitrate's
// estimate, as FFmpeg 7.1's id3v2.c, id3v1.c and mp3dec.c read them. A VBR file
// with no header is ffprobe's: its estimate depends on the frames its decoder
// happens to read (SCAN-SPIKE decision 1 keeps that value). FFmpeg 8 differs
// from 7 here twice (ffRules): a Xing frame count's duration leaves out the
// LAME header's encoder delay and padding, and decode_str reads an empty
// UTF-16 string (no bytes, or a 0 where its BOM goes) as "" instead of failing.

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

var id3v234Conv = [][2]string{
	{"TALB", "album"}, {"TCOM", "composer"}, {"TCON", "genre"}, {"TCOP", "copyright"},
	{"TENC", "encoded_by"}, {"TIT2", "title"}, {"TLAN", "language"}, {"TPE1", "artist"},
	{"TPE2", "album_artist"}, {"TPE3", "performer"}, {"TPOS", "disc"}, {"TPUB", "publisher"},
	{"TRCK", "track"}, {"TSSE", "encoder"}, {"USLT", "lyrics"},
}

var id3v24Conv = [][2]string{
	{"TCMP", "compilation"}, {"TDRC", "date"}, {"TDRL", "date"}, {"TDEN", "creation_time"},
	{"TSOA", "album-sort"}, {"TSOP", "artist-sort"}, {"TSOT", "title-sort"}, {"TIT1", "grouping"},
}

var id3v22Conv = [][2]string{
	{"TAL", "album"}, {"TCO", "genre"}, {"TCP", "compilation"}, {"TT2", "title"},
	{"TEN", "encoded_by"}, {"TP1", "artist"}, {"TP2", "album_artist"}, {"TP3", "performer"},
	{"TRK", "track"},
}

// id3Chap is one CHAP frame as id3v2.c keeps it.
type id3Chap struct {
	elementID  string
	start, end uint32
	meta       avDict
}

// cursor is FFmpeg's AVIOContext over one frame's bytes (reads past the end
// return zeros, as avio_r8 at EOF does), and the rules of the FFmpeg read for.
type cursor struct {
	b   []byte
	pos int
	ff  ffRules
}

func (c *cursor) r8() byte {
	if c.pos >= len(c.b) {
		c.pos++
		return 0
	}
	v := c.b[c.pos]
	c.pos++
	return v
}

func (c *cursor) rb16() uint16 { return uint16(c.r8())<<8 | uint16(c.r8()) }
func (c *cursor) rl16() uint16 { a := c.r8(); return uint16(c.r8())<<8 | uint16(a) }
func (c *cursor) rb32() uint32 { return uint32(c.rb16())<<16 | uint32(c.rb16()) }

// decodeStr is id3v2.c decode_str: one string, its terminator consumed; left
// is decremented by what was read. ok false where FFmpeg returns an error.
func decodeStr(c *cursor, encoding byte, left *int) (string, bool) {
	var out []byte
	var ch uint32 = 1
	l := *left
	get := c.rb16
	if l == 0 && c.ff >= ff8 {
		return "", true
	}
	switch encoding {
	case 0:
		for l > 0 && ch != 0 {
			ch = uint32(c.r8())
			if ch != 0 {
				out = utf8.AppendRune(out, rune(ch))
			}
			l--
		}
	case 1, 2:
		if encoding == 1 {
			if l -= 2; l < 0 {
				return "", false
			}
			switch bom := c.rb16(); bom {
			case 0xfffe:
				get = c.rl16
			case 0xfeff:
			default:
				// FFmpeg 8 reads a 0 where the BOM goes as an empty string.
				*left = l
				return "", bom == 0 && c.ff >= ff8
			}
		}
		for l > 1 && ch != 0 {
			l -= 2
			ch = uint32(get())
			if hi := ch - 0xD800; hi < 0x800 {
				var lo uint32
				if l -= 2; l >= 0 {
					lo = uint32(get())
				}
				lo -= 0xDC00
				if lo > 0x3FF || hi > 0x3FF {
					break
				}
				ch = lo + (hi << 10) + 0x10000
			}
			if ch != 0 {
				out = utf8.AppendRune(out, rune(ch))
			}
		}
		if l < 0 {
			l += 2
		}
	case 3:
		for l > 0 && ch != 0 {
			ch = uint32(c.r8())
			if ch != 0 {
				out = append(out, byte(ch))
			}
			l--
		}
	}
	*left = l
	return string(out), true
}

// readTTag is id3v2.c read_ttag.
func readTTag(c *cursor, taglen int, d *avDict, key string) {
	if taglen < 1 {
		return
	}
	enc := c.r8()
	taglen--
	dst, ok := decodeStr(c, enc, &taglen)
	if !ok {
		return
	}
	if key == "TCON" || key == "TCO" {
		if g, ok := id3Genre(dst); ok {
			d.set(key, id3v1Genres[g], dictDontOverwrite)
			return
		}
	}
	if key == "TXXX" || key == "TXX" {
		val, ok := decodeStr(c, enc, &taglen)
		if !ok {
			return
		}
		d.set(dst, val, dictDontOverwrite)
		return
	}
	if dst == "" {
		return
	}
	d.set(key, dst, dictDontOverwrite)
}

// id3Genre is read_ttag's genre test: sscanf(dst, "(%d)") or sscanf(dst, "%d")
// converting, then genre <= 191 (unsigned, so a negative one fails).
func id3Genre(s string) (int, bool) {
	n, ok := scanInt(s, true)
	if !ok {
		n, ok = scanInt(s, false)
	}
	if !ok || n < 0 || n > 191 {
		return 0, false
	}
	return int(n), true
}

// scanInt is sscanf's %d at the start of s (after a literal "(" when paren).
func scanInt(s string, paren bool) (int64, bool) {
	if paren {
		if !strings.HasPrefix(s, "(") {
			return 0, false
		}
		s = s[1:]
	}
	i := 0
	for i < len(s) && strings.IndexByte(" \t\n\v\f\r", s[i]) >= 0 {
		i++
	}
	j := i
	if j < len(s) && (s[j] == '+' || s[j] == '-') {
		j++
	}
	k := j
	for k < len(s) && s[k] >= '0' && s[k] <= '9' {
		k++
	}
	if k == j {
		return 0, false
	}
	n, err := strconv.ParseInt(s[i:k], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func syncsafeToSize(v uint32) uint32 {
	return (v & 0x7f) + ((v & (0x7f << 8)) >> 1) + ((v & (0x7f << 16)) >> 2) + ((v & (0x7f << 24)) >> 3)
}

func isTagID(b []byte) bool {
	for _, c := range b {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return len(b) > 0
}

// readID3v2 reads the ID3v2 tags at off (several in a row, as
// id3v2_read_internal does) into d and chaps, returning where they end.
func readID3v2(b *blockFile, off int64, d *avDict, chaps *[]id3Chap) (int64, error) {
	for {
		h, ok := b.bytesAt(off, 10)
		if !ok || !(h[0] == 'I' && h[1] == 'D' && h[2] == '3' && h[3] != 0xff && h[4] != 0xff &&
			h[6]&0x80 == 0 && h[7]&0x80 == 0 && h[8]&0x80 == 0 && h[9]&0x80 == 0) {
			break
		}
		size := int64(h[6]&0x7f)<<21 | int64(h[7]&0x7f)<<14 | int64(h[8]&0x7f)<<7 | int64(h[9]&0x7f)
		end := off + 10 + size
		if err := parseID3v2(b, off+10, size, h[3], h[5], d, chaps); err != nil {
			return 0, err
		}
		if h[3] == 4 && h[5]&0x10 != 0 {
			end += 10
		}
		off = end
	}
	d.conv(id3v234Conv)
	d.conv(id3v22Conv)
	d.conv(id3v24Conv)
	mergeDate(d)
	return off, nil
}

func parseID3v2(b *blockFile, start, size int64, version, flags byte, d *avDict, chaps *[]id3Chap) error {
	if size > 64<<20 {
		return unsure("id3v2 tag over 64 MiB")
	}
	tagBuf := make([]byte, min(size, max(b.size-start, 0)))
	n, _ := b.ReadAt(tagBuf, start)
	tagBuf = tagBuf[:n]
	pb := &cursor{b: tagBuf, ff: b.ff}
	length := int(size)
	var isv34 bool
	var hdr int
	comm := "COMM"
	switch version {
	case 2:
		if flags&0x40 != 0 {
			return nil
		}
		hdr, comm = 6, "COM"
	case 3, 4:
		isv34, hdr = true, 10
	default:
		return nil
	}
	unsync := flags&0x80 != 0
	if isv34 && flags&0x40 != 0 {
		e := pb.rb32()
		extlen := int(e>>24&0x7f)<<21 | int(e>>16&0x7f)<<14 | int(e>>8&0x7f)<<7 | int(e&0x7f)
		if version == 4 {
			extlen -= 4
		}
		if extlen < 0 {
			return nil
		}
		pb.pos += extlen
		if length -= extlen + 4; length < 0 {
			return nil
		}
	}
	for length >= hdr {
		var tflags uint16
		var tag string
		var tlen uint32
		if pb.pos+hdr-2 > len(pb.b) && isv34 || pb.pos+hdr > len(pb.b) && !isv34 {
			break
		}
		if isv34 {
			tag = string(pb.b[pb.pos : pb.pos+4])
			pb.pos += 4
			tlen = pb.rb32()
			if version == 4 && tlen > 0x7f {
				if int64(tlen) < int64(length) {
					cur := pb.pos
					chk := func(at int) int {
						if at < 0 || at+4 > len(pb.b) {
							return -1
						}
						t := pb.b[at : at+4]
						if binary.BigEndian.Uint32(t) == 0 || isTagID(t) {
							return 1
						}
						return 0
					}
					if chk(cur+2+int(syncsafeToSize(tlen))) == 1 {
						tlen = syncsafeToSize(tlen)
					} else if chk(cur+2+int(tlen)) != 1 {
						break
					}
				} else {
					tlen = syncsafeToSize(tlen)
				}
			}
			tflags = pb.rb16()
		} else {
			tag = string(pb.b[pb.pos : pb.pos+3])
			pb.pos += 3
			tlen = uint32(pb.r8())<<16 | uint32(pb.r8())<<8 | uint32(pb.r8())
		}
		if tlen > 1<<28 {
			break
		}
		length -= hdr + int(tlen)
		if length < 0 {
			break
		}
		next := pb.pos + int(tlen)
		if tlen == 0 {
			continue
		}
		dlen := tlen
		if tflags&0x0001 != 0 {
			if tlen < 4 {
				break
			}
			dlen = pb.rb32()
			tlen -= 4
		}
		tcomp, tencr, tunsync := tflags&0x0008 != 0, tflags&0x0004 != 0, tflags&0x0002 != 0
		isText := tag[0] == 'T'
		isChap := isv34 && tag == "CHAP"
		known := isText || tag == "USLT" || tag == comm || isChap
		switch {
		case tencr:
		case known:
			end := min(pb.pos+int(tlen), len(pb.b))
			data := pb.b[min(pb.pos, end):end]
			if unsync || tunsync {
				var o []byte
				for i := 0; i < len(data); i++ {
					o = append(o, data[i])
					if data[i] == 0xff && i+1 < len(data) && data[i+1] == 0 {
						i++
					}
				}
				data = o
			}
			if tcomp {
				if dlen/32768 > uint32(len(data)) || len(data) == 0 {
					break
				}
				zr, err := zlib.NewReader(bytes.NewReader(data))
				if err != nil {
					break
				}
				u, err := io.ReadAll(io.LimitReader(zr, int64(dlen)))
				if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
					break
				}
				data = u
			}
			fc := &cursor{b: data, ff: b.ff}
			switch {
			case isText:
				readTTag(fc, len(data), d, tag)
			case tag == "USLT":
				readUSLT(fc, len(data), d)
			case tag == comm:
				readComment(fc, len(data), d)
			case isChap:
				if ch, ok := readChapter(fc, len(data)); ok {
					*chaps = append(*chaps, ch)
				}
			}
		case tag[0] == 0:
			pb.pos = next
			// padding: stop
			length = -1
		}
		if length < 0 {
			break
		}
		pb.pos = next
	}
	return nil
}

func readUSLT(c *cursor, taglen int, d *avDict) {
	if taglen < 4 {
		return
	}
	enc := c.r8()
	taglen--
	lang := string([]byte{c.r8(), c.r8(), c.r8()})
	taglen -= 3
	desc, ok := decodeStr(c, enc, &taglen)
	if !ok || taglen < 0 {
		return
	}
	text, ok := decodeStr(c, enc, &taglen)
	if !ok || taglen < 0 {
		return
	}
	key := "lyrics-"
	if desc != "" {
		key += desc + "-"
	}
	d.set(key+lang, text, 0)
}

func readComment(c *cursor, taglen int, d *avDict) {
	if taglen < 4 {
		return
	}
	enc := c.r8()
	c.pos += 3
	taglen -= 4
	desc, ok := decodeStr(c, enc, &taglen)
	if !ok {
		return
	}
	key := "comment"
	if desc != "" {
		key = desc
	}
	text, ok := decodeStr(c, enc, &taglen)
	if !ok {
		return
	}
	d.set(key, text, dictDontOverwrite)
}

func readChapter(c *cursor, length int) (id3Chap, bool) {
	var ch id3Chap
	id, ok := decodeStr(c, 0, &length)
	if !ok || length < 16 {
		return ch, false
	}
	ch.elementID = id
	ch.start = c.rb32()
	ch.end = c.rb32()
	c.pos += 8
	length -= 16
	for length > 10 {
		if c.pos+4 > len(c.b) {
			return ch, false
		}
		tag := string(c.b[c.pos : c.pos+4])
		c.pos += 4
		taglen := int(int32(c.rb32()))
		c.pos += 2
		length -= 10
		if taglen < 0 || taglen > length {
			return ch, false
		}
		if tag[0] == 'T' {
			readTTag(c, taglen, &ch.meta, tag)
		} else {
			c.pos += taglen
		}
		length -= taglen
	}
	ch.meta.conv(id3v234Conv)
	ch.meta.conv(id3v24Conv)
	return ch, true
}

func isNumber(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// mergeDate is id3v2.c merge_date (TYER/TDAT/TIME into "date").
func mergeDate(d *avDict) {
	get := func(k string) (string, bool) {
		for _, e := range d.e { // AV_DICT_MATCH_CASE
			if e.k == k && len(e.v) == 4 && isNumber(e.v) {
				return e.v, true
			}
		}
		return "", false
	}
	y, ok := get("TYER")
	if !ok {
		if y, ok = get("TYE"); !ok {
			return
		}
	}
	date := y
	d.del("TYER")
	d.del("TYE")
	if t, ok := get("TDAT"); ok || func() bool { t, ok = get("TDA"); return ok }() {
		date += "-" + t[2:4] + "-" + t[0:2]
		d.del("TDAT")
		d.del("TDA")
		if t, ok := get("TIME"); ok || func() bool { t, ok = get("TIM"); return ok }() {
			date += " " + t[0:2] + ":" + t[2:4]
			d.del("TIME")
			d.del("TIM")
		}
	}
	d.set("date", date, 0)
}

// readID3v1 is id3v1.c ff_id3v1_read (strings kept as their bytes).
func readID3v1(b *blockFile, d *avDict) {
	if b.size <= 128 {
		return
	}
	buf, ok := b.bytesAt(b.size-128, 128)
	if !ok || string(buf[:3]) != "TAG" {
		return
	}
	get := func(key string, f []byte) {
		var q []byte
		firstSpace := -1
		for _, c := range f {
			if c == 0 {
				break
			}
			if c == ' ' {
				if firstSpace < 0 {
					firstSpace = len(q)
				}
			} else {
				firstSpace = -1
			}
			q = append(q, c)
		}
		if firstSpace >= 0 {
			q = q[:firstSpace]
		}
		if len(q) > 0 {
			d.set(key, string(q), 0)
		}
	}
	get("title", buf[3:33])
	get("artist", buf[33:63])
	get("album", buf[63:93])
	get("date", buf[93:97])
	get("comment", buf[97:127])
	if buf[125] == 0 && buf[126] != 0 {
		d.set("track", strconv.Itoa(int(buf[126])), 0)
	}
	if g := int(buf[127]); g <= 191 {
		d.set("genre", id3v1Genres[g], 0)
	}
}

// mpaHeader is avpriv_mpegaudio_decode_header's result.
type mpaHeader struct {
	lsf, layer, sampleRate, bitRate, frameSize, channels int
	free                                                 bool
}

var mpaBitrates = [2][3][15]int{
	{{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}},
	{{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}},
}

func decodeMPA(h uint32) (mpaHeader, bool) {
	var m mpaHeader
	if h&0xffe00000 != 0xffe00000 || h&(3<<19) == 1<<19 || h&(3<<17) == 0 ||
		h&(0xf<<12) == 0xf<<12 || h&(3<<10) == 3<<10 {
		return m, false
	}
	mpeg25 := 0
	if h&(1<<20) != 0 {
		if h&(1<<19) == 0 {
			m.lsf = 1
		}
	} else {
		m.lsf, mpeg25 = 1, 1
	}
	m.layer = 4 - int(h>>17&3)
	sri := int(h >> 10 & 3)
	m.sampleRate = [3]int{44100, 48000, 32000}[sri] >> (m.lsf + mpeg25)
	padding := int(h >> 9 & 1)
	m.channels = 2
	if h>>6&3 == 3 {
		m.channels = 1
	}
	bi := int(h >> 12 & 0xf)
	if bi == 0 {
		m.free = true
		return m, true
	}
	fs := mpaBitrates[m.lsf][m.layer-1][bi]
	m.bitRate = fs * 1000
	switch m.layer {
	case 1:
		fs = (fs*12000/m.sampleRate + padding) * 4
	case 2:
		fs = fs*144000/m.sampleRate + padding
	default:
		fs = fs*144000/(m.sampleRate<<m.lsf) + padding
	}
	m.frameSize = fs
	return m, true
}

const mp3Mask = 0xFFFE0CCF

// mp3Check is mp3dec.c check(): the frame size at pos (<0 for none, -2 when
// four bytes can't be read there).
func mp3Check(b *blockFile, pos int64) (int, uint32) {
	h, ok := b.bytesAt(pos, 4)
	if !ok {
		return -2, 0
	}
	v := binary.BigEndian.Uint32(h)
	m, ok := decodeMPA(v)
	if !ok || m.free {
		return -1, 0
	}
	return m.frameSize, v
}

func probeMP3(b *blockFile) (*probeResult, error) {
	d := &avDict{}
	var chaps []id3Chap
	off, err := readID3v2(b, 0, d, &chaps)
	if err != nil {
		return nil, err
	}
	if len(d.e) == 0 {
		readID3v1(b, d)
	}
	base := off

	// mp3_parse_vbr_tags at base.
	var frames, hdrSize uint32
	var startPad, endPad int64
	var spf int64
	var sr int
	haveVBR := false
	vbrSize := 0
	if h, ok := b.bytesAt(base, 4); ok {
		v := binary.BigEndian.Uint32(h)
		if c, ok := decodeMPA(v); ok && c.layer == 3 {
			if !c.free {
				vbrSize = c.frameSize
			}
			spf = 1152
			if c.lsf == 1 {
				spf = 576
			}
			sr = c.sampleRate
			pos := base + 4
			fsize := b.size - pos
			lsfIdx, monoIdx := 0, 0
			if c.lsf == 1 {
				lsfIdx = 1
			}
			if c.channels == 1 {
				monoIdx = 1
			}
			xo := [2][2]int64{{32, 17}, {17, 9}}[lsfIdx][monoIdx]
			at := pos + xo
			if t, ok := b.bytesAt(at, 4); ok && (string(t) == "Xing" || string(t) == "Info") {
				at += 4
				fl, _ := b.bytesAt(at, 4)
				flags := binary.BigEndian.Uint32(fl)
				at += 4
				if flags&1 != 0 {
					v, _ := b.bytesAt(at, 4)
					frames = binary.BigEndian.Uint32(v)
					at += 4
				}
				if flags&2 != 0 {
					v, _ := b.bytesAt(at, 4)
					hdrSize = binary.BigEndian.Uint32(v)
					at += 4
				}
				if fsize > 0 && hdrSize > 0 {
					mn, mx := min(fsize, int64(hdrSize)), max(fsize, int64(hdrSize))
					if fsize > int64(hdrSize) && mx-mn > mn>>4 {
						return nil, unsure("mp3: concatenated file (Xing size)")
					} // else FFmpeg only warns ("growing file?") and keeps the frame count
				}
				if flags&4 != 0 { // the TOC
					at += 100
				}
				if flags&8 != 0 { // the VBR quality
					at += 4
				}
				// The encoder's short version string, then (past the revision, lowpass,
				// peak, two ReplayGains, flags and bitrate) the encoder delay and padding,
				// read for LAME and FFmpeg's own encoders.
				ver, _ := b.bytesAt(at, 9)
				dl, _ := b.bytesAt(at+9+1+1+4+2+2+1+1, 3)
				if len(ver) == 9 && len(dl) == 3 {
					if v4 := string(ver[:4]); v4 == "LAME" || v4 == "Lavf" || v4 == "Lavc" {
						v := int64(dl[0])<<16 | int64(dl[1])<<8 | int64(dl[2])
						startPad, endPad = v>>12, v&4095
					}
				}
			}
			if t, ok := b.bytesAt(base+4+32, 4); ok && string(t) == "VBRI" {
				if v, ok := b.bytesAt(base+4+32+4, 2); ok && binary.BigEndian.Uint16(v) == 1 {
					s, _ := b.bytesAt(base+4+32+4+2+4, 8)
					if len(s) == 8 {
						hdrSize = binary.BigEndian.Uint32(s[:4])
						frames = binary.BigEndian.Uint32(s[4:])
					}
				}
			}
			if frames != 0 || hdrSize != 0 {
				haveVBR = true
				off = base + int64(vbrSize)
			}
		}
	}
	// The junk skip: the first two consecutive frames within 64 KiB.
	for i := int64(0); i < 64*1024; i++ {
		fs, h1 := mp3Check(b, off+i)
		if fs > 0 {
			fs2, h2 := mp3Check(b, off+i+int64(fs))
			if fs2 >= 0 && h1&mp3Mask == h2&mp3Mask {
				off += i
				break
			}
		} else if fs == -2 {
			return nil, unsure("mp3: no two consecutive frames (ffprobe errors)")
		}
	}
	first, ok := b.bytesAt(off, 4)
	if !ok {
		return nil, unsure("mp3: no first frame")
	}
	fh, ok := decodeMPA(binary.BigEndian.Uint32(first))
	if !ok || fh.layer != 3 || fh.free {
		return nil, unsure("mp3: first frame is not a layer III frame")
	}

	var durUS int64
	switch {
	case haveVBR && frames != 0:
		samples := int64(frames) * spf
		if b.ff >= ff8 {
			samples -= startPad + endPad
		}
		stDur := rescale(samples, 14112000, int64(sr))
		durUS = tbToUS(stDur, 1, 14112000)
	case haveVBR:
		return nil, unsure("mp3: VBR header without a frame count")
	default:
		br, cbr := mp3ConstantBitrate(b, off, fh)
		if !cbr {
			return nil, unsure("mp3: no Xing/VBRI header and not constant bitrate")
		}
		stDur := rescale(b.size-off, 8*14112000, int64(br))
		durUS = tbToUS(stDur, 1, 14112000)
	}

	var cl chapterList
	for i, ch := range chaps {
		c := cl.add(int64(i), 1, 1000, int64(ch.start), int64(ch.end), true, ch.elementID, true)
		if t, ok := ch.meta.get("title"); ok {
			c.title = t
		}
	}
	return result(durUS, "mp3", d, cl.finish(durUS, 0, true)), nil
}

// mp3ConstantBitrate reports whether the stream at off looks constant-bitrate:
// its first 64 frames and frames found at 20%..80% of the file all share the
// first frame's bitrate.
func mp3ConstantBitrate(b *blockFile, off int64, fh mpaHeader) (int, bool) {
	at := off
	for range 64 {
		fs, h := mp3Check(b, at)
		if fs <= 0 {
			break
		}
		m, _ := decodeMPA(h)
		if m.bitRate != fh.bitRate || m.sampleRate != fh.sampleRate {
			return 0, false
		}
		at += int64(fs)
	}
	span := b.size - off
	for _, frac := range []int64{2, 4, 6, 8} {
		pos := off + span*frac/10
		found := false
		for i := int64(0); i < 4096 && pos+i < b.size; i++ {
			fs, h := mp3Check(b, pos+i)
			if fs <= 0 {
				continue
			}
			fs2, h2 := mp3Check(b, pos+i+int64(fs))
			if fs2 <= 0 || h&mp3Mask != h2&mp3Mask {
				continue
			}
			m, _ := decodeMPA(h)
			m2, _ := decodeMPA(h2)
			if m.bitRate != fh.bitRate || m2.bitRate != fh.bitRate {
				return 0, false
			}
			found = true
			break
		}
		if !found {
			return 0, false
		}
	}
	return fh.bitRate, true
}
