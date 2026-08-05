// Package metadata extracts audiobook metadata from files. Lightweight tags are
// read in-process with dhowden/tag; durations and chapters come from ffprobe
// when available. A filename/path heuristic fills gaps for untagged files.
package metadata

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dhowden/tag"
)

// Chapter is one playable unit, normalized so a client can treat embedded m4b
// chapters and separate multi-file parts identically:
//
//   - FileIndex is the 0-based ordinal of the book file the chapter lives in
//     (matching BookFile.Seq; 0 for single-file books), used for ordering.
//     Streaming is path-addressed: play FilePath via
//     /libraries/{id}/stream?path=<FilePath>.
//   - Start/End are offsets *within that file* - seek to Start.
//   - BookOffset is the chapter's start on the whole-book timeline (the sum of
//     earlier files' durations), so progress can use one continuous position.
type Chapter struct {
	Index      int     `json:"index"`
	Title      string  `json:"title"`
	FileIndex  int     `json:"file_index"`
	FilePath   string  `json:"file_path"` // library-relative path of the file to stream
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	BookOffset float64 `json:"book_offset"`
}

// Metadata is the extracted view of a book file.
type Metadata struct {
	Title       string    `json:"title"`
	Author      string    `json:"author"`
	Series      string    `json:"series"`
	SeriesIndex float64   `json:"series_index"`
	Narrator    string    `json:"narrator"`
	Duration    float64   `json:"duration"`
	Format      string    `json:"format"`
	Codec       string    `json:"codec"` // audio codec from ffprobe (e.g. aac, mp3, ac3)
	HasCover    bool      `json:"has_cover"`
	Chapters    []Chapter `json:"chapters,omitempty"`
}

// AudioExtensions are the file extensions AudioSilo treats as audiobooks.
// .mp4 is included because audiobooks are sometimes delivered as AAC-in-MP4;
// media.ServeFile serves it as audio/mp4 and the transcoder covers odd codecs.
//
// Audible DRM formats (.aax/.aaxc) are intentionally excluded: the server can never
// stream them (they need per-account decryption), and indexing an .aax that sits
// next to its converted .m4b would lump both into one book with duplicated chapters
// and an unplayable track. The manager converts .aax/.aaxc to .m4b before content
// enters a library, so a library should only ever hold the playable .m4b.
var AudioExtensions = map[string]bool{
	".m4b": true, ".m4a": true, ".mp4": true, ".mp3": true,
	".flac": true, ".ogg": true, ".opus": true,
}

// IsAudio reports whether path has a recognized audiobook extension.
func IsAudio(path string) bool {
	return AudioExtensions[strings.ToLower(filepath.Ext(path))]
}

// Extract reads embedded metadata (tags + ffprobe) for the file at path.
// ffprobePath may be empty to skip ffprobe-based duration/chapter extraction.
// Path-based inference is intentionally not done here - the scanner applies it
// with knowledge of the library's storage layout (see DeriveFromPath).
func Extract(path, ffprobePath string) (*Metadata, error) {
	m := &Metadata{Format: strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")}

	// Embedded tags via dhowden/tag (best-effort).
	if f, err := os.Open(path); err == nil {
		if md, terr := tag.ReadFrom(f); terr == nil {
			applyTags(m, md)
		}
		f.Close()
	}

	// ffprobe for duration + chapters + richer container tags (best-effort).
	if ffprobePath != "" {
		if p, err := probe(path, ffprobePath); err == nil {
			applyProbe(m, p)
		}
	}
	return m, nil
}

func applyTags(m *Metadata, md tag.Metadata) {
	if title := bookTitle(md.Album(), md.Title()); title != "" {
		m.Title = title
	}
	if a := firstNonEmpty(md.AlbumArtist(), md.Artist()); a != "" {
		m.Author = a
	}
	if c := strings.TrimSpace(md.Composer()); c != "" {
		m.Narrator = c
	}
	if raw := md.Raw(); raw != nil {
		if v := rawString(raw, "series", "©grp", "show", "TXXX:SERIES"); v != "" {
			m.Series = strings.TrimSpace(v)
		}
		if v := rawString(raw, "narrator", "©nrt", "----:com.apple.iTunes:NARRATOR"); v != "" {
			m.Narrator = strings.TrimSpace(v)
		}
	}
	if md.Picture() != nil {
		m.HasCover = true
	}
}

// bookTitle picks the book title from an album/title tag pair. The album tag
// usually holds the audiobook's title (the title tag often carries a chapter or
// track name), but Audible-style files put the SERIES in album and the real title
// in the title tag ("Portal to Nova Roma (Unabridged)" vs "Portal to Nova Roma:
// Paris (Unabridged)") - preferring album there indexes the book under its series
// name and the real title is lost. Prefer the title tag when it extends the album
// at a word boundary with a tail that carries book identity (a subtitle, or a
// "Volume N" that names a different book); a tail that is a track marker ("Album -
// Part 2", "Album 03") or leads with one ("Album 03 - Some Chapter Name", the
// per-file naming of CD/chapter rips whose first file would retitle the whole
// book) keeps the album, since that is a track name.
func bookTitle(album, title string) string {
	a, t := strings.TrimSpace(album), strings.TrimSpace(title)
	if a == "" || t == "" {
		return firstNonEmpty(a, t)
	}
	ca, ct := canonTag(a), canonTag(t)
	if len(ct) > len(ca) && strings.HasPrefix(ct, ca) && !isAlnumByte(ct[len(ca)]) {
		if tail := strings.Trim(ct[len(ca):], " -:,;|."); tail != "" && !IsGenericTitle(tail) && !trackMarkerLead(tail) {
			return t
		}
	}
	return a
}

// trackMarkerLead reports whether a tail STARTS with a track ordinal ("03 - Some
// Chapter", "Chapter 1 - Bran", "CD2 The Escape"): its first one or two tokens
// alone form a generic track label. "Volume"/"Book" markers are deliberately not
// track labels - a "Volume N" tail names a different book, not a file of this one.
func trackMarkerLead(tail string) bool {
	f := strings.Fields(tail)
	if len(f) == 0 {
		return false
	}
	return IsGenericTitle(f[0]) || (len(f) > 1 && IsGenericTitle(f[0]+" "+f[1]))
}

func isAlnumByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// canonTag lowercases a tag and strips bracketed groups ("(Unabridged)") so an
// edition suffix doesn't hide an album-is-prefix-of-title relationship.
func canonTag(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(bracketed.ReplaceAllString(s, " ")), " "))
}

