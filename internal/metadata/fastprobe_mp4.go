package metadata

// The in-process probe's MP4/M4B/M4A reader (fastprobe.go), as FFmpeg 7.1's
// mov.c reads it: the stream durations (mdhd, stts, edit list) that
// update_stream_timings combines, the Nero chpl and QuickTime chapter-track
// chapters, and the iTunes/QuickTime metadata keys. A fragmented or compressed
// movie, an unknown handler or sample entry is ffprobe's.

import (
	"encoding/binary"
	"strconv"
	"unicode/utf16"
)

type mp4Elst struct {
	dur  int64
	time int64
}

type mp4Leaf struct{ off, size int64 }

type mp4Track struct {
	id          uint32
	handler     string
	timescale   int64
	mdhdDur     int64
	sttsSum     int64
	stts        [][2]uint32
	elst        []mp4Elst
	hasElst     bool
	stsd        string
	oti         int
	hasEsds     bool
	chap        []uint32
	stsc, stsz  mp4Leaf
	stco        mp4Leaf
	co64        bool
	stz2        bool
	syncOrCtts  bool // an stss or ctts: keyframes or a DTS shift mov_fix_index heeds
	sttsEntries int
}

type mp4State struct {
	b           *blockFile
	tracks      []*mp4Track
	inTrak      bool
	itunes      bool
	mvTimescale int64
	dict        avDict
	chapters    chapterList
	foundMoov   bool
	err         error
	mdta        bool
	metaKeys    []string
	boxes       int
}

func probeMP4(b *blockFile) (*probeResult, error) {
	s := &mp4State{b: b, mvTimescale: 0}
	s.walk(0, b.size, "root", 0)
	if s.err != nil {
		return nil, s.err
	}
	if !s.foundMoov {
		return nil, unsure("mp4: no moov")
	}
	// The streams' durations (mov_read_trak: mdhd, min'd with the stts sum,
	// then mov_fix_index's edit list end) and start times.
	const i64min, i64max = -1 << 63, 1<<63 - 1
	start, startText := int64(i64max), int64(i64max)
	end, endText := int64(i64min), int64(i64min)
	dur, durText := int64(i64min), int64(i64min)
	audio := -1
	// The chapter tracks' ids, and each id's first track: looked up per track
	// and per id, so a file of many tracks and ids stays linear.
	chapIDs := s.chapterIDs()
	isChapID := make(map[uint32]bool, len(chapIDs))
	for _, id := range chapIDs {
		isChapID[id] = true
	}
	trackByID := make(map[uint32]*mp4Track, len(s.tracks))
	for _, t := range s.tracks {
		if _, ok := trackByID[t.id]; !ok {
			trackByID[t.id] = t
		}
	}
	for i, t := range s.tracks {
		if t.timescale <= 0 {
			return nil, unsure("mp4: track without a timescale")
		}
		isText := false
		switch t.handler {
		case "soun":
			if audio < 0 {
				audio = i
			}
		case "text", "sbtl", "subt":
			isText = true
		case "vide":
		default:
			return nil, unsure("mp4: track handler " + strconv.Quote(t.handler))
		}
		if isChapID[t.id] && t.handler != "vide" {
			isText = true // mov_read_chapters makes it a data stream
		}
		sd := t.mdhdDur
		if t.sttsSum > 0 {
			sd = min(sd, t.sttsSum)
		}
		var st int64
		hasStart := t.handler == "soun" || t.handler == "vide"
		if t.hasElst && len(t.elst) > 0 && t.sttsEntries > 0 {
			if s.mvTimescale == 0 {
				return nil, unsure("mp4: edit list with no movie timescale")
			}
			var sum, empty int64
			nonEmpty := false
			for _, e := range t.elst {
				ed := rescale(e.dur, t.timescale, s.mvTimescale)
				sum += ed
				if !nonEmpty && e.time == -1 {
					empty += ed
				} else {
					nonEmpty = true
				}
			}
			sd = min(sd, sum)
			st = empty
			hasStart = true
		}
		if hasStart {
			s1 := tbToUS(st, 1, t.timescale)
			e1 := tbToUS(sd, 1, t.timescale) + s1
			if isText {
				startText, endText = min(startText, s1), max(endText, e1)
			} else {
				start, end = min(start, s1), max(end, e1)
			}
		}
		d1 := tbToUS(sd, 1, t.timescale)
		if isText {
			durText = max(durText, d1)
		} else {
			dur = max(dur, d1)
		}
	}
	if audio < 0 {
		return nil, unsure("mp4: no audio track")
	}
	if start == i64max || (start > startText && start-startText < 1000000) {
		start = startText
	}
	if end == i64min || (end < endText && endText-end < 1000000) {
		end = endText
	}
	if dur == i64min || (dur < durText && durText-dur < 1000000) {
		dur = durText
	}
	if start != i64max && end != i64min && end >= start {
		dur = max(dur, end-start)
	}
	durUS := int64(0)
	if dur > 0 {
		durUS = dur
	}
	codec, err := mp4Codec(s.tracks[audio])
	if err != nil {
		return nil, err
	}
	// Chapter tracks (mov_read_chapters), after chpl.
	for _, id := range chapIDs {
		t := trackByID[id]
		if t == nil || t.handler == "vide" {
			continue
		}
		if err := s.readChapterTrack(t); err != nil {
			return nil, err
		}
	}
	startUS := int64(0)
	hasStart := start != i64max
	if hasStart {
		startUS = start
	}
	return result(durUS, codec, &s.dict, s.chapters.finish(durUS, startUS, hasStart)), nil
}

