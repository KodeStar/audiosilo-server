package metadata

import (
	"testing"

	"github.com/dhowden/tag"
)

func TestSplitSeriesIndex(t *testing.T) {
	cases := []struct {
		in        string
		wantIdx   float64
		wantTitle string
	}{
		{"01 - Unsouled", 1, "Unsouled"},
		{"C02 Soulsmith", 2, "Soulsmith"},
		{"Book 3 - Title", 3, "Title"},
		{"#4 Reaper", 4, "Reaper"},
		{"Unsouled", 0, "Unsouled"},
		{"  7. Wintersteel ", 7, "Wintersteel"},
	}
	for _, c := range cases {
		idx, title := splitSeriesIndex(c.in)
		if idx != c.wantIdx || title != c.wantTitle {
			t.Errorf("splitSeriesIndex(%q) = %v,%q want %v,%q", c.in, idx, title, c.wantIdx, c.wantTitle)
		}
	}
}

func TestDeriveFromPath(t *testing.T) {
	cases := []struct {
		name     string
		rel      string
		isFolder bool
		want     Metadata
	}{
		{
			name: "single-file book",
			rel:  "Will Wight/Cradle/01 - Unsouled.m4b",
			want: Metadata{Title: "Unsouled", Series: "Cradle", Author: "Will Wight", SeriesIndex: 1},
		},
		{
			name:     "folder book",
			rel:      "Brandon Sanderson/Mistborn/02 - The Well of Ascension",
			isFolder: true,
			want:     Metadata{Title: "The Well of Ascension", Series: "Mistborn", Author: "Brandon Sanderson", SeriesIndex: 2},
		},
		{
			name: "root file carries no hierarchy",
			rel:  "01 - Unsouled.m4b",
			want: Metadata{Title: "Unsouled", SeriesIndex: 1},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeriveFromPath(c.rel, c.isFolder)
			if got.Title != c.want.Title || got.Author != c.want.Author ||
				got.Series != c.want.Series || got.SeriesIndex != c.want.SeriesIndex {
				t.Errorf("DeriveFromPath = %+v want %+v", *got, c.want)
			}
		})
	}
}

func TestIsAudio(t *testing.T) {
	if !IsAudio("foo.m4b") || !IsAudio("BAR.MP3") || !IsAudio("part.mp4") {
		t.Error("expected audio extensions recognized")
	}
	if IsAudio("cover.jpg") || IsAudio("notes.txt") {
		t.Error("expected non-audio rejected")
	}
}

func TestApplyProbePrecedence(t *testing.T) {
	// applyProbe consumes lowercased tag maps (normalizeTags). It documents these
	// precedences: album over title, album_artist over artist (then author), the
	// narrator/composer fallback and the series multi-key fallback.
	t.Run("preferred keys win", func(t *testing.T) {
		m := &Metadata{}
		applyProbe(m, &probeResult{Tags: map[string]string{
			"album":        "The Album Title",
			"title":        "The Track Title",
			"album_artist": "Album Artist",
			"artist":       "Track Artist",
			"author":       "Author Tag",
			"narrator":     "The Narrator",
			"composer":     "The Composer",
			"series":       "First Series",
			"show":         "Show Series",
			"grouping":     "Grouping Series",
			"series-part":  "3",
		}})
		if m.Title != "The Album Title" {
			t.Errorf("Title = %q, want album over title", m.Title)
		}
		if m.Author != "Album Artist" {
			t.Errorf("Author = %q, want album_artist over artist/author", m.Author)
		}
		if m.Narrator != "The Narrator" {
			t.Errorf("Narrator = %q, want narrator over composer", m.Narrator)
		}
		if m.Series != "First Series" {
			t.Errorf("Series = %q, want series over show/grouping", m.Series)
		}
		if m.SeriesIndex != 3 {
			t.Errorf("SeriesIndex = %v, want 3 (parsed from series-part)", m.SeriesIndex)
		}
	})

	t.Run("fallback keys when preferred absent", func(t *testing.T) {
		m := &Metadata{}
		applyProbe(m, &probeResult{Tags: map[string]string{
			"title":    "The Track Title",
			"artist":   "Track Artist",
			"composer": "The Composer",
			"grouping": "Grouping Series",
		}})
		if m.Title != "The Track Title" {
			t.Errorf("Title = %q, want title fallback", m.Title)
		}
		if m.Author != "Track Artist" {
			t.Errorf("Author = %q, want artist fallback", m.Author)
		}
		if m.Narrator != "The Composer" {
			t.Errorf("Narrator = %q, want composer fallback", m.Narrator)
		}
		if m.Series != "Grouping Series" {
			t.Errorf("Series = %q, want grouping fallback", m.Series)
		}
	})

	t.Run("author tag fills when no album_artist/artist", func(t *testing.T) {
		m := &Metadata{}
		applyProbe(m, &probeResult{Tags: map[string]string{"author": "Author Tag"}})
		if m.Author != "Author Tag" {
			t.Errorf("Author = %q, want author tag as last fallback", m.Author)
		}
	})

	t.Run("duration and codec only set when present", func(t *testing.T) {
		m := &Metadata{Duration: 100, Codec: "aac"}
		applyProbe(m, &probeResult{Tags: map[string]string{}}) // zero duration, empty codec
		if m.Duration != 100 || m.Codec != "aac" {
			t.Errorf("applyProbe clobbered existing duration/codec: %v %q", m.Duration, m.Codec)
		}
		applyProbe(m, &probeResult{Duration: 250, Codec: "mp3", Tags: map[string]string{}})
		if m.Duration != 250 || m.Codec != "mp3" {
			t.Errorf("applyProbe didn't set duration/codec: %v %q", m.Duration, m.Codec)
		}
	})
}

