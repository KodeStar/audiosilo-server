package metadata

// The in-process probe's Matroska audio (.mka) reader (fastprobe.go), as FFmpeg
// 7.1's matroskadec.c reads it: Segment Info Duration x TimecodeScale, the
// Chapters element, global Tags (with TargetType prefixes and language
// variants), the first audio track's codec. It reads up to the first Cluster,
// plus what the SeekHead points at after it.

import (
	"encoding/binary"
	"math"
	"strings"
)

const (
	mkvEBML     = 0x1A45DFA3
	mkvSegment  = 0x18538067
	mkvSeekHead = 0x114D9B74
	mkvInfo     = 0x1549A966
	mkvTracks   = 0x1654AE6B
	mkvChapters = 0x1043A770
	mkvTags     = 0x1254C367
	mkvCluster  = 0x1F43B675
	mkvCues     = 0x1C53BB6B
)

// ebmlElem is one element: id, where its payload starts, its size (-1 unknown).
type ebmlElem struct {
	id   uint32
	data int64
	size int64
}

func readVint(b []byte, keepMarker bool) (uint64, int, bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n := 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 || len(b) < n {
		return 0, 0, false
	}
	v := uint64(b[0])
	if !keepMarker {
		v &= uint64(0xff >> n)
	}
	allOnes := v == uint64(0xff>>n)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
		if b[i] != 0xff {
			allOnes = false
		}
	}
	if !keepMarker && allOnes {
		return math.MaxUint64, n, true
	}
	return v, n, true
}

// elemAt reads the element header at off in the file.
func elemAt(b *blockFile, off int64) (ebmlElem, bool) {
	h, ok := b.bytesAt(off, int(min(12, b.size-off)))
	if !ok || len(h) < 2 {
		return ebmlElem{}, false
	}
	id, n, ok := readVint(h, true)
	if !ok || n > 4 {
		return ebmlElem{}, false
	}
	sz, m, ok := readVint(h[n:], false)
	if !ok {
		return ebmlElem{}, false
	}
	e := ebmlElem{id: uint32(id), data: off + int64(n+m)}
	if sz == math.MaxUint64 {
		e.size = -1
	} else {
		if sz > uint64(b.size) {
			return ebmlElem{}, false
		}
		e.size = int64(sz)
	}
	return e, true
}

// ebmlChild is one element of an in-memory master payload.
type ebmlChild struct {
	id   uint32
	data []byte
}

// seekEntry is a SeekHead entry: the id of a level-1 element and its position.
type seekEntry struct {
	id  uint32
	pos int64
}

// children splits an in-memory master payload into its elements.
func children(p []byte) []ebmlChild {
	var out []ebmlChild
	for i := 0; i < len(p); {
		id, n, ok := readVint(p[i:], true)
		if !ok || n > 4 {
			break
		}
		sz, m, ok := readVint(p[i+n:], false)
		if !ok || sz == math.MaxUint64 || uint64(len(p)-i-n-m) < sz {
			break
		}
		out = append(out, ebmlChild{uint32(id), p[i+n+m : i+n+m+int(sz)]})
		i += n + m + int(sz)
	}
	return out
}

func ebmlUint(p []byte) uint64 {
	var v uint64
	for _, c := range p {
		v = v<<8 | uint64(c)
	}
	return v
}

func ebmlFloat(p []byte) (float64, bool) {
	switch len(p) {
	case 0:
		return 0, true
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(p))), true
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(p)), true
	}
	return 0, false
}

type mkvChapter struct {
	uid, start, end  uint64
	hasStart, hasEnd bool
	title            string
}

type mkvSimpleTag struct {
	name, lang, str string
	hasName, hasStr bool
	def             bool
	sub             []mkvSimpleTag
}

type mkvTag struct {
	targetType                      string
	hasTargetType                   bool
	trackUID, chapterUID, attachUID uint64
	tags                            []mkvSimpleTag
}

type mkvState struct {
	b                   *blockFile
	timescale           uint64
	duration            float64
	hasDuration         bool
	title, muxingApp    string
	hasTitle, hasMuxing bool
	codec               string
	audioSeen           bool
	chapters            []mkvChapter
	tags                []mkvTag
	parsed              map[int64]bool
	err                 error
}