func (s *mp4State) chapterIDs() []uint32 {
	// c->chapter_tracks is the last tref/chap read.
	var ids []uint32
	for _, t := range s.tracks {
		if t.chap != nil {
			ids = t.chap
		}
	}
	return ids
}

func mp4Codec(t *mp4Track) (string, error) {
	switch t.stsd {
	case "mp4a":
		if !t.hasEsds {
			return "aac", nil
		}
		switch t.oti {
		case 0x40, 0x66, 0x67, 0x68:
			return "aac", nil
		case 0x69, 0x6B:
			return "mp3", nil
		case 0xA5:
			return "ac3", nil
		case 0xA6:
			return "eac3", nil
		case 0xAD:
			return "opus", nil
		case 0xDD:
			return "vorbis", nil
		}
		return "", unsure("mp4: mp4a object type " + strconv.Itoa(t.oti))
	case "alac":
		return "alac", nil
	case "ac-3":
		return "ac3", nil
	case "ec-3":
		return "eac3", nil
	case "Opus":
		return "opus", nil
	case "fLaC":
		return "flac", nil
	case ".mp3":
		return "mp3", nil
	}
	return "", unsure("mp4: sample entry " + strconv.Quote(t.stsd))
}

func (s *mp4State) cur() *mp4Track {
	if len(s.tracks) == 0 {
		return nil
	}
	return s.tracks[len(s.tracks)-1]
}

// walk is mov_read_default over [start, start+size) inside parent.
func (s *mp4State) walk(start, size int64, parent string, depth int) {
	if depth > 10 || s.err != nil {
		return
	}
	var total int64
	at := start
	for total <= size-8 {
		if s.boxes++; s.boxes > 100000 {
			s.err = unsure("mp4: too many boxes")
			return
		}
		h, ok := s.b.bytesAt(at, 8)
		if !ok {
			return
		}
		asize := int64(binary.BigEndian.Uint32(h[:4]))
		typ := string(h[4:8])
		if parent != "root" && parent != "moov" && (typ == "trak" || typ == "mdat") {
			return
		}
		total += 8
		body := at + 8
		if asize == 1 && total+8 <= size {
			l, ok := s.b.bytesAt(at+8, 8)
			if !ok {
				return
			}
			asize = int64(binary.BigEndian.Uint64(l)) - 8
			total += 8
			body += 8
		}
		if asize == 0 {
			asize = size - total + 8
		}
		if asize < 0 {
			return
		}
		asize -= 8
		if asize < 0 {
			return
		}
		asize = min(asize, size-total)
		s.atom(typ, body, asize, parent, depth)
		if s.err != nil {
			return
		}
		// mov_read_default stops after moov and mdat only on a stream it cannot
		// seek or at the end of the file: a file's later root boxes still count.
		total += asize
		at = body + asize
	}
}

