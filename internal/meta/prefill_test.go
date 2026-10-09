package meta

import "testing"

func TestSearchPrefill(t *testing.T) {
	// The leaf as named: metaserve, not the server, reads "Sharpe - 08 - " as volume 8.
	const sharpe = "Bernard Cornwell/Richard Sharpe/Sharpe - 08 - Sharpe's Eagle"
	const leaf = "Sharpe - 08 - Sharpe's Eagle Bernard Cornwell"
	for _, c := range []struct {
		name, title, author, series, path string
		isFolder                          bool
		want                              string
	}{
		{"good tags stay", "Sharpe's Eagle", "Bernard Cornwell", "Richard Sharpe", sharpe, true,
			"Sharpe's Eagle Bernard Cornwell"},
		{"good tags stay beside a different folder name", "The Way of Kings", "Brandon Sanderson", "",
			"Fantasy/Sanderson/01 - Way of Kings", true, "The Way of Kings Brandon Sanderson"},
		{"swapped: the title is the author folder", "Bernard Cornwell", "Sharpe's Eagle (Sharpe 08)",
			"Richard Sharpe", sharpe, true, leaf},
		{"swapped: the author is the path's title", "Cornwell, B.", "Sharpe's Eagle (Sharpe 08)", "Richard Sharpe",
			"Bernard Cornwell/Richard Sharpe/08 - Sharpe's Eagle", true, "Sharpe's Eagle Bernard Cornwell"},
		{"junk title: a track number", "Track 01", "Bernard Cornwell", "", sharpe, true, leaf},
		{"junk title: unknown", "Unknown", "Bernard Cornwell", "", sharpe, true, leaf},
		{"no title tag", "", "Bernard Cornwell", "", sharpe, true, leaf},
		{"junk author alone", "Sharpe's Eagle", "Unknown Artist", "", sharpe, true,
			"Sharpe's Eagle Bernard Cornwell"},
		{"a single file in an author folder", "03", "Agatha Christie", "",
			"Agatha Christie/The ABC Murders.m4b", false, "The ABC Murders Agatha Christie"},
		{"a volume folder names the series", "Track 01", "Brandon Sanderson", "",
			"Brandon Sanderson/Stormlight Archive/03", true, "Stormlight Archive 3 Brandon Sanderson"},
		{"junk tags with a path that says nothing keep the tags", "Track 01", "Unknown", "",
			"Track 01.mp3", false, "Track 01 Unknown"},
		{"no author folder: the title only", "Unknown", "", "", "Sharpe's Eagle", true, "Sharpe's Eagle"},
	} {
		if got := SearchPrefill(c.title, c.author, c.series, c.path, c.isFolder); got != c.want {
			t.Errorf("%s: SearchPrefill(%q, %q) = %q, want %q", c.name, c.title, c.author, got, c.want)
		}
	}
}