func probeMKV(b *blockFile) (*probeResult, error) {
	e, ok := elemAt(b, 0)
	if !ok || e.id != mkvEBML || e.size < 0 {
		return nil, unsure("mkv: no EBML header")
	}
	hdr, ok := b.bytesAt(e.data, int(e.size))
	if !ok {
		return nil, unsure("mkv: EBML header")
	}
	for _, c := range children(hdr) {
		if c.id == 0x4282 && string(c.data) != "matroska" && string(c.data) != "webm" {
			return nil, unsure("mkv: doctype " + string(c.data))
		}
		if c.id == 0x4285 && ebmlUint(c.data) > 3 {
			return nil, unsure("mkv: doctype version")
		}
	}
	seg, ok := elemAt(b, e.data+e.size)
	if !ok || seg.id != mkvSegment {
		return nil, unsure("mkv: no Segment after the header")
	}
	segEnd := b.size
	if seg.size >= 0 {
		segEnd = min(seg.data+seg.size, b.size)
	}
	s := &mkvState{b: b, timescale: 1000000, parsed: map[int64]bool{}}
	var seeks []seekEntry
	at := seg.data
	for at < segEnd {
		c, ok := elemAt(b, at)
		if !ok {
			break
		}
		if c.id == mkvCluster {
			break
		}
		if c.size < 0 {
			return nil, unsure("mkv: unknown-size element before the first cluster")
		}
		switch c.id {
		case mkvSeekHead:
			p := s.payload(c)
			for _, sk := range children(p) {
				if sk.id != 0x4DBB {
					continue
				}
				var id uint32
				var pos int64 = -1
				for _, f := range children(sk.data) {
					switch f.id {
					case 0x53AB:
						id = uint32(ebmlUint(f.data))
					case 0x53AC:
						pos = int64(ebmlUint(f.data))
					}
				}
				if pos >= 0 {
					seeks = append(seeks, seekEntry{id, pos})
				}
			}
		default:
			s.level1(c, at)
		}
		if s.err != nil {
			return nil, s.err
		}
		at = c.data + c.size
	}
	// matroska_execute_seekhead: the level-1 elements not read yet.
	for _, sk := range seeks {
		if sk.id == mkvCues || sk.id == mkvCluster || sk.id == mkvSeekHead {
			continue
		}
		pos := seg.data + sk.pos
		if s.parsed[pos] {
			continue
		}
		c, ok := elemAt(b, pos)
		if !ok || c.id != sk.id || c.size < 0 {
			continue
		}
		s.level1(c, pos)
		if s.err != nil {
			return nil, s.err
		}
	}
	if s.codec == "" {
		if !s.audioSeen {
			return nil, unsure("mkv: no audio track")
		}
		return nil, s.err
	}
	if !s.hasDuration || s.duration == 0 || math.IsNaN(s.duration) {
		return nil, unsure("mkv: no Segment duration")
	}
	durUS := int64(s.duration * float64(s.timescale) * 1000 / 1000000)

	var d avDict
	if s.hasTitle {
		d.set("title", s.title, 0)
	}
	if s.hasMuxing {
		d.set("encoder", s.muxingApp, 0)
	}
	// Chapters (ids are uids), then the tags.
	var cl chapterList
	var maxStart uint64
	for _, c := range s.chapters {
		if c.hasStart && c.uid != 0 && (maxStart == 0 || c.start > maxStart) {
			if c.uid > math.MaxInt64 {
				return nil, unsure("mkv: chapter uid over int64")
			}
			cl.add(int64(c.uid), 1, 1000000000, int64(c.start), int64(c.end), c.hasEnd, c.title)
			maxStart = c.start
		}
	}
	// A chapter whose end comes from the format's end needs the start time
	// FFmpeg reads from the packets: not modelled.
	var last *avChapter
	for _, ch := range cl.c {
		if last == nil || ch.start > last.start {
			last = ch
		}
	}
	if last != nil && !last.hasEnd {
		return nil, unsure("mkv: last chapter has no end")
	}
	for _, t := range s.tags {
		switch {
		case t.attachUID != 0:
			continue
		case t.chapterUID != 0:
			for _, sc := range t.tags {
				if asciiFold(sc.name, "title") {
					return nil, unsure("mkv: chapter title from a tag")
				}
			}
			continue
		case t.trackUID != 0:
			continue
		}
		convertMKVTags(&d, t.tags, t.targetType, t.hasTargetType)
	}
	return result(durUS, s.codec, &d, cl.finish(durUS, 0, false)), nil
}

func convertMKVTags(d *avDict, tags []mkvSimpleTag, prefix string, hasPrefix bool) {
	for _, t := range tags {
		lang := ""
		if t.lang != "und" {
			lang = t.lang
		}
		if !t.hasName {
			continue
		}
		key := t.name
		if hasPrefix {
			key = prefix + "/" + t.name
		}
		set := func(k string) {
			if t.hasStr {
				d.set(k, t.str, 0)
			} else {
				d.del(k)
			}
			if len(t.sub) > 0 {
				convertMKVTags(d, t.sub, k, true)
			}
		}
		if t.def || lang == "" {
			set(key)
		}
		if lang != "" {
			set(key + "-" + lang)
		}
	}
	d.conv([][2]string{{"LEAD_PERFORMER", "performer"}, {"PART_NUMBER", "track"}})
}