func TestNormalizeTagsAndRawString(t *testing.T) {
	// normalizeTags lowercases keys so applyProbe's lookups are case-insensitive.
	norm := normalizeTags(map[string]string{"ALBUM": "X", "Series-Part": "2"})
	if norm["album"] != "X" || norm["series-part"] != "2" {
		t.Fatalf("normalizeTags did not lowercase keys: %v", norm)
	}

	// rawString returns the first non-empty match across its key list (the
	// multi-key fallback applyTags relies on for series/narrator).
	raw := map[string]interface{}{
		"series": "",        // present but empty -> skip
		"©grp":   "Grouped", // second key wins
		"other":  123,       // non-string -> ignored
	}
	if got := rawString(raw, "series", "©grp", "show"); got != "Grouped" {
		t.Errorf("rawString = %q, want %q (multi-key fallback)", got, "Grouped")
	}
	if got := rawString(raw, "missing", "absent"); got != "" {
		t.Errorf("rawString = %q, want empty when no key matches", got)
	}
	if got := rawString(raw, "other"); got != "" {
		t.Errorf("rawString = %q, want empty for non-string value", got)
	}
}

// fakeTags is a minimal tag.Metadata stub for exercising applyTags. Only the
// accessors applyTags reads are overridden; the rest are never called.
type fakeTags struct {
	tag.Metadata
	album, title, albumArtist, artist, composer string
	raw                                         map[string]interface{}
}

func (f fakeTags) Album() string               { return f.album }
func (f fakeTags) Title() string               { return f.title }
func (f fakeTags) AlbumArtist() string         { return f.albumArtist }
func (f fakeTags) Artist() string              { return f.artist }
func (f fakeTags) Composer() string            { return f.composer }
func (f fakeTags) Picture() *tag.Picture       { return nil }
func (f fakeTags) Raw() map[string]interface{} { return f.raw }

func TestApplyTagsTrimsWhitespace(t *testing.T) {
	// Regression for the trim-consistency fix: Title, Narrator and Series are now
	// trimmed like Author already was, so no extracted string field keeps stray
	// leading/trailing whitespace.
	m := &Metadata{}
	applyTags(m, fakeTags{
		album:       "  Padded Title  ",
		albumArtist: "  Padded Author  ",
		composer:    "  Padded Narrator  ",
		raw: map[string]interface{}{
			"series":   "  Padded Series  ",
			"narrator": "  Raw Narrator  ",
		},
	})
	if m.Title != "Padded Title" {
		t.Errorf("Title = %q, want trimmed", m.Title)
	}
	if m.Author != "Padded Author" {
		t.Errorf("Author = %q, want trimmed", m.Author)
	}
	if m.Series != "Padded Series" {
		t.Errorf("Series = %q, want trimmed", m.Series)
	}
	// raw narrator overrides composer, and is trimmed.
	if m.Narrator != "Raw Narrator" {
		t.Errorf("Narrator = %q, want trimmed raw narrator", m.Narrator)
	}
}

func TestApplyTagsTitleAlbumPrecedence(t *testing.T) {
	// Album wins over Title (audiobook title commonly lives in album), and the
	// fix preserves that precedence while trimming.
	m := &Metadata{}
	applyTags(m, fakeTags{album: " Album Name ", title: " Track Name "})
	if m.Title != "Album Name" {
		t.Errorf("Title = %q, want trimmed album (album over title)", m.Title)
	}

	// Title is used (trimmed) only when album is absent.
	m2 := &Metadata{}
	applyTags(m2, fakeTags{title: "  Only Title  "})
	if m2.Title != "Only Title" {
		t.Errorf("Title = %q, want trimmed title fallback", m2.Title)
	}
}

func TestIsGenericTitle(t *testing.T) {
	generic := []string{
		"", "Track 01", "track 1", "Chapter 12", "Part 3", "Disc 2",
		"CD 1", "CD1", "disc02", "01", "1", "  07. ", "Track 1 - 2",
	}
	for _, s := range generic {
		if !IsGenericTitle(s) {
			t.Errorf("IsGenericTitle(%q) = false, want true", s)
		}
	}
	// Real titles — including ones that merely start with a label word.
	real := []string{
		"A Christmas Carol", "Unsouled", "How to Train Your Dragon",
		"Part of Your World", "Chapter and Verse", "Track Changes", "CD Projekt",
	}
	for _, s := range real {
		if IsGenericTitle(s) {
			t.Errorf("IsGenericTitle(%q) = true, want false", s)
		}
	}
}
