package match

import "testing"

func TestSeriesKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"The Expanse", "theexpanse"},
		{"  the expanse! ", "theexpanse"},
		{"Thé Éxpanse", "theexpanse"},
		{"Mistborn: Era 2", "mistbornera2"},
		{"Ⅻ Kingdoms", "xiikingdoms"}, // NFKD spells the numeral out
		{"Война и мир", "воинаимир"},  // й decomposes; its breve goes, as in the console
		{"ﬁre", "fire"},               // NFKD splits the ligature
		{"第〇巻", "第〇巻"},                // every number, as \p{N}: Fold (digits only) would drop the 〇
		{"--", ""},
	} {
		if got := SeriesKey(tc.in); got != tc.want {
			t.Errorf("SeriesKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSameSeries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"He Who Fights With Monsters", "He Who Fights WIth Monsters", true},
		{"The Expanse", "thé  expanse!", true},
		{"The Expanse", "Expanse", false}, // nothing fuzzier: an article counts
		{"!!", "!!", true},
		{"!!", "??", false}, // no key: only the same name
		{"", "!!", false},
		{"", "", false}, // blank is no series
		{"Dune", "", false},
	} {
		if got := SameSeries(tc.a, tc.b); got != tc.want {
			t.Errorf("SameSeries(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
