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
		// A bare year/large number is a title, not a volume index...
		{"1984", 0, "1984"},
		{"2001 A Space Odyssey", 0, "2001 A Space Odyssey"},
		// ...unless it is explicitly marked as a volume.
		{"Book 1984 - Title", 1984, "Title"},
		// Realistic three-digit volume numbers still parse.
		{"100 - Long Series Vol", 100, "Long Series Vol"},
		// A number running into the words is part of the title.
		{"20,000 Leagues Under the Sea", 0, "20,000 Leagues Under the Sea"},
		{"3rd Rock", 0, "3rd Rock"},
		{"1's and 0's", 0, "1's and 0's"},
		// A decimal position (a novella between two books) keeps its fraction.
		{"02.5 - Gods of Risk", 2.5, "Gods of Risk"},
		{"Book 7.5 Interlude", 7.5, "Interlude"},
		{"3.Title", 3, "Title"},
		{"2001.5 Odyssey", 0, "2001.5 Odyssey"},
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
			name: "one folder is the author, not a series",
			rel:  "Charles Dickens/Great Expectations.m4b",
			want: Metadata{Title: "Great Expectations", Author: "Charles Dickens"},
		},
		{
			name:     "one folder above a folder book",
			rel:      "George Orwell/1984",
			isFolder: true,
			want:     Metadata{Title: "1984", Author: "George Orwell"},
		},
		{
			name: "root file carries no hierarchy",
			rel:  "01 - Unsouled.m4b",
			want: Metadata{Title: "Unsouled", SeriesIndex: 1},
		},
		// A leaf named "Author; Series; Book NN[; Title]" states its series and
		// position (LeafSeries), the shapes of the LibraForge benchmark; its title
		// part or the numbered series identifies it.
		{
			name:     "leaf series and position",
			rel:      "Mark Tufo/Mark Tufo; Indian Hill; Book 04; From The Ashes",
			isFolder: true,
			want: Metadata{Title: "From The Ashes", Author: "Mark Tufo",
				Series: "Indian Hill", SeriesIndex: 4},
		},
		{
			name: "leaf file without a title part",
			rel:  "A.R. Wise/A.R. Wise; Deadlocked; Book 01.m4b",
			want: Metadata{Title: "Deadlocked 1", Author: "A.R. Wise", Series: "Deadlocked", SeriesIndex: 1},
		},
		{
			name:     "leaf range",
			rel:      "Bruno Miller/Bruno Miller; Dark Road; Book 01-05",
			isFolder: true,
			want: Metadata{Title: "Dark Road 1-5", Author: "Bruno Miller", Series: "Dark Road",
				SeriesIndex: 1, SeriesIndexEnd: 5},
		},
		{
			name:     "leaf range with a title part",
			rel:      "Tao Wong/Tao Wong; System Apocalypse; Book 10-12; Omnibus Volume 4",
			isFolder: true,
			want: Metadata{Title: "Omnibus Volume 4", Author: "Tao Wong",
				Series: "System Apocalypse", SeriesIndex: 10, SeriesIndexEnd: 12},
		},
		{
			name:     "leaf Book XX states no position",
			rel:      "B.V. Larson/B.V. Larson; Star Force; Book XX; Army of One",
			isFolder: true,
			want:     Metadata{Title: "Army of One", Author: "B.V. Larson", Series: "Star Force"},
		},
		{
			name:     "leaf Book 00 states no position",
			rel:      "Larry Correia/Larry Correia; Grimnoir Chronicles; Book 00; Tokyo Raider",
			isFolder: true,
			want: Metadata{Title: "Tokyo Raider", Author: "Larry Correia",
				Series: "Grimnoir Chronicles"},
		},
		{
			name:     "a leaf's series over the folder's",
			rel:      "Mark Tufo/Zombies/Mark Tufo; Zombie Fallout; Book 02",
			isFolder: true,
			want: Metadata{Title: "Zombie Fallout 2", Author: "Mark Tufo", Series: "Zombie Fallout",
				SeriesIndex: 2},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeriveFromPath(c.rel, c.isFolder)
			if got.Title != c.want.Title || got.Author != c.want.Author || got.Series != c.want.Series ||
				got.SeriesIndex != c.want.SeriesIndex || got.SeriesIndexEnd != c.want.SeriesIndexEnd {
				t.Errorf("DeriveFromPath = %+v want %+v", *got, c.want)
			}
		})
	}
}