func (s *mp4State) read(off, n int64) []byte {
	if n < 0 || n > 16<<20 {
		s.err = unsure("mp4: box too large to read")
		return nil
	}
	buf, ok := s.b.bytesAt(off, int(n))
	if !ok {
		s.err = unsure("mp4: box past the end of the file")
		return nil
	}
	return buf
}

func (s *mp4State) atom(typ string, body, size int64, parent string, depth int) {
	t := s.cur()
	switch typ {
	case "moov":
		// mov_read_moov: a second moov is skipped; found once parsed.
		if !s.foundMoov {
			s.walk(body, size, typ, depth+1)
			s.foundMoov = true
		}
	case "moof", "mvex", "trex", "trun", "tfhd", "sidx":
		s.err = unsure("mp4: fragmented")
	case "cmov":
		s.err = unsure("mp4: compressed moov")
	case "trak":
		s.tracks = append(s.tracks, &mp4Track{})
		s.inTrak = true
		s.walk(body, size, typ, depth+1)
		s.inTrak = false
	case "mdia", "minf", "stbl", "dinf", "edts", "udta", "tref", "schi", "sinf", "traf":
		s.walk(body, size, typ, depth+1)
	case "ilst":
		s.itunes = true
		s.walk(body, size, typ, depth+1)
		s.itunes = false
	case "meta":
		// mov_read_meta: look for hdlr four bytes at a time, then read from it.
		for off := int64(0); size-off > 8; off += 4 {
			tb := s.read(body+off+4, 4)
			if tb == nil {
				return
			}
			if string(tb) == "hdlr" {
				s.walk(body+off, size-off, typ, depth+1)
				return
			}
		}
	case "mvhd":
		v := s.read(body, min(size, 32))
		if len(v) < 24 {
			return
		}
		if v[0] == 1 {
			s.mvTimescale = int64(binary.BigEndian.Uint32(v[20:24]))
		} else {
			s.mvTimescale = int64(binary.BigEndian.Uint32(v[12:16]))
		}
		if s.mvTimescale <= 0 {
			s.mvTimescale = 1
		}
	case "tkhd":
		if t == nil {
			return
		}
		v := s.read(body, min(size, 24))
		if len(v) < 24 {
			return
		}
		if v[0] == 1 {
			t.id = binary.BigEndian.Uint32(v[20:24])
		} else {
			t.id = binary.BigEndian.Uint32(v[12:16])
		}
	case "mdhd":
		if t == nil {
			return
		}
		v := s.read(body, min(size, 36))
		if len(v) < 24 {
			return
		}
		if v[0] == 1 {
			t.timescale = int64(binary.BigEndian.Uint32(v[20:24]))
			d := binary.BigEndian.Uint64(v[24:32])
			if d == 1<<64-1 {
				d = 0
			}
			t.mdhdDur = int64(d)
		} else {
			t.timescale = int64(binary.BigEndian.Uint32(v[12:16]))
			d := binary.BigEndian.Uint32(v[16:20])
			if d == 1<<32-1 {
				d = 0
			}
			t.mdhdDur = int64(d)
		}
		if t.timescale <= 0 {
			t.timescale = 1
		}
	case "hdlr":
		v := s.read(body, min(size, 12))
		if len(v) < 12 {
			return
		}
		sub := string(v[8:12])
		if !s.inTrak {
			if sub == "mdta" {
				s.mdta = true
			}
			return
		}
		if t != nil && parent == "mdia" {
			t.handler = sub
		}
	case "stsd":
		if t == nil {
			return
		}
		v := s.read(body, min(size, 16))
		if len(v) < 16 {
			return
		}
		t.stsd = string(v[12:16])
		entrySize := int64(binary.BigEndian.Uint32(v[8:12]))
		if t.stsd == "mp4a" && entrySize > 36 {
			s.readEsds(t, body+8, min(entrySize, size-8))
		}
	case "stts":
		if t == nil {
			return
		}
		v := s.read(body, min(size, 8))
		if len(v) < 8 {
			return
		}
		n := int64(binary.BigEndian.Uint32(v[4:8]))
		if n*8 > size-8 {
			n = (size - 8) / 8
		}
		e := s.read(body+8, n*8)
		if e == nil {
			return
		}
		t.sttsEntries = int(n)
		t.stts = t.stts[:0]
		var sum int64
		for i := int64(0); i < n; i++ {
			c := binary.BigEndian.Uint32(e[i*8:])
			d := binary.BigEndian.Uint32(e[i*8+4:])
			if d > 1<<31-1 {
				s.err = unsure("mp4: stts delta over the max")
				return
			}
			sum += int64(c) * int64(d)
			t.stts = append(t.stts, [2]uint32{c, d})
		}
		t.sttsSum = sum
	case "elst":
		if t == nil {
			return
		}
		v := s.read(body, min(size, 8))
		if len(v) < 8 {
			return
		}
		es := int64(12)
		if v[0] == 1 {
			es = 20
		}
		n := int64(binary.BigEndian.Uint32(v[4:8]))
		if n*es != size-8 {
			n = (size - 8) / es
		}
		if n == 0 {
			return
		}
		e := s.read(body+8, n*es)
		if e == nil {
			return
		}
		t.elst = t.elst[:0]
		t.hasElst = true
		for i := int64(0); i < n; i++ {
			p := e[i*es:]
			if es == 20 {
				t.elst = append(t.elst, mp4Elst{int64(binary.BigEndian.Uint64(p)), int64(binary.BigEndian.Uint64(p[8:]))})
			} else {
				t.elst = append(t.elst, mp4Elst{int64(binary.BigEndian.Uint32(p)), int64(int32(binary.BigEndian.Uint32(p[4:])))})
			}
		}
	case "stsc":
		if t != nil {
			t.stsc = mp4Leaf{body, size}
		}
	case "stsz", "stz2":
		if t != nil {
			t.stsz = mp4Leaf{body, size}
			t.stz2 = typ == "stz2"
		}
	case "stss", "ctts":
		if t != nil {
			t.syncOrCtts = true
		}
	case "stco", "co64":
		if t != nil {
			t.stco = mp4Leaf{body, size}
			t.co64 = typ == "co64"
		}
	case "chap":
		if t == nil {
			return
		}
		v := s.read(body, size-size%4)
		if v == nil {
			return
		}
		t.chap = []uint32{}
		for i := 0; i+4 <= len(v); i += 4 {
			t.chap = append(t.chap, binary.BigEndian.Uint32(v[i:]))
		}
	case "chpl":
		s.readChpl(body, size)
	case "----":
		s.readCustom(body, size)
	case "keys":
		if s.mdta && parent == "meta" && s.metaKeys == nil {
			s.readKeys(body, size)
		} else if parent == "udta" || parent == "ilst" {
			s.udtaString(typ, body, size)
		}
	default:
		if parent == "udta" || parent == "ilst" {
			s.udtaString(typ, body, size)
		}
	}
}

