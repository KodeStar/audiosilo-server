package metadata

// The in-process probe's FLAC (flacdec.c) and Ogg Vorbis/Opus/FLAC readers
// (oggdec.c, oggparse*.c, fastprobe.go), as FFmpeg 7.1 reads them, with Vorbis comments and their
// CHAPTERxxx chapters (oggparsevorbis.c ogm_chapter).

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
)

var vorbisCommentConv = [][2]string{
	{"ALBUMARTIST", "album_artist"}, {"TRACKNUMBER", "track"}, {"DISCNUMBER", "disc"}, {"DESCRIPTION", "comment"},
}

// scanfInt is sscanf's %<width>d at s: optional whitespace, then up to width
// characters of sign and digits, an out-of-range value clamped as strtol does;
// the rest of s after it.
func scanfInt(s string, width int) (int, string, bool) {
	i := 0
	for i < len(s) && strings.IndexByte(" \t\n\v\f\r", s[i]) >= 0 {
		i++
	}
	j := i
	neg := false
	if j < len(s) && j-i < width && (s[j] == '+' || s[j] == '-') {
		neg = s[j] == '-'
		j++
	}
	k := j
	n, clamped := 0, false
	for k < len(s) && k-i < width && s[k] >= '0' && s[k] <= '9' {
		if d := int(s[k] - '0'); n <= (math.MaxInt-d)/10 {
			n = n*10 + d
		} else {
			n, clamped = math.MaxInt, true
		}
		k++
	}
	switch {
	case k == j:
		return 0, s, false
	case neg && clamped:
		n = math.MinInt
	case neg:
		n = -n
	}
	return n, s[k:], true
}

// ogmChapter is oggparsevorbis.c ogm_chapter: whether the comment was a chapter.
func ogmChapter(cl *chapterList, key, val string) bool {
	kl := len(key)
	if kl < 9 || !asciiFold(key[:7], "CHAPTER") {
		return false
	}
	cnum, _, ok := scanfInt(key[7:], 3)
	if !ok {
		return false
	}
	if kl <= 10 {
		h, rest, ok1 := scanfInt(val, 2)
		if !ok1 || !strings.HasPrefix(rest, ":") {
			return false
		}
		m, rest, ok2 := scanfInt(rest[1:], 2)
		if !ok2 || !strings.HasPrefix(rest, ":") {
			return false
		}
		sec, rest, ok3 := scanfInt(rest[1:], 2)
		if !ok3 || !strings.HasPrefix(rest, ".") {
			return false
		}
		ms, _, ok4 := scanfInt(rest[1:], 3)
		if !ok4 {
			return false
		}
		cl.add(int64(cnum), 1, 1000, int64(ms+1000*(sec+60*(m+60*h))), 0, false, "")
		return true
	}
	if asciiFold(key[kl-4:], "NAME") {
		if c := cl.byID[int64(cnum)]; c != nil {
			c.title = val
			return true
		}
	}
	return false
}

// vorbisComment is ff_vorbis_comment into d (chapters into cl).
func vorbisComment(b *blockFile, buf []byte, d *avDict, cl *chapterList) {
	if len(buf) < 8 {
		return
	}
	p := 0
	s := int(int32(binary.LittleEndian.Uint32(buf)))
	p += 4
	if len(buf)-p-4 < s || s < 0 {
		return
	}
	p += s
	n := binary.LittleEndian.Uint32(buf[p:])
	p += 4
	for len(buf)-p >= 4 && n > 0 {
		b.tick() // in memory: a comment costs a map lookup or two
		s := int(int32(binary.LittleEndian.Uint32(buf[p:])))
		p += 4
		if len(buf)-p < s || s < 0 {
			break
		}
		c := buf[p : p+s]
		if eq := bytes.IndexByte(c, '='); eq > 0 && eq < len(c)-1 {
			// A picture's value (base64, often megabytes) is never copied.
			if t := cutNUL(string(c[:eq])); !asciiFold(t, "METADATA_BLOCK_PICTURE") {
				if v := cutNUL(string(c[eq+1:])); !ogmChapter(cl, t, v) {
					if _, ok := d.get(t); ok {
						b.check() // a repeat copies the value so far, which grows
						d.set(t, ";", dictAppend)
					}
					d.set(t, v, dictAppend)
				}
			}
		}
		p += s
		n--
	}
	d.conv(vorbisCommentConv)
}

// noID3Front is unsure of a file that opens with an ID3v2 tag (format names it).
func noID3Front(b *blockFile, format string) error {
	var id3 avDict
	var chaps []id3Chap
	off, err := readID3v2(b, 0, &id3, &chaps)
	if err != nil {
		return err
	}
	if off != 0 {
		return unsure(format + ": ID3v2 tag in front")
	}
	return nil
}

