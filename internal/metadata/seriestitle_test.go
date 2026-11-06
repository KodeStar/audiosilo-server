package metadata

import "testing"

// probed is the Metadata a probe answering these tags gives (applyProbe), as a
// scan reads an M4B: the tag shapes below are real ones, from the LibraForge
// benchmark's libraries.
func probed(tags map[string]string) *Metadata {
	m := &Metadata{}
	applyProbe(m, &probeResult{Tags: normalizeTags(tags)})
	return m
}

// TestTitleNotSeries pins the title of a book whose album tag names only its
// series: the title tag or the path's title takes over, and every layout where
// the album IS the book's title keeps it.
func TestTitleNotSeries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, path string
		several    bool
		tags       map[string]string
		want       string
	}{
		// "Author/Author; Series; Book NN; Title": album = series, title = book.
		{"leaf layout: title tag", "Mark Tufo/Mark Tufo; Indian Hill; Book 04; From The Ashes", false,
			map[string]string{"genre": "Audiobook", "album": "Indian Hill", "album_artist": "Mark Tufo", "artist": "Mark Tufo", "title": "From The Ashes", "date": "2013"},
			"From The Ashes"},
		{"leaf layout: first book agrees with the leaf", "J. Zachary Pike/J. Zachary Pike; The Dark Profit Saga; Book 01; Orconomics", false,
			map[string]string{"genre": "Audiobook", "album": "The Dark Profit Saga", "album_artist": "J. Zachary Pike", "artist": "J. Zachary Pike", "title": "Orconomics", "date": "2016"},
			"Orconomics"},
		{"leaf layout: 3D6", "Robert Bevan/Robert Bevan; D6; Book 03; 3D6", false,
			map[string]string{"genre": "Audiobook", "album": "D6", "album_artist": "Robert Bevan", "artist": "Robert Bevan", "title": "3D6", "date": "2016"},
			"3D6"},
		{"leaf layout: book 1 named for its series keeps it", "Mark Tufo/Mark Tufo; Indian Hill; Book 01", false,
			map[string]string{"genre": "Audiobook", "album": "Indian Hill", "album_artist": "Mark Tufo", "artist": "Mark Tufo", "title": "Indian Hill", "date": "2012"},
			"Indian Hill"},
		// "Author/Series/Book N": the folder holding the book is the series.
		{"series folder: series and number", "Eric Vall/Without Law/Book 17", false,
			map[string]string{"title": "Without Law 17", "artist": "Eric Vall", "album": "Without Law", "track": "17"},
			"Without Law 17"},
		{"series folder: book 1 tagged series 1 keeps the album", "Logan Jacobs/Arena Road/Book 1", false,
			map[string]string{"title": "Arena Road 1", "artist": "Logan Jacobs", "album": "Arena Road", "genre": "Audiobook"},
			"Arena Road"},
		{"series folder: book 1's title tag is a chapter's", "Cassius Lange/Tower Apocalypse/Book 1", false,
			map[string]string{"title": "End Credits", "series": "Tower Apocalypse", "series-part": "1", "artist": "Cassius Lange, Ryan Tang", "album_artist": "Cassius Lange", "album": "Tower Apocalypse"},
			"Tower Apocalypse"},
		{"series folder: book 1 named in the leaf, tagged series 1", "Dante King/Dragon Born/Book 1 - The Shifter's Hoard", false,
			map[string]string{"title": "Dragon Born 1", "artist": "Dante King", "album": "Dragon Born"},
			"The Shifter's Hoard"},
		{"series folder: a tag carrying the series yields to the leaf", "Terry Mancour/Spellmonger/Book 2 - Warmage", false,
			map[string]string{"title": "002 - Spellmonger - Book 002 - Warmage", "artist": "Terry Mancour", "genre": "Audiobook", "date": "2017", "album": "Spellmonger", "track": "2"},
			"Warmage"},
		{"several files: a track name yields to the leaf", "Mark Tufo/Indian Hill/Book 3 - Bastion", true,
			map[string]string{"title": "Indian Hill 03", "artist": "Mark Tufo", "album": "Indian Hill"},
			"Bastion"},
		{"several files: the leaf before the first file's title", "Mark Tufo/Indian Hill/Book 3 - Bastion", true,
			map[string]string{"title": "Prologue", "artist": "Mark Tufo", "album": "Indian Hill"},
			"Bastion"},
		// The album IS the title: no change.
		{"the title tag already won (Dungeon Crawler Carl)", "Matt Dinniman/Dungeon Crawler Carl/Book 8 - A Parade of Horribles", false,
			map[string]string{"title": "A Parade of Horribles - Dungeon Crawler Carl, Book 8", "artist": "Matt Dinniman", "album_artist": "Matt Dinniman", "album": "A Parade of Horribles", "track": "1"},
			"A Parade of Horribles - Dungeon Crawler Carl, Book 8"},
		{"album is the title (Critical Failures)", "Robert Bevan/Caverns and Creatures/Book 6 - Critical Failures VI", false,
			map[string]string{"title": "Critical Failures VI: Caverns and Creatures, Book 6", "artist": "Robert Bevan", "album": "Critical Failures VI: Caverns and Creatures, Book 6", "genre": "Audiobook"},
			"Critical Failures VI: Caverns and Creatures, Book 6"},
		{"album is the title, the leaf only bracketed", "Pierce Brown/Red Rising [Dramatized]/Book 1 - (Part 1 of 2)", false,
			map[string]string{"subtitle": "Red Rising, Book 1", "series-part": "1", "series": "Red Rising", "album_artist": "Pierce Brown", "album": "Red Rising (Part 1 of 2) (Dramatized Adaptation)"},
			"Red Rising (Part 1 of 2) (Dramatized Adaptation)"},
		{"series named for its first book, the leaf names it too", "Frank Herbert/Dune/Dune", false,
			map[string]string{"title": "Dune - Part 1", "series": "Dune", "album": "Dune"},
			"Dune"},
		{"the title tag already won (bookTitle)", "Seth Ring/The Titan/Book 9 - Nova Terra - Catalyst", false,
			map[string]string{"title": "Nova Terra: Catalyst: The Titan Series, Book 9 (Unabridged)", "artist": "Seth Ring", "album": "Nova Terra: Catalyst (Unabridged)"},
			"Nova Terra: Catalyst: The Titan Series, Book 9 (Unabridged)"},
		{"album over a series tag of another name", "Brandon Sanderson/Mistborn/Book 1 - The Final Empire", false,
			map[string]string{"title": "Chapter 1", "series": "Mistborn", "album": "The Final Empire"},
			"The Final Empire"},
		{"named for its series, the leaf beside the author", "Frank Herbert/Frank Herbert - Dune", false,
			map[string]string{"title": "Dune", "series": "Dune", "series-part": "1", "album": "Dune", "artist": "Frank Herbert"},
			"Dune"},
		{"named for its series, the leaf after a year", "Frank Herbert/Dune/1965 - Dune", false,
			map[string]string{"title": "Dune", "series": "Dune", "album": "Dune"},
			"Dune"},
		{"named for its series, the leaf beside the author and the series 1", "Pierce Brown/Pierce Brown - Red Rising 01 - Red Rising", false,
			map[string]string{"title": "Red Rising", "series": "Red Rising", "album": "Red Rising", "artist": "Pierce Brown"},
			"Red Rising"},
		{"a leaf part beyond the author is the book's", "Dante King/Divine Arsenal/Book 1 - Divine Arsenal - Dual Weapon Cultivation", false,
			map[string]string{"title": "Divine Arsenal", "album": "Divine Arsenal", "artist": "Dante King"},
			"Divine Arsenal - Dual Weapon Cultivation"},
		{"a disc's folder is the book, not its series", "J.K. Rowling/Bastion/CD1", true,
			map[string]string{"title": "The Boy Who Lived", "album": "Bastion"},
			"Bastion"},
		// "C02" is a volume marker (splitSeriesIndex's "c" prefix), no title.
		{"series folder: a C-numbered leaf", "Will Wight/Cradle/C02", false,
			map[string]string{"title": "Cradle 2", "album": "Cradle"},
			"Cradle 2"},
	}
	for _, c := range cases {
		md := probed(c.tags)
		if got := TitleNotSeries(md.Title, md, c.path, true, c.several); got != c.want {
			t.Errorf("%s: TitleNotSeries(%q) = %q, want %q", c.name, md.Title, got, c.want)
		}
	}
	// A title not from the album (no tags at all, a path's) is left alone.
	if got := TitleNotSeries("Indian Hill", &Metadata{}, "Mark Tufo/Indian Hill/Book 4", true, false); got != "Indian Hill" {
		t.Errorf("a title with no album tag = %q, want it kept", got)
	}
	if got := TitleNotSeries("Indian Hill", nil, "Mark Tufo/Indian Hill/Book 4", true, false); got != "Indian Hill" {
		t.Errorf("no tags = %q, want it kept", got)
	}
	// A tag's position decides book 1 over the path's, and a later book takes
	// its title tag.
	md := probed(map[string]string{"title": "End Credits", "album": "Tower Apocalypse"})
	if got := TitleNotSeries(md.Title, md, "Cassius Lange/Tower Apocalypse/Book 2", true, false); got != "End Credits" {
		t.Errorf("book 2 = %q, want its title tag", got)
	}
	md = probed(map[string]string{"title": "End Credits", "album": "Tower Apocalypse", "series-part": "1"})
	if got := TitleNotSeries(md.Title, md, "Cassius Lange/Tower Apocalypse/Book 2", true, false); got != "Tower Apocalypse" {
		t.Errorf("tagged position 1 = %q, want the album kept", got)
	}
}