func (s *mp4State) readEsds(t *mp4Track, entry, size int64) {
	v := s.read(entry, min(size, 4096))
	if v == nil {
		return
	}
	// The sound sample entry: 8 header + 28 body (version at 16), v1 +16, v2 +36.
	if len(v) < 36 {
		return
	}
	off := 36
	switch binary.BigEndian.Uint16(v[16:18]) {
	case 1:
		off += 16
	case 2:
		off += 36
	}
	var find func(b []byte) []byte
	find = func(b []byte) []byte {
		for i := 0; i+8 <= len(b); {
			sz := int(binary.BigEndian.Uint32(b[i:]))
			if sz < 8 || i+sz > len(b) {
				return nil
			}
			switch string(b[i+4 : i+8]) {
			case "esds":
				return b[i+8 : i+sz]
			case "wave":
				if r := find(b[i+8 : i+sz]); r != nil {
					return r
				}
			}
			i += sz
		}
		return nil
	}
	if off > len(v) {
		return
	}
	e := find(v[off:])
	if len(e) < 4 {
		return
	}
	t.hasEsds = true
	c := &cursor{b: e[4:]}
	descLen := func() int {
		n := 0
		for i := 0; i < 4; i++ {
			x := c.r8()
			n = n<<7 | int(x&0x7f)
			if x&0x80 == 0 {
				break
			}
		}
		return n
	}
	if c.r8() == 0x03 {
		descLen()
		c.rb16()
		flags := c.r8()
		if flags&0x80 != 0 {
			c.rb16()
		}
		if flags&0x40 != 0 {
			c.pos += int(c.r8())
		}
		if flags&0x20 != 0 {
			c.rb16()
		}
	} else {
		c.rb16()
	}
	if c.r8() == 0x04 {
		descLen()
		t.oti = int(c.r8())
	}
}