func probeFLAC(b *blockFile) (*probeResult, error) {
	if err := noID3Front(b, "flac"); err != nil {
		return nil, err
	}
	m, ok := b.bytesAt(0, 4)
	if !ok || string(m) != "fLaC" {
		return nil, unsure("flac: no fLaC marker")
	}
	at := int64(4)
	var d avDict
	var cl chapterList
	var samples, sr int64
	found := false
	for last := false; !last; {
		h, ok := b.bytesAt(at, 4)
		if !ok {
			return nil, unsure("flac: metadata past the end")
		}
		last = h[0]&0x80 != 0
		typ := h[0] & 0x7f
		size := int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])
		at += 4
		switch typ {
		case 0:
			if found || size != 34 {
				return nil, unsure("flac: bad STREAMINFO")
			}
			si, ok := b.bytesAt(at, 34)
			if !ok {
				return nil, unsure("flac: STREAMINFO past the end")
			}
			found = true
			sr = int64(binary.BigEndian.Uint32(si[10:14]) >> 12)
			samples = int64(binary.BigEndian.Uint64(si[13:21])>>24) & (1<<36 - 1)
		case 4:
			if !found {
				return nil, unsure("flac: comment before STREAMINFO")
			}
			if size > 16<<20 {
				return nil, unsure("flac: comment block over 16 MiB")
			}
			vc, ok := b.bytesAt(at, int(size))
			if !ok {
				return nil, unsure("flac: comment past the end")
			}
			vorbisComment(b, vc, &d, &cl)
			if _, ok := d.get("WAVEFORMATEXTENSIBLE_CHANNEL_MASK"); ok {
				return nil, unsure("flac: channel mask comment")
			}
		case 5:
			return nil, unsure("flac: CUESHEET")
		}
		at += size
	}
	if sr <= 0 || samples <= 0 {
		return nil, unsure("flac: no sample count")
	}
	durUS := tbToUS(samples, 1, sr)
	return result(durUS, "flac", &d, cl.finish(durUS, 0, true)), nil
}

// ---------------------------------------------------------------------------
// Ogg

