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

func TestGroupKey(t *testing.T) {
	// Parts of one book share a key, regardless of separator/padding.
	parts := []string{"Dungeon Born - 001.mp3", "Dungeon Born - 002.mp3", "Dungeon Born - 050.mp3"}
	want := GroupKey(parts[0])
	for _, p := range parts[1:] {
		if GroupKey(p) != want {
			t.Errorf("GroupKey(%q)=%q, want %q", p, GroupKey(p), want)
		}
	}
	// Pure-number parts collapse (the folder name is the title).
	if GroupKey("001.mp3") != GroupKey("002.mp3") {
		t.Error("pure-number parts should share a group key")
	}
	// Distinct titles must not collapse.
	if GroupKey("01 - Unsouled.m4b") == GroupKey("02 - Soulsmith.m4b") {
		t.Error("distinct titles should not share a group key")
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