func (s *mp4State) readChpl(body, size int64) {
	if size-5 < 0 {
		return
	}
	v := s.read(body, size)
	if v == nil {
		return
	}
	c := &cursor{b: v}
	left := size - 5
	version := c.r8()
	c.pos += 3
	if version != 0 {
		c.pos += 4
	}
	n := int(c.r8())
	for i := 0; i < n; i++ {
		if left < 9 {
			return
		}
		hi, lo := c.rb32(), c.rb32()
		startV := int64(uint64(hi)<<32 | uint64(lo))
		l := int(c.r8())
		if left -= int64(9 + l); left < 0 {
			return
		}
		if c.pos+l > len(c.b) {
			s.err = unsure("mp4: chpl past its box")
			return
		}
		title := string(c.b[c.pos : c.pos+l])
		c.pos += l
		s.chapters.add(int64(i), 1, 10000000, startV, 0, false, title)
	}
}

func (s *mp4State) readCustom(body, size int64) {
	if len(s.tracks) < 1 {
		return
	}
	v := s.read(body, size)
	if v == nil {
		return
	}
	var key, val, mean []byte
	var haveKey, haveVal, haveMean bool
	c := &cursor{b: v}
	for i := 0; i < 3; i++ {
		if int64(len(v)-c.pos) <= 12 {
			break
		}
		l := c.rb32()
		tag := string(v[c.pos : c.pos+4])
		c.pos += 4 + 4
		if l < 12 || int(l-12) > len(v)-c.pos {
			break
		}
		l -= 12
		var p *[]byte
		var have *bool
		switch {
		case tag == "mean":
			p, have = &mean, &haveMean
		case tag == "name":
			p, have = &key, &haveKey
		case tag == "data" && l > 4:
			c.pos += 4
			l -= 4
			p, have = &val, &haveVal
		}
		if p == nil || *have {
			break
		}
		*p = v[c.pos : c.pos+int(l)]
		*have = true
		c.pos += int(l)
	}
	if haveMean && haveKey && haveVal && cutNUL(string(key)) != "cdec" {
		s.dict.set(cutNUL(string(key)), cutNUL(string(val)), 0)
	}
}

