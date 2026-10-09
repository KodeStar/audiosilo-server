package metadata

import "testing"

func TestChapterTitle(t *testing.T) {
	for raw, want := range map[string]string{
		// Real titles stay as they are.
		"The Shadow of the Past": "The Shadow of the Past",
		"3.5 The Interlude":      "3.5 The Interlude",
		"Opening Credits":        "Opening Credits",
		"Part of Your World":     "Part of Your World",
		"1984 and After":         "1984 and After",
		// A numbered chapter or part keeps its own number.
		"Chapter 10":  "Chapter 10",
		"Chapter 024": "Chapter 024",
		"Part 7":      "Part 7",
		"chapter12":   "chapter12",
		// Names nothing: the console says "Chapter N" by its place.
		"":                "",
		"   ":             "",
		"024":             "",
		"01.":             "",
		"1-02":            "",
		"Track 01":        "",
		"Track 2-1":       "",
		"CD1 Track 03":    "",
		"Disc 2":          "",
		"TRACK01.MP3":     "",
		"024.mp3":         "",
		"track_07_64kb":   "",
		"  Track 3  ":     "",
		"Bonus Track 01":  "Bonus Track 01",
		"Track 01 - Home": "Track 01 - Home",
		// Filename-shaped titles are tidied like the player's prettifyChapterTitle.
		"01_the_hobbit_ch1.mp3":     "01 the hobbit ch1",
		"wonderland_ch_01_64kb.mp3": "wonderland ch 01",
		"story_part2_128 kbps.mp3":  "story part2",
		"The Hobbit.m4b":            "The Hobbit",
		"Chapter 64":                "Chapter 64",
		"_64kb.mp3":                 "64kb",
	} {
		if got := ChapterTitle(raw); got != want {
			t.Errorf("ChapterTitle(%q) = %q, want %q", raw, got, want)
		}
	}
}

// prettifyChapterTitle answers as the player's does (its own test cases).
func TestPrettifyChapterTitleMatchesThePlayer(t *testing.T) {
	for raw, want := range map[string]string{
		"chapter-01.mp3":            "chapter-01",
		"part_one":                  "part one",
		"a__b___c.opus":             "a b c",
		"Mother-in-law":             "Mother-in-law",
		"intro_32k.mp3":             "intro",
		"64kb_wonderland_ch_01.mp3": "64kb wonderland ch 01",
		"":                          "",
		"   ":                       "   ",
		"_.mp3":                     "_.mp3",
		"TRACK01.MP3":               "TRACK01",
	} {
		if got := prettifyChapterTitle(raw); got != want {
			t.Errorf("prettifyChapterTitle(%q) = %q, want %q", raw, got, want)
		}
	}
}