// bracketed strips parenthetical/bracketed groups for canonTag.
var bracketed = regexp.MustCompile(`[\(\[][^\)\]]*[\)\]]`)

func rawString(raw map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := raw[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// DeriveFromPath infers title/series/author/index from a book's relative path.
// The book identity is the filename (single-file books) or the folder name
// (multi-file / folder books); the two ancestor directories are treated as
// series and author respectively:
//
//	Author/Series/01 - Title.m4b   (single-file book)
//	Author/Series/01 - Title/      (folder book, isFolder)
//	01 - Title.m4b                 (no ancestors -> no series/author)
//
// Derivation is purely structural now that libraries auto-detect their shape: a
// book sitting at the library root simply has no ancestors and so carries no
// hierarchy (the former "flat" layout falls out for free).
func DeriveFromPath(relPath string, isFolder bool) *Metadata {
	m := &Metadata{}
	segments := strings.Split(strings.Trim(filepath.ToSlash(relPath), "/"), "/")
	if len(segments) == 0 {
		return m
	}

	name := segments[len(segments)-1]
	if !isFolder {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	ancestors := segments[:len(segments)-1]

	m.SeriesIndex, m.Title = splitSeriesIndex(name)
	if n := len(ancestors); n >= 1 {
		m.Series = ancestors[n-1]
	}
	if n := len(ancestors); n >= 2 {
		m.Author = ancestors[n-2]
	}
	return m
}

// SplitSeriesIndex parses a leading volume/track number out of a name like
// "01 - Unsouled", returning the number and the remaining title. Exported for
// reuse by the scanner when titling multi-file parts.
func SplitSeriesIndex(name string) (float64, string) { return splitSeriesIndex(name) }

// discLabels are the words that, together with a number, form a generic part
// label ("Track 01", "Disc 2", "CD1").
var discLabels = map[string]bool{"track": true, "chapter": true, "part": true, "disc": true, "disk": true, "cd": true}

// IsGenericTitle reports whether a title carries no real book identity - a bare
// number or a "track/chapter/part/disc N"-style label (e.g. "Track 01"). It is
// token-based, so real titles that merely start with such a word ("Part of Your
// World") are NOT flagged: every token must be a number or a disc label, with at
// least one number present. Such a title (common in poorly-tagged files) should
// not override a path-derived title, nor be used to match two books as the same.
func IsGenericTitle(title string) bool {
	// Normalize: lowercase, collapse any run of non-alphanumerics to one space.
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(title) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			space = false
		case !space:
			b.WriteByte(' ')
			space = true
		}
	}
	t := strings.TrimSpace(b.String())
	if t == "" {
		return true
	}
	hasNumber := false
	for _, tok := range strings.Fields(t) {
		switch {
		case isAllDigits(tok):
			hasNumber = true
		case discLabels[tok]:
			// a bare label word; contributes no number on its own
		default:
			// a label with digits attached, e.g. "cd1" / "disc02"?
			ok := false
			for lbl := range discLabels {
				if rest, found := strings.CutPrefix(tok, lbl); found && rest != "" && isAllDigits(rest) {
					ok, hasNumber = true, true
					break
				}
			}
			if !ok {
				return false // a real word - not a generic label
			}
		}
	}
	return hasNumber
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// splitSeriesIndex parses a leading volume number out of names like
// "01 - Unsouled", "C02 Soulsmith", "Book 3 - Title".
func splitSeriesIndex(name string) (float64, string) {
	s := strings.TrimSpace(name)
	lower := strings.ToLower(s)
	explicit := false
	for _, prefix := range []string{"book ", "vol ", "vol. ", "volume ", "c", "#"} {
		if strings.HasPrefix(lower, prefix) {
			s = strings.TrimSpace(s[len(prefix):])
			explicit = true
			break
		}
	}
	// Now read leading digits.
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, strings.TrimSpace(name)
	}
	// A bare leading number of 4+ digits (e.g. "1984", "2001 A Space Odyssey") is far
	// more likely a year or part of the title than a volume number, so only accept it
	// as a series index when it was explicitly marked ("Book 1984", "#1984"). This
	// keeps ordinary volume numbers (up to 999) working while not mangling titles.
	if !explicit && i >= 4 {
		return 0, strings.TrimSpace(name)
	}
	idx, _ := strconv.ParseFloat(s[:i], 64)
	rest := strings.TrimLeft(s[i:], " -_.")
	if rest == "" {
		rest = strings.TrimSpace(name)
	}
	return idx, rest
}