var movKeys = map[string]string{
	"aART": "album_artist", "apID": "account_id", "catg": "category", "cprt": "copyright",
	"desc": "description", "keyw": "keywords", "ldes": "synopsis", "manu": "make", "modl": "model",
	"purd": "purchase_date", "soaa": "sort_album_artist", "soal": "sort_album", "soar": "sort_artist",
	"soco": "sort_composer", "sonm": "sort_name", "sosn": "sort_show", "tven": "episode_id",
	"tvnn": "network", "tvsh": "show",
	"\xa9ART": "artist", "\xa9PRD": "producer", "\xa9alb": "album", "\xa9aut": "artist",
	"\xa9chp": "chapter", "\xa9cmt": "comment", "\xa9com": "composer", "\xa9cpy": "copyright",
	"\xa9day": "date", "\xa9dir": "director", "\xa9dis": "disclaimer", "\xa9ed1": "edit_date",
	"\xa9enc": "encoder", "\xa9fmt": "original_format", "\xa9gen": "genre", "\xa9grp": "grouping",
	"\xa9hst": "host_computer", "\xa9inf": "comment", "\xa9lyr": "lyrics", "\xa9mak": "make",
	"\xa9mod": "model", "\xa9nam": "title", "\xa9ope": "original_artist", "\xa9prd": "producer",
	"\xa9prf": "performers", "\xa9req": "playback_requirements", "\xa9src": "original_source",
	"\xa9st3": "subtitle", "\xa9swr": "encoder", "\xa9too": "encoder", "\xa9trk": "track",
	"\xa9url": "URL", "\xa9wrn": "warning", "\xa9wrt": "composer", "\xa9xyz": "location",
	// parsed specially:
	"gnre": "genre", "trkn": "track", "disk": "disc", "akID": "account_type", "cpil": "compilation",
	"egid": "episode_uid", "hdvd": "hd_video", "pcst": "podcast", "pgap": "gapless_playback",
	"rtng": "rating", "stik": "media_type", "tves": "episode_sort", "tvsn": "season_number",
}

// udtaString is mov_read_udta_string for the keys that are text (and gnre).
func (s *mp4State) udtaString(typ string, body, size int64) {
	key, ok := movKeys[typ]
	if !ok && s.itunes && s.mdta && s.metaKeys != nil {
		idx := binary.BigEndian.Uint32([]byte(typ))
		if idx > 0 && int(idx) < len(s.metaKeys) && s.metaKeys[idx] != "" {
			key, ok = s.metaKeys[idx], true
			if key == "com.apple.quicktime.artwork" {
				return
			}
		}
	}
	switch typ {
	case "@PRM", "@PRQ", "FIRM", "XMP_", "HMMT", "loci":
		return
	}
	if !ok {
		return
	}
	v := s.read(body, min(size, 1<<20))
	if v == nil {
		return
	}
	c := &cursor{b: v}
	var dataType uint32
	var strSize int64
	if s.itunes {
		if size <= 8 {
			return
		}
		ds := int64(c.rb32())
		tg := string(v[4:8])
		c.pos = 8
		if tg != "data" || ds > size || ds < 16 {
			return
		}
		dataType = c.rb32()
		c.rb32()
		strSize = ds - 16
	} else if size > 4 {
		strSize = int64(c.rb16())
		if strSize > size {
			s.err = unsure("mp4: raw udta string")
			return
		}
		lang := c.rb16()
		if strSize > 0 && (lang < 0x400 || lang == 0x7fff) {
			// Mac-encoded: same as ASCII unless a byte is >= 0x80.
			for _, x := range v[4:min(int64(len(v)), 4+strSize)] {
				if x >= 0x80 {
					s.err = unsure("mp4: Mac-encoded udta string")
					return
				}
			}
		}
		if lang >= 0x400 && lang != 0x7fff {
			s.err = unsure("mp4: udta string with an ISO language")
			return
		}
	} else {
		strSize = size
	}
	if int64(c.pos)+strSize > int64(len(v)) {
		s.err = unsure("mp4: udta string past its box")
		return
	}
	data := v[c.pos : int64(c.pos)+strSize]
	switch typ {
	case "gnre":
		if len(data) >= 2 {
			g := int(data[1])
			if g >= 1 && g <= 191 {
				s.dict.set(key, id3v1Genres[g-1], 0)
			}
		}
		return
	case "trkn", "disk", "akID", "cpil", "egid", "hdvd", "pcst", "pgap", "rtng", "stik", "tves", "tvsn":
		return // numbers, not keys applyProbe reads
	}
	var str string
	switch {
	case dataType == 3 || s.itunes && dataType == 0: // mov_read_mac_string
		for _, x := range data {
			if x >= 0x80 {
				s.err = unsure("mp4: Mac-encoded item")
				return
			}
		}
		str = string(data)
	case dataType == 21 || dataType == 22 || dataType == 23:
		s.err = unsure("mp4: numeric text item")
		return
	case dataType > 1 && dataType != 4:
		return
	default:
		str = string(data)
	}
	s.dict.set(key, cutNUL(str), 0)
}

