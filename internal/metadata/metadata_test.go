package metadata

import "testing"

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
		layout   string
		rel      string
		isFolder bool
		want     Metadata
	}{
		{
			name:   "books_in_folder file",
			layout: LayoutBooksInFolder,
			rel:    "Will Wight/Cradle/01 - Unsouled.m4b",
			want:   Metadata{Title: "Unsouled", Series: "Cradle", Author: "Will Wight", SeriesIndex: 1},
		},
		{
			name:     "chapters_in_folder folder",
			layout:   LayoutChaptersInFolder,
			rel:      "Brandon Sanderson/Mistborn/02 - The Well of Ascension",
			isFolder: true,
			want:     Metadata{Title: "The Well of Ascension", Series: "Mistborn", Author: "Brandon Sanderson", SeriesIndex: 2},
		},
		{
			name:   "flat carries no hierarchy",
			layout: LayoutFlat,
			rel:    "01 - Unsouled.m4b",
			want:   Metadata{Title: "Unsouled", SeriesIndex: 1},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeriveFromPath(c.layout, c.rel, c.isFolder)
			if got.Title != c.want.Title || got.Author != c.want.Author ||
				got.Series != c.want.Series || got.SeriesIndex != c.want.SeriesIndex {
				t.Errorf("DeriveFromPath = %+v want %+v", *got, c.want)
			}
		})
	}
}

func TestIsAudio(t *testing.T) {
	if !IsAudio("foo.m4b") || !IsAudio("BAR.MP3") {
		t.Error("expected audio extensions recognized")
	}
	if IsAudio("cover.jpg") || IsAudio("notes.txt") {
		t.Error("expected non-audio rejected")
	}
}