func TestIsAudio(t *testing.T) {
	if !IsAudio("foo.m4b") || !IsAudio("BAR.MP3") || !IsAudio("part.mp4") || !IsAudio("book.mka") {
		t.Error("expected audio extensions recognized")
	}
	if IsAudio("cover.jpg") || IsAudio("notes.txt") {
		t.Error("expected non-audio rejected")
	}
	// Audible DRM formats are intentionally not indexed - the server can't stream
	// them, and one next to its converted .m4b would double up the book's chapters.
	if IsAudio("book.aax") || IsAudio("book.aaxc") || IsAudio("BOOK.AAX") {
		t.Error("expected Audible DRM (.aax/.aaxc) rejected")
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
	album, title, albumArtist, artist, composer, genre string
	raw                                                map[string]interface{}
}

func (f fakeTags) Album() string               { return f.album }
func (f fakeTags) Title() string               { return f.title }
func (f fakeTags) AlbumArtist() string         { return f.albumArtist }
func (f fakeTags) Artist() string              { return f.artist }
func (f fakeTags) Composer() string            { return f.composer }
func (f fakeTags) Genre() string               { return f.genre }
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

// TestBookTitle pins the album/title tag choice. Album is still preferred (the
// audiobook title commonly lives there while title carries a track name), EXCEPT
// when the title tag extends the album with a real subtitle - the Audible tagging
// shape where album holds the SERIES ("Portal to Nova Roma (Unabridged)") and only
// the title tag carries the actual book title ("Portal to Nova Roma: Paris
// (Unabridged)"). Preferring album there indexed the book under its series name,
// which broke the manager's already-on-server matching for that book.
func TestBookTitle(t *testing.T) {
	cases := []struct{ name, album, title, want string }{
		{"album wins by default", "Album Name", "Track Name", "Album Name"},
		{"series-in-album yields to subtitled title", "Portal to Nova Roma (Unabridged)", "Portal to Nova Roma: Paris (Unabridged)", "Portal to Nova Roma: Paris (Unabridged)"},
		{"identical pair keeps album", "Book Title", "Book Title", "Book Title"},
		{"part tail is a track name, keep album", "Book Title", "Book Title - Part 2", "Book Title"},
		{"bare number tail is a track name, keep album", "Book Title", "Book Title 03", "Book Title"},
		{"disc tail is a track name, keep album", "Book Title", "Book Title CD1", "Book Title"},
		{"chapter tail is a track name, keep album", "Book Title", "Book Title - Chapter 12", "Book Title"},
		{"volume tail is book identity, prefer title", "Unintended Cultivator", "Unintended Cultivator: Volume 9", "Unintended Cultivator: Volume 9"},
		{"numbered chapter-name tail is a track name, keep album", "Book Title", "Book Title 03 - Some Chapter Name", "Book Title"},
		{"chapter N + name tail is a track name, keep album", "A Game of Thrones", "A Game of Thrones - Chapter 1 - Bran", "A Game of Thrones"},
		{"mid-word prefix is not an extension, keep album", "Dark", "Darker Days", "Dark"},
		{"mid-word prefix in another script, keep album", "Сто", "Столица", "Сто"},
		{"empty album falls back to title", "", "Only Title", "Only Title"},
		{"empty title falls back to album", "Only Album", "", "Only Album"},
		{"edition suffix alone keeps album", "Book Title", "Book Title (Unabridged)", "Book Title"},
	}
	for _, tc := range cases {
		if got := bookTitle(tc.album, tc.title); got != tc.want {
			t.Errorf("%s: bookTitle(%q, %q) = %q, want %q", tc.name, tc.album, tc.title, got, tc.want)
		}
	}
}

// TestPartsTitle: a book of several files takes the album over a title tag
// that extends it (the first file's: a chapter's name), and keeps a title the
// album gave, one with no album to give way to, a tag that names the book's
// own folder, or one another of its files carries too.
func TestPartsTitle(t *testing.T) {
	const unread = "\x00" // no other file's tags (nil)
	cases := []struct{ name, album, title, path, other, want string }{
		{"chapter tail gives way", "Frankenstein; or, The Modern Prometheus", "Frankenstein; or, The Modern Prometheus - Letters 1-4", "Frankenstein", unread, "Frankenstein; or, The Modern Prometheus"},
		{"another file's chapter", "Frankenstein; or, The Modern Prometheus", "Frankenstein; or, The Modern Prometheus - Letters 1-4", "Frankenstein", "Frankenstein; or, The Modern Prometheus - Chapters 21-24", "Frankenstein; or, The Modern Prometheus"},
		{"book tail gives way too", "The Odyssey", "The Odyssey: Book 01", "The Odyssey", "The Odyssey: Book 24", "The Odyssey"},
		{"underscore tail gives way", "The Diary of a Nobody", "The Diary of a Nobody_Ch00", "The Diary of a Nobody", unread, "The Diary of a Nobody"},
		{"another file without a title tag", "The Odyssey", "The Odyssey: Book 01", "The Odyssey", "", "The Odyssey"},
		{"album already chosen", "Pride and Prejudice", "Chapters 21-22", "Pride and Prejudice", "Chapters 21-22", "Pride and Prejudice"},
		{"no album", "", "The Essays of Francis Bacon 01", "Essays", unread, "Essays"},
		{"no album, generic first tag", "", "Track 1", "The Eye of the World", unread, "The Eye of the World"},
		{"no album, named chapters", "", "Ravens", "The Eye of the World", "The Peddler", "The Eye of the World"},
		{"no album, shared title", "", "The Quiet Harbour", "Files", "The Quiet Harbour", "The Quiet Harbour"},
		{"unrelated album, chapter names", "The Wheel of Time", "Ravens Rising", "The Dragon Reborn", "The Peddler", "The Dragon Reborn"},
		{"filename tags keep the album", "Real Title", "Part 01.mp3", "Odd Name", "Part 02.mp3", "Real Title"},
		{"album corroborated by folder", "The Odyssey", "The Voyage", "Odyssey", "Homecoming", "The Odyssey"},
		// Tags naming no chapter (a track label, the album and a number), or a
		// folder named as a volume or a numbered slot, keep the album.
		{"track tags keep the album", "The Gunslinger", "Track 01", "The Dark Tower 1", "Track 12", "The Gunslinger"},
		{"album and number tags keep the album", "The Gunslinger", "The Gunslinger 01", "Dark Tower", "The Gunslinger 05", "The Gunslinger"},
		{"number and album tags keep the album", "The Gunslinger", "01 - The Gunslinger", "Dark Tower", "05 - The Gunslinger", "The Gunslinger"},
		{"numbered folder keeps the album", "Sourcery", "Rincewind Runs", "Discworld 05", "The Sourcerer", "Sourcery"},
		{"volume folder keeps the album", "The Fellowship of the Ring", "A Long-expected Party", "Book One", "The Breaking of the Fellowship", "The Fellowship of the Ring"},
		{"box set range folder keeps the album", "The Expanse Box Set", "The Expanse Box Set: Leviathan Wakes", "Books 1-3", "The Expanse Box Set: Caliban's War", "The Expanse Box Set"},
		{"no album, bracketed parts", "", "Dune (Part 1)", "Frank Herbert - Dune [2007]", "Dune (Part 2)", "Dune"},
		// A folder named for what it holds, or files numbering one name in turn
		// (the book under a series album), keep the album as before.
		{"container folder keeps the album", "Pride and Prejudice", "Netherfield", "Unabridged", "Pemberley", "Pride and Prejudice"},
		{"series album, numbered book tags, container folder", "Harry Potter", "Harry Potter: Philosopher's Stone 01", "Files", "Harry Potter: Philosopher's Stone 17", "Harry Potter"},
		{"numbered book tags keep the album", "Harry Potter", "Philosopher's Stone 01", "Sorcerer Rip", "Philosopher's Stone 17", "Harry Potter"},
		{"no album, container folder keeps the tag", "", "Ravens", "MP3", "The Peddler", "Ravens"},
		{"no title tag", "Main Street", "", "Main Street", unread, "Main Street"},
		// A box set's album beside each book's own title: the tail names the folder.
		{"tail names the folder", "The Expanse Box Set", "The Expanse Box Set: Leviathan Wakes", "Leviathan Wakes", unread, "The Expanse Box Set: Leviathan Wakes"},
		{"whole tag names the folder", "Portal to Nova Roma", "Portal to Nova Roma: Paris", "Portal to Nova Roma - Paris", unread, "Portal to Nova Roma: Paris"},
		// The series as the album, the book's title on every file, in a folder
		// that names neither: the files agree, so the tag is the book's.
		{"every file carries it", "Harry Potter", "Harry Potter and the Philosopher's Stone", "HP1", "harry potter and the philosopher's stone", "Harry Potter and the Philosopher's Stone"},
		{"a longer name for the album", "Mistborn", "Mistborn: The Final Empire (Unabridged)", "Mistborn 1 - The Final Empire", "Mistborn: The Final Empire (Unabridged)", "Mistborn: The Final Empire (Unabridged)"},
		// Tags that differ only in a bracketed part marker name the book.
		{"bracketed parts, folder names the album", "Sapiens", "Sapiens: A Brief History (Part 1)", "Sapiens", "Sapiens: A Brief History (Part 2)", "Sapiens: A Brief History"},
		{"bracketed parts, no separator", "Harry Potter", "Harry Potter and the Philosopher's Stone (Part 1)", "HP1", "Harry Potter and the Philosopher's Stone (Part 2)", "Harry Potter and the Philosopher's Stone"},
		// The series as the album, the book's name past it on every file, then
		// the file's part or chapter, in a folder that names neither: the name
		// the files share is the book's.
		{"shared name, chapter tails", "The Wheel of Time", "The Wheel of Time: The Great Hunt - The Wheel", "Book 2", "The Wheel of Time: The Great Hunt - The Dragon", "The Great Hunt"},
		{"shared name, bracketed parts", "Portal to Nova Roma", "Portal to Nova Roma: Paris (Part 1)", "Book 2", "Portal to Nova Roma: Paris (Part 2)", "Paris"},
		{"shared name, bracketed album", "Portal to Nova Roma (Unabridged)", "Portal to Nova Roma: Paris (Part 1)", "Book 2", "Portal to Nova Roma: Paris (Part 2)", "Paris"},
		{"shared name, album in another case", "THE WHEEL OF TIME", "The Wheel of Time: The Great Hunt - The Wheel", "Book 2", "The Wheel of Time: The Great Hunt - The Dragon", "The Great Hunt"},
		{"shared name, part tails", "Portal to Nova Roma", "Portal to Nova Roma: Athens - Part 1", "Nova Roma 4", "Portal to Nova Roma: Athens - Part 2", "Athens"},
		{"shared name of two parts", "Cosmere", "Cosmere: Mistborn - The Final Empire - Chapter 1", "Book 1", "Cosmere: Mistborn - The Final Empire - Chapter 38", "Mistborn - The Final Empire"},
		{"shared subtitle, generic folder", "Sapiens", "Sapiens: A Brief History (Part 1)", "Book 2", "Sapiens: A Brief History (Part 2)", "A Brief History"},
		// Tags that share nothing past the album but a marker: each file's own
		// chapter. An unrelated album yields to the book folder.
		{"only chapters past the album", "The Wheel of Time", "The Wheel of Time - Ravens", "The Eye of the World", "The Wheel of Time - The Peddler", "The Eye of the World"},
		{"only a part marker shared", "Dune", "Dune: Part 1 - Chapter 1", "Book 1", "Dune: Part 1 - Chapter 9", "Dune"},
		{"only a volume marker shared", "The Expanse", "The Expanse: Book 1 - Chapter 01", "Book 1", "The Expanse: Book 1 - Chapter 30", "The Expanse"},
		{"underscore chapters", "The Diary of a Nobody", "The Diary of a Nobody_Ch00", "Diary of a Nobody", "The Diary of a Nobody_Ch24", "The Diary of a Nobody"},
		{"underscore chapters as written", "secret_agent", "secret_agent_ch01", "Secret Agent", "secret_agent_ch13", "secret_agent"},
	}
	for _, tc := range cases {
		var m Metadata
		m.setTitle(tc.album, tc.title)
		var other *Metadata
		if tc.other != unread {
			other = &Metadata{}
			other.setTitle(tc.album, tc.other)
		}
		if got := m.PartsTitle(tc.path, other); got != tc.want {
			t.Errorf("%s: PartsTitle(%q, %q, %q, %q) = %q, want %q", tc.name, tc.album, tc.title, tc.path, tc.other, got, tc.want)
		}
	}
	for _, tc := range []struct {
		album, title, path string
		want               bool
	}{
		{"", "Prologue", "The Dragon Reborn", true},
		{"The Wheel of Time", "Ravens", "The Dragon Reborn", true},
		{"The Wheel of Time", "Chapter 1", "The Dragon Reborn", false},
		{"The Odyssey", "The Voyage", "Odyssey", false},
		{"The Expanse", "The Expanse: Leviathan Wakes", "Leviathan Wakes", false},
		{"", "The Dragon Reborn", "The Dragon Reborn", false},
		{"", "The Dragon Reborn", "the dragon reborn", false},
	} {
		var m Metadata
		m.setTitle(tc.album, tc.title)
		if got := m.PartNamed(tc.path); got != tc.want {
			t.Errorf("PartNamed(%q, %q in %q) = %v, want %v", tc.album, tc.title, tc.path, got, tc.want)
		}
	}
	if (*Metadata)(nil).PartNamed("Any") {
		t.Error("no tags: PartNamed = true")
	}
}

func TestIsGenericTitle(t *testing.T) {
	generic := []string{
		"", "Track 01", "track 1", "Chapter 12", "Part 3", "Disc 2",
		"CD 1", "CD1", "disc02", "01", "1", "  07. ", "Track 1 - 2",
		"０１", "٠٣", "Track ０１", // a number in other digits is still a number
	}
	for _, s := range generic {
		if !IsGenericTitle(s) {
			t.Errorf("IsGenericTitle(%q) = false, want true", s)
		}
	}
	// Real titles - including ones that merely start with a label word.
	real := []string{
		"A Christmas Carol", "Unsouled", "How to Train Your Dragon",
		"Part of Your World", "Chapter and Verse", "Track Changes", "CD Projekt",
		// Letters outside ASCII count (R-38): a title in another script names something.
		"Война и мир", "Пролог", "Глава 1", "第1章", "Метро 2033", "Część 1", "Ἰλιάς",
	}
	for _, s := range real {
		if IsGenericTitle(s) {
			t.Errorf("IsGenericTitle(%q) = true, want false", s)
		}
	}
}

// TestApplyTagsReleased: the date a raw tag keeps, month and day included, and
// nothing for a placeholder.
func TestApplyTagsReleased(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags fakeTags
		want string
	}{
		{"mp4 day", fakeTags{raw: map[string]interface{}{"\xa9day": "2010-08-31T07:00:00Z"}}, "2010-08-31"},
		{"id3 TDRC", fakeTags{raw: map[string]interface{}{"TDRC": "2014-03-04"}}, "2014-03-04"},
		{"id3 year", fakeTags{raw: map[string]interface{}{"TYER": "1968"}}, "1968"},
		{"placeholder", fakeTags{raw: map[string]interface{}{"TYER": "0000"}}, ""},
		{"placeholder then year", fakeTags{raw: map[string]interface{}{"date": "0000", "year": "1999"}}, "1999"},
	} {
		m := &Metadata{}
		applyTags(m, tc.tags)
		if m.Released != tc.want {
			t.Errorf("%s: released = %q, want %q", tc.name, m.Released, tc.want)
		}
	}
}

// TestReleaseDate pins how a date tag becomes the catalogue's YYYY[-MM[-DD]]:
// the leading date of an ISO date or timestamp, cut back to what is a real date,
// and nothing for placeholders.
func TestReleaseDate(t *testing.T) {
	cases := map[string]string{
		"2010":                 "2010",
		" 2010 ":               "2010",
		"2010-08":              "2010-08",
		"2010-08-31":           "2010-08-31",
		"2010-08-31T07:00:00Z": "2010-08-31",
		"2010-13-01":           "2010",
		"2010-02-30":           "2010-02",
		"2010/08/31":           "2010",
		"2010-08-31 07:00":     "2010-08-31",
		"12345":                "",
		"1283212800":           "",
		"0000":                 "",
		"0999":                 "",
		"9999-01-01":           "",
		"":                     "",
		"unknown":              "",
		"Aug 2010":             "",
	}
	for in, want := range cases {
		if got := ReleaseDate(in); got != want {
			t.Errorf("ReleaseDate(%q) = %q, want %q", in, got, want)
		}
	}
}