var oggCRCTable = func() [256]uint32 {
	var t [256]uint32
	for i := range t {
		r := uint32(i) << 24
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04C11DB7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

type oggPage struct {
	flags   byte
	granule int64
	serial  uint32
	segs    []byte
	data    []byte
	size    int64
}

// oggMagic opens every Ogg page.
var oggMagic = []byte("OggS")

// parseOggPage reads a page at buf[0:] (CRC checked, as oggdec does).
func parseOggPage(buf []byte) (*oggPage, bool) {
	if len(buf) < 27 || string(buf[:4]) != "OggS" || buf[4] != 0 {
		return nil, false
	}
	n := int(buf[26])
	if len(buf) < 27+n {
		return nil, false
	}
	segs := buf[27 : 27+n]
	body := 0
	for _, s := range segs {
		body += int(s)
	}
	total := 27 + n + body
	if len(buf) < total {
		return nil, false
	}
	var crc uint32
	for i := 0; i < total; i++ {
		c := buf[i]
		if i >= 22 && i < 26 {
			c = 0
		}
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^c]
	}
	if crc != binary.LittleEndian.Uint32(buf[22:26]) {
		return nil, false
	}
	return &oggPage{
		flags: buf[5], granule: int64(binary.LittleEndian.Uint64(buf[6:14])),
		serial: binary.LittleEndian.Uint32(buf[14:18]), segs: segs, data: buf[27+n : total], size: int64(total),
	}, true
}

// packets splits a page into its packets: each with whether it ends on this page.
func (p *oggPage) packets() (pk [][]byte, complete []bool) {
	at := 0
	start := 0
	for i, s := range p.segs {
		at += int(s)
		if s < 255 {
			pk = append(pk, p.data[start:at])
			complete = append(complete, true)
			start = at
		} else if i == len(p.segs)-1 {
			pk = append(pk, p.data[start:at])
			complete = append(complete, false)
		}
	}
	return
}

func opusDuration(b []byte) int {
	if len(b) < 1 {
		return -1
	}
	toc := int(b[0])
	cfg, cnt := toc>>3, toc&3
	var fs int
	switch {
	case cfg < 12:
		fs = max(480, 960*(cfg&3))
	case cfg < 16:
		fs = 480 << (cfg & 1)
	default:
		fs = 120 << (cfg & 3)
	}
	n := 1
	if cnt == 3 {
		if len(b) < 2 {
			return -1
		}
		n = int(b[1] & 0x3f)
	} else if cnt != 0 {
		n = 2
	}
	return fs * n
}

// vorbisParse is libavcodec's vorbis_parser for packet durations.
type vorbisParse struct {
	bs        [2]int
	modeCount int
	modeMask  int
	prevMask  int
	modeBlock [64]int
	prevBlock int
}

func (v *vorbisParse) setup(id, setup []byte) bool {
	if len(id) < 30 || id[0] != 1 || id[29]&1 == 0 {
		return false
	}
	v.bs = [2]int{1 << (id[28] & 0xf), 1 << (id[28] >> 4)}
	if len(setup) < 7 || setup[0] != 5 {
		return false
	}
	rev := make([]byte, len(setup))
	for i := range setup {
		rev[i] = setup[len(setup)-1-i]
	}
	total := len(rev) * 8
	pos := 0
	bit := func() int {
		b := int(rev[pos>>3]>>(7-pos&7)) & 1
		pos++
		return b
	}
	bits := func(n int) int {
		x := 0
		for range n {
			x = x<<1 | bit()
		}
		return x
	}
	framing := 0
	for total-pos > 97 {
		if bit() == 1 {
			framing = pos
			break
		}
	}
	if framing == 0 {
		return false
	}
	modeCount, last, got := 0, 0, false
	for total-pos >= 97 {
		if bits(8) > 63 || bits(16) != 0 || bits(16) != 0 {
			break
		}
		pos++
		modeCount++
		if modeCount > 64 {
			break
		}
		save := pos
		if bits(6)+1 == modeCount {
			got, last = true, modeCount
		}
		pos = save
	}
	if !got || last > 63 {
		return false
	}
	v.modeCount = last
	l := 0
	for x := last - 1; x > 1; x >>= 1 { // av_log2(mode_count - 1)
		l++
	}
	v.modeMask = ((1 << (l + 1)) - 1) << 1
	v.prevMask = (v.modeMask | 1) + 1
	pos = framing
	for i := last - 1; i >= 0; i-- {
		pos += 40
		v.modeBlock[i] = bit()
	}
	v.prevBlock = v.bs[0]
	return true
}

func (v *vorbisParse) duration(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	if b[0]&1 != 0 {
		return 0
	}
	mode := 0
	if v.modeCount != 1 {
		mode = (int(b[0]) & v.modeMask) >> 1
	}
	if mode >= v.modeCount {
		return -1
	}
	prev := v.prevBlock
	if v.modeBlock[mode] != 0 {
		flag := 0
		if int(b[0])&v.prevMask != 0 {
			flag = 1
		}
		prev = v.bs[flag]
	}
	cur := v.bs[v.modeBlock[mode]]
	v.prevBlock = cur
	return (prev + cur) >> 2
}

func probeOgg(b *blockFile) (*probeResult, error) {
	if err := noID3Front(b, "ogg"); err != nil {
		return nil, err
	}
	// Read pages from the start until the first data page of the stream.
	var serial uint32
	codec := ""
	var headers [][]byte
	var partial []byte
	nHeaders := 0
	var firstData *oggPage
	var firstDataPackets [][]byte
	var firstDataComplete []bool
	at := int64(0)
	var cl chapterList
	var d avDict // the stream's tags: ffprobe shows them on the stream, not the format
	sr := int64(0)
	pageCount := 0
	for firstData == nil {
		if pageCount++; pageCount > 512 || at > 4<<20 {
			return nil, unsure("ogg: headers too long")
		}
		hdr, ok := b.bytesAt(at, 27)
		if !ok {
			return nil, unsure("ogg: no data page")
		}
		n := int(hdr[26])
		segs, ok := b.bytesAt(at+27, n)
		if !ok {
			return nil, unsure("ogg: truncated page")
		}
		body := 0
		for _, s := range segs {
			body += int(s)
		}
		buf, ok := b.bytesAt(at, 27+n+body)
		if !ok {
			return nil, unsure("ogg: truncated page")
		}
		pg, ok := parseOggPage(buf)
		if !ok {
			return nil, unsure("ogg: bad page at start")
		}
		at += pg.size
		if pg.flags&2 != 0 { // BOS
			if codec != "" {
				return nil, unsure("ogg: several streams")
			}
			serial = pg.serial
		} else if pg.serial != serial {
			return nil, unsure("ogg: several streams")
		}
		pk, complete := pg.packets()
		if codec != "" && len(headers) >= nHeaders {
			if pg.flags&1 != 0 { // continued packet on the first data page
				return nil, unsure("ogg: first data page continues a packet")
			}
			firstData, firstDataPackets, firstDataComplete = pg, pk, complete
			break
		}
		for i, p := range pk {
			cont := partial != nil
			if cont {
				p = append(partial, p...) // owned: partial was a copy
				partial = nil
			}
			if !complete[i] {
				if cont {
					partial = p // grows in place, not copied per page
				} else {
					partial = append([]byte(nil), p...)
				}
				continue
			}
			if codec == "" {
				switch {
				case len(p) >= 7 && p[0] == 1 && string(p[1:7]) == "vorbis":
					codec, nHeaders = "vorbis", 3
					if len(p) >= 16 {
						sr = int64(binary.LittleEndian.Uint32(p[12:16]))
					}
				case len(p) >= 19 && string(p[:8]) == "OpusHead":
					if p[8]&0xf0 != 0 {
						return nil, unsure("ogg: opus version")
					}
					codec, nHeaders, sr = "opus", 2, 48000
				case len(p) >= 13+34 && p[0] == 0x7f && string(p[1:5]) == "FLAC":
					if p[5] != 1 {
						return nil, unsure("ogg: flac mapping version")
					}
					codec = "flac"
					nHeaders = 1 + int(binary.BigEndian.Uint16(p[7:9]))
					sr = int64(binary.BigEndian.Uint32(p[13+4+10:13+4+14]) >> 12)
				default:
					return nil, unsure("ogg: not vorbis, opus or flac")
				}
				headers = append(headers, p)
				continue
			}
			if len(headers) >= nHeaders {
				return nil, unsure("ogg: data on a header page")
			}
			headers = append(headers, p)
			switch codec {
			case "vorbis":
				if len(headers) == 2 && len(p) > 8 && p[0] == 3 {
					vorbisComment(b, p[7:len(p)-1], &d, &cl)
				}
			case "opus":
				if len(headers) == 2 {
					if len(p) < 8 || string(p[:8]) != "OpusTags" {
						return nil, unsure("ogg: no OpusTags")
					}
					vorbisComment(b, p[8:], &d, &cl)
				}
			case "flac":
				if len(p) > 0 && p[0]&0x7f == 4 {
					vorbisComment(b, p[4:], &d, &cl)
				}
			}
		}
	}
	if sr <= 0 {
		return nil, unsure("ogg: no sample rate")
	}
	// The stream's start (the first data page's granule less its packets).
	var startTS int64
	switch codec {
	case "vorbis":
		var vp vorbisParse
		if len(headers) < 3 || !vp.setup(headers[0], headers[2]) {
			return nil, unsure("ogg: vorbis setup header")
		}
		dur := 0
		for i, p := range firstDataPackets {
			if !firstDataComplete[i] {
				break
			}
			x := vp.duration(p)
			if x < 0 {
				dur = int(firstData.granule)
				break
			}
			dur += x
		}
		if firstData.granule < 0 {
			return nil, unsure("ogg: no granule on the first data page")
		}
		lp := firstData.granule - int64(dur)
		if firstData.granule == 0 && dur != 0 {
			lp = 0
		}
		startTS = max(lp, 0)
	case "opus":
		dur := 0
		for i, p := range firstDataPackets {
			if !firstDataComplete[i] {
				break
			}
			if x := opusDuration(p); x > 0 {
				dur += x
			} else if i == 0 {
				return nil, unsure("ogg: bad opus packet")
			}
		}
		startTS = firstData.granule - int64(dur)
	case "flac":
		startTS = 0
	}
	// The last page with a granule (ogg_get_length).
	tailStart := max(b.size-65307, 0)
	tail, ok := b.bytesAt(tailStart, int(b.size-tailStart))
	if !ok {
		return nil, unsure("ogg: tail")
	}
	lastGranule := int64(-1)
	for i := 0; i+27 <= len(tail); {
		j := bytes.Index(tail[i:], oggMagic)
		if j < 0 {
			break
		}
		i += j
		pg, ok := parseOggPage(tail[i:])
		if !ok {
			i++
			continue
		}
		if pg.serial == serial && pg.granule != -1 && pg.granule != 0 {
			lastGranule = pg.granule
		}
		i += int(pg.size)
	}
	if lastGranule <= 0 {
		return nil, unsure("ogg: no granule at the end")
	}
	var stDur, stStart int64
	switch codec {
	case "vorbis":
		stDur, stStart = lastGranule-startTS, startTS
	case "opus":
		stDur, stStart = lastGranule, startTS
	case "flac":
		stDur, stStart = lastGranule, 0
	}
	start1 := tbToUS(stStart, 1, sr)
	durUS := tbToUS(stDur, 1, sr)
	return result(durUS, codec, nil, cl.finish(durUS, start1, true)), nil
}
