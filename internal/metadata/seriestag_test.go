package metadata

import "testing"

// TestSplitSeriesTag (R-42, #100): a series tag ending in a marked position is
// the series and that position; a name that merely ends in a number, or holds a
// marker word without one, or a tag naming several series, stays whole.
func TestSplitSeriesTag(t *testing.T) {
	split := []struct {
		tag, series string
		pos         float64
	}{
		{"Discworld, book #40", "Discworld", 40},
		{"The Expanse, book #3", "The Expanse", 3},
		{"Series, Book 3", "Series", 3},
		{"Series Book 3", "Series", 3},
		{"Series #3", "Series", 3},
		{"Series, #3.5", "Series", 3.5},
		{"Series (Book 3)", "Series", 3},
		{"Series [Book 12]", "Series", 12},
		{"Series, Vol. 2", "Series", 2},
		{"Series VOLUME 7", "Series", 7},
		{"Series - Book 4", "Series", 4},
		{"Warhammer 40,000, Book 2", "Warhammer 40,000", 2},
		{"  Padded, book 1  ", "Padded", 1},
		{"Series (Book 3", "Series", 3},
		{"Series, No. 3", "Series", 3},
		{"Series:Book 3", "Series", 3},
		// Several series, as Audiobookshelf writes them: the first one's.
		{"Discworld #40; Moist von Lipwig #3", "Discworld", 40},
	}
	for _, c := range split {
		series, pos, ok := SplitSeriesTag(c.tag)
		if !ok || series != c.series || pos != c.pos {
			t.Errorf("SplitSeriesTag(%q) = %q, %v, %v; want %q, %v", c.tag, series, pos, ok, c.series, c.pos)
		}
	}
	whole := []string{
		"Warhammer 40,000", "Metro 2033", "Book of the New Sun", "The Book Thief",
		"Notebook 3", "Catch-22", "Book #3", "Vol. 2", "Discworld",
		"Series, Book 0",
		// The marker set off from the name in any script, "no" only as "no.".
		"Dobré víno 3", "Learning C# 2", "Mambo No 5",
		// Several series, the first with no position: the last number is the last one's.
		"Discworld; Moist von Lipwig #3", "Discworld, book #40, Rincewind, book #8", "Bk 1, Bk 2",
	}
	for _, tag := range whole {
		if series, pos, ok := SplitSeriesTag(tag); ok || series != tag || pos != 0 {
			t.Errorf("SplitSeriesTag(%q) = %q, %v, %v; want it whole", tag, series, pos, ok)
		}
	}
}

// TestApplyTagsSeriesWithPosition: the iTunes grouping "Discworld, book #40"
// (Libation's) indexes series Discworld at 40, from dhowden's tags and from
// ffprobe's; an explicit series-part tag still wins over the grouping's number.
func TestApplyTagsSeriesWithPosition(t *testing.T) {
	m := &Metadata{}
	applyTags(m, fakeTags{raw: map[string]interface{}{"\xa9grp": "Discworld, book #40"}}) // dhowden's MP4 key
	if m.Series != "Discworld" || m.SeriesIndex != 40 {
		t.Errorf("applyTags grouping = %q #%v, want Discworld #40", m.Series, m.SeriesIndex)
	}
	m = &Metadata{}
	applyProbe(m, &probeResult{Tags: map[string]string{"grouping": "Discworld, book #40"}})
	if m.Series != "Discworld" || m.SeriesIndex != 40 {
		t.Errorf("applyProbe grouping = %q #%v, want Discworld #40", m.Series, m.SeriesIndex)
	}
	m = &Metadata{}
	applyProbe(m, &probeResult{Tags: map[string]string{"series": "Omnibus, Book 9", "series-part": "1-3"}})
	if m.Series != "Omnibus" || m.SeriesIndex != 1 || m.SeriesIndexEnd != 3 {
		t.Errorf("series-part over the tag's number = %q %v-%v, want Omnibus 1-3", m.Series, m.SeriesIndex, m.SeriesIndexEnd)
	}
	m = &Metadata{}
	applyTags(m, fakeTags{raw: map[string]interface{}{"series": "Metro 2033"}})
	if m.Series != "Metro 2033" || m.SeriesIndex != 0 {
		t.Errorf("unmarked number = %q #%v, want Metro 2033 with no position", m.Series, m.SeriesIndex)
	}
}
