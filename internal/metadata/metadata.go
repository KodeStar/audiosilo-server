// Package metadata extracts audiobook metadata from files. Lightweight tags are
// read in-process with dhowden/tag; durations and chapters come from ffprobe
// when available. A filename/path heuristic fills gaps for untagged files.
package metadata

import (
	"os"
	"path/filepath"
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
//   - Start/End are offsets *within that file* — seek to Start.
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
var AudioExtensions = map[string]bool{
	".m4b": true, ".m4a": true, ".mp4": true, ".mp3": true, ".aax": true,
	".flac": true, ".ogg": true, ".opus": true,
}

// IsAudio reports whether path has a recognized audiobook extension.
func IsAudio(path string) bool {
	return AudioExtensions[strings.ToLower(filepath.Ext(path))]
}

// Extract reads embedded metadata (tags + ffprobe) for the file at path.
// ffprobePath may be empty to skip ffprobe-based duration/chapter extraction.
// Path-based inference is intentionally not done here — the scanner applies it
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
	if title := firstNonEmpty(md.Album(), md.Title()); title != "" { // audiobook title commonly in album
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

// IsGenericTitle reports whether a title carries no real book identity — a bare
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
				return false // a real word — not a generic label
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
	for _, prefix := range []string{"book ", "vol ", "vol. ", "volume ", "c", "#"} {
		if strings.HasPrefix(lower, prefix) {
			s = strings.TrimSpace(s[len(prefix):])
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
	idx, _ := strconv.ParseFloat(s[:i], 64)
	rest := strings.TrimLeft(s[i:], " -_.")
	if rest == "" {
		rest = strings.TrimSpace(name)
	}
	return idx, rest
}