func TestLeafSeries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		leaf, series, title string
		volume              float64
		ok                  bool
	}{
		{"Mark Tufo; Indian Hill; Book 04; From The Ashes", "Indian Hill", "From The Ashes", 4, true},
		{"Mark Tufo; Indian Hill; Book 01", "Indian Hill", "", 1, true},
		{"Aaron Oster; The Rules; Book 01-03", "The Rules", "", 1, true},
		{"B.V. Larson; Star Force; Book XX; Army of One", "Star Force", "Army of One", 0, true},
		{"A. Writer; Series; Book 2.5; Title; With a Semicolon", "Series", "Title; With a Semicolon", 2.5, true},
		{"Albert Camus; The Stranger", "", "", 0, false},
		{"Amir Levine; Attached; The New Science of Adult Attachment", "", "", 0, false},
		{"Codename; Freedom - Book 001 - Survive Week One", "", "", 0, false},
		{"New Era Online; Life Reset - Book 001 - New Era Online; Life Reset", "", "", 0, false},
		{"DESTINY;DIVINE", "", "", 0, false},
		{"Plain Title", "", "", 0, false},
	}
	for _, c := range cases {
		s, title, volume, ok := LeafSeries(c.leaf)
		if s != c.series || title != c.title || volume != c.volume || ok != c.ok {
			t.Errorf("LeafSeries(%q) = %q, %q, %v, %v; want %q, %q, %v, %v", c.leaf, s, title, volume, ok,
				c.series, c.title, c.volume, c.ok)
		}
	}
}