func (s *mkvState) payload(c ebmlElem) []byte {
	if c.size > 16<<20 {
		s.err = unsure("mkv: element over 16 MiB")
		return nil
	}
	p, ok := s.b.bytesAt(c.data, int(c.size))
	if !ok {
		s.err = unsure("mkv: element past the end")
		return nil
	}
	return p
}

func (s *mkvState) level1(c ebmlElem, pos int64) {
	switch c.id {
	case mkvInfo, mkvTracks, mkvChapters, mkvTags:
	default:
		return
	}
	s.parsed[pos] = true
	p := s.payload(c)
	if p == nil {
		return
	}
	switch c.id {
	case mkvInfo:
		for _, f := range children(p) {
			switch f.id {
			case 0x2AD7B1:
				s.timescale = ebmlUint(f.data)
			case 0x4489:
				v, ok := ebmlFloat(f.data)
				if !ok {
					s.err = unsure("mkv: duration size")
					return
				}
				s.duration, s.hasDuration = v, true
			case 0x7BA9:
				s.title, s.hasTitle = cutNUL(string(f.data)), true
			case 0x4D80:
				s.muxingApp, s.hasMuxing = cutNUL(string(f.data)), true
			}
		}
	case mkvTracks:
		for _, te := range children(p) {
			if te.id != 0xAE {
				continue
			}
			var typ uint64
			var codecID string
			for _, f := range children(te.data) {
				switch f.id {
				case 0x83:
					typ = ebmlUint(f.data)
				case 0x86:
					codecID = cutNUL(string(f.data))
				}
			}
			if typ == 2 && !s.audioSeen {
				s.audioSeen = true
				s.codec = mkvCodec(codecID)
				if s.codec == "" {
					s.err = unsure("mkv: codec " + codecID)
					return
				}
			}
		}
	case mkvChapters:
		for _, ed := range children(p) {
			if ed.id != 0x45B9 {
				continue
			}
			for _, at := range children(ed.data) {
				if at.id != 0xB6 {
					continue
				}
				var ch mkvChapter
				for _, f := range children(at.data) {
					switch f.id {
					case 0x73C4:
						ch.uid = ebmlUint(f.data)
					case 0x91:
						ch.start, ch.hasStart = ebmlUint(f.data), true
					case 0x92:
						ch.end, ch.hasEnd = ebmlUint(f.data), true
					case 0x80:
						for _, g := range children(f.data) {
							if g.id == 0x85 {
								ch.title = cutNUL(string(g.data))
							}
						}
					}
				}
				s.chapters = append(s.chapters, ch)
			}
		}
	case mkvTags:
		for _, tg := range children(p) {
			if tg.id != 0x7373 {
				continue
			}
			var t mkvTag
			for _, f := range children(tg.data) {
				switch f.id {
				case 0x63C0:
					for _, g := range children(f.data) {
						switch g.id {
						case 0x63CA:
							t.targetType, t.hasTargetType = cutNUL(string(g.data)), true
						case 0x63C5:
							t.trackUID = ebmlUint(g.data)
						case 0x63C4:
							t.chapterUID = ebmlUint(g.data)
						case 0x63C6:
							t.attachUID = ebmlUint(g.data)
						}
					}
				case 0x67C8:
					t.tags = append(t.tags, parseSimpleTag(f.data))
				}
			}
			s.tags = append(s.tags, t)
		}
	}
}

func parseSimpleTag(p []byte) mkvSimpleTag {
	t := mkvSimpleTag{lang: "und"}
	for _, f := range children(p) {
		switch f.id {
		case 0x45A3:
			t.name, t.hasName = cutNUL(string(f.data)), true
		case 0x447A:
			t.lang = cutNUL(string(f.data))
		case 0x4484, 0x44B4:
			t.def = ebmlUint(f.data) != 0
		case 0x4487:
			t.str, t.hasStr = cutNUL(string(f.data)), true
		case 0x67C8:
			t.sub = append(t.sub, parseSimpleTag(f.data))
		}
	}
	return t
}

// mkvCodec is ff_mkv_codec_tags' prefix match for the audio codecs whose name
// needs nothing more than the id ("" otherwise).
func mkvCodec(id string) string {
	table := []struct{ prefix, name string }{
		{"A_AAC", "aac"}, {"A_AC3", "ac3"}, {"A_ALAC", "alac"}, {"A_DTS", "dts"},
		{"A_EAC3", "eac3"}, {"A_FLAC", "flac"}, {"A_MLP", "mlp"}, {"A_MPEG/L2", "mp2"},
		{"A_MPEG/L1", "mp1"}, {"A_MPEG/L3", "mp3"}, {"A_OPUS", "opus"}, {"A_TRUEHD", "truehd"},
		{"A_TTA1", "tta"}, {"A_VORBIS", "vorbis"}, {"A_WAVPACK4", "wavpack"},
	}
	for _, t := range table {
		if strings.HasPrefix(id, t.prefix) {
			return t.name
		}
	}
	return ""
}