// maxChapterSamples bounds a chapter track's samples: more is a crafted
// table, left to ffprobe rather than walked.
const maxChapterSamples = 1 << 16

// readChapterTrack is mov_read_chapters for a text track.
func (s *mp4State) readChapterTrack(t *mp4Track) error {
	if len(t.elst) > 1 || (len(t.elst) == 1 && t.elst[0].time != 0) {
		return unsure("mp4: chapter track with an edit list")
	}
	if t.stz2 || t.co64 && t.stco.size == 0 {
		return unsure("mp4: chapter track stz2")
	}
	sz := s.read(t.stsz.off, t.stsz.size)
	sc := s.read(t.stsc.off, t.stsc.size)
	co := s.read(t.stco.off, t.stco.size)
	if s.err != nil || len(sz) < 12 || len(sc) < 8 || len(co) < 8 {
		return unsure("mp4: chapter track tables")
	}
	fixed := binary.BigEndian.Uint32(sz[4:8])
	nSamples := int(binary.BigEndian.Uint32(sz[8:12]))
	nChunks := int(binary.BigEndian.Uint32(co[4:8]))
	nStsc := int(binary.BigEndian.Uint32(sc[4:8]))
	entry := 4
	if t.co64 {
		entry = 8
	}
	switch {
	case nSamples > maxChapterSamples:
		return unsure("mp4: chapter track with too many samples")
	// FFmpeg reads a table's entries on past its box: not modelled. Counts
	// inside the tables also bound the walk below.
	case fixed == 0 && nSamples > (len(sz)-12)/4, nChunks > (len(co)-8)/entry, nStsc > (len(sc)-8)/12:
		return unsure("mp4: chapter track tables")
	// A track FFmpeg drops as a broken header (mov_read_trak).
	case nChunks > 0 && (nStsc == 0 || len(t.stts) == 0 || fixed == 0 && nSamples == 0), nChunks == 0 && nSamples > 0:
		return unsure("mp4: chapter track tables")
	}
	sizeOf := func(i int) int64 {
		if fixed != 0 {
			return int64(fixed)
		}
		return int64(binary.BigEndian.Uint32(sz[12+i*4:]))
	}
	chunkOff := func(i int) int64 {
		if t.co64 {
			return int64(binary.BigEndian.Uint64(co[8+i*8:]))
		}
		return int64(binary.BigEndian.Uint32(co[8+i*4:]))
	}
	// stsc: FFmpeg repairs entries out of order or with no samples
	// (mov_read_stsc) and gives chunks before the first entry its count; only
	// a table that needs neither is read.
	type sce struct{ first, per int }
	stsc := make([]sce, nStsc)
	for i := range stsc {
		e := sce{int(binary.BigEndian.Uint32(sc[8+i*12:])), int(binary.BigEndian.Uint32(sc[8+i*12+4:]))}
		if e.per < 1 || binary.BigEndian.Uint32(sc[8+i*12+8:]) < 1 || i == 0 && e.first != 1 || i > 0 && e.first <= stsc[i-1].first {
			return unsure("mp4: chapter track stsc")
		}
		stsc[i] = e
	}
	// Sample positions and timestamps.
	type sample struct {
		pos, size, ts int64
	}
	var samples []sample
	si := 0
	var ts int64
	sttsIdx, sttsLeft := 0, uint32(0)
	if len(t.stts) > 0 {
		sttsLeft = t.stts[0][0]
	}
	nextDelta := func() int64 {
		for sttsIdx < len(t.stts) && sttsLeft == 0 {
			sttsIdx++
			if sttsIdx < len(t.stts) {
				sttsLeft = t.stts[sttsIdx][0]
			}
		}
		if sttsIdx >= len(t.stts) {
			return 0
		}
		sttsLeft--
		return int64(t.stts[sttsIdx][1])
	}
	cur := 0 // the stsc entry for chunk ch
	for ch := 0; ch < nChunks && si < nSamples; ch++ {
		for cur+1 < len(stsc) && stsc[cur+1].first == ch+1 {
			cur++
		}
		off := chunkOff(ch)
		for k := 0; k < stsc[cur].per && si < nSamples; k++ {
			size := sizeOf(si)
			if off < 0 {
				return unsure("mp4: chapter track tables")
			}
			samples = append(samples, sample{off, size, ts})
			ts += nextDelta()
			off += size
			si++
		}
	}
	stDur := t.mdhdDur
	if t.sttsSum > 0 {
		stDur = min(stDur, t.sttsSum)
	}
	if len(t.elst) == 1 && len(samples) > 0 {
		// mov_fix_index for the one edit (media time 0, checked above): it keeps
		// the samples up to the first that reaches the edit's end and drops the
		// rest, so a trailing sample at the end (zero length, as muxers write
		// after the last chapter) is never a chapter, and an edit of length 0
		// keeps only the first. The stream's duration ends at the edit's end.
		if s.mvTimescale == 0 || t.syncOrCtts {
			return unsure("mp4: chapter track edit list not modelled")
		}
		d := rescale(t.elst[0].dur, t.timescale, s.mvTimescale)
		if d < 0 { // get_edit_list_entry: past INT64_MAX is 0
			d = 0
		}
		// A sample ends where the next starts; the last one's length is the
		// edit's, so it always reaches the end.
		for i := 0; i+1 < len(samples); i++ {
			if samples[i+1].ts >= d {
				samples = samples[:i+1]
				break
			}
		}
		stDur = min(stDur, d)
	}
	for i, smp := range samples {
		end, hasEnd := stDur, true
		if i+1 < len(samples) {
			end = samples[i+1].ts
		}
		if end < smp.ts {
			hasEnd = false
		}
		hdr, ok := s.b.bytesAt(smp.pos, 2)
		if !ok {
			return unsure("mp4: chapter sample past the end")
		}
		l := int64(binary.BigEndian.Uint16(hdr))
		if l > smp.size-2 {
			continue
		}
		var title string
		if l > 0 {
			raw, ok := s.b.bytesAt(smp.pos+2, int(l))
			if !ok {
				return unsure("mp4: chapter sample past the end")
			}
			if l >= 2 && (raw[0] == 0xfe && raw[1] == 0xff || raw[0] == 0xff && raw[1] == 0xfe) {
				var u []uint16
				for j := 2; j+1 < len(raw); j += 2 {
					var w uint16
					if raw[0] == 0xfe {
						w = uint16(raw[j])<<8 | uint16(raw[j+1])
					} else {
						w = uint16(raw[j+1])<<8 | uint16(raw[j])
					}
					if w == 0 {
						break
					}
					u = append(u, w)
				}
				title = string(utf16.Decode(u))
				if l%2 == 1 {
					return unsure("mp4: odd UTF-16 chapter title")
				}
			} else {
				title = cutNUL(string(raw))
			}
		}
		s.chapters.add(int64(i), 1, t.timescale, smp.ts, end, hasEnd, title)
	}
	return nil
}

// readKeys is mov_read_keys.
func (s *mp4State) readKeys(body, size int64) {
	if size < 8 {
		return
	}
	v := s.read(body, size)
	if v == nil {
		return
	}
	count := binary.BigEndian.Uint32(v[4:8])
	if count > 1<<16 {
		s.err = unsure("mp4: too many keys")
		return
	}
	keys := make([]string, count+1)
	left := size - 8
	at := int64(8)
	for i := 1; i <= int(count); i++ {
		if at+8 > int64(len(v)) {
			s.err = unsure("mp4: keys past its box")
			return
		}
		ks := int64(binary.BigEndian.Uint32(v[at:]))
		typ := string(v[at+4 : at+8])
		if ks < 8 || ks > left {
			s.err = unsure("mp4: invalid key size")
			return
		}
		left -= ks
		if typ == "mdta" {
			keys[i] = cutNUL(string(v[at+8 : at+ks]))
		}
		at += ks
	}
	s.metaKeys = keys
}