// TestMaybeSeriesTitle pins which stored readings a scan reads again: a title
// naming its series, or a series its path states, unless its leaf names it so.
func TestMaybeSeriesTitle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path, title, series string
		want                bool
	}{
		{"Mark Tufo/Mark Tufo; Indian Hill; Book 04; From The Ashes", "Indian Hill", "", true},
		{"Eric Vall/Without Law/Book 17", "Without Law", "Without Law", true},
		{"Some One/Book 4", "Discworld", "Discworld", true},
		{"Some One/Book 4", "Discworld (Unabridged)", "Discworld", true},
		{"Mark Tufo/Mark Tufo; Indian Hill; Book 04; From The Ashes", "From The Ashes", "", false},
		{"Frank Herbert/Dune/Dune", "Dune", "Dune", false},
		{"Matt Dinniman/Dungeon Crawler Carl/Book 8 - A Parade of Horribles", "A Parade of Horribles", "Dungeon Crawler Carl", false},
		{"Eric Vall/Without Law/Book 17", "", "Without Law", false},
		{"J.K. Rowling/Bastion/CD1", "Bastion", "", false},
		{"Frank Herbert/Frank Herbert - Dune", "Dune", "Dune", false},
		{"Frank Herbert/Dune/1965 - Dune", "Dune", "Dune", false},
		{"Eric Vall/Without Law/Book 10 - Without Law 10", "Without Law", "Without Law", true},
	}
	for _, c := range cases {
		if got := MaybeSeriesTitle(c.path, true, c.title, c.series); got != c.want {
			t.Errorf("MaybeSeriesTitle(%q, %q, %q) = %v, want %v", c.path, c.title, c.series, got, c.want)
		}
	}
}
