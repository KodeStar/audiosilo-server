package metadata

import "testing"

// TestPositionInTitle: the titles a series' books carry their number in (the He
// Who Fights with Monsters golden case, tagged "He Who Fights With Monsters"),
// and the ones that must give none: a series named like the start of a word or
// title with a number of its own ("Catch-22", "2001: A Space Odyssey"), a year, an
// ordinal, a number not right after the name.
func TestPositionInTitle(t *testing.T) {
	t.Parallel()
	const hw = "He Who Fights With Monsters"
	for _, tc := range []struct {
		title, series string
		want          float64
	}{
		{"He Who Fights with Monsters 4: A LitRPG Adventure (He Who Fights with Monsters, Book 4)", hw, 4},
		{"He Who Fights with Monsters 10 - A LitRPG Adventure (He Who Fights with Monsters, Book 10)", hw, 10},
		{"He Who Fights with Monsters 13: A LitRPG Adventure: He Who Fights with Monsters, Book 13", hw, 13},
		{"He Who Fights with Monsters 6", hw, 6},
		{"He Who Fights WIth Monsters 2: A LitRPG Adventure", hw, 2},
		{"He Who Fights With Monsters 2", "He Who Fights WIth Monsters", 2},
		{"The Expanse, Book 3", "The Expanse", 3},
		{"The Expanse Vol. 2 - Caliban's War", "The Expanse", 2},
		{"Discworld #8: Guards! Guards!", "Discworld", 8},
		{"Thé Expanse 7 (Unabridged)", "the expanse", 7},
		{"Café 3", "Café", 3}, // a decomposed title (NFD, a macOS file name)
		{"Café 4", "Café", 4},
		{"Expanse 2.5: The Churn", "Expanse", 2.5},
		{"Bobiverse 3", "Bobiverse", 3},
		{"Bobiverse 4 \u2013 Heaven's River", "Bobiverse", 4},
		{"Bobiverse 5 [Dramatized]", "Bobiverse", 5},

		// None.
		{"He Who Fights with Monsters: A LitRPG Adventure", hw, 0}, // book 1, unnumbered
		{"Catch-22", "Catch", 0},
		{"Catch-22 2", "Catch", 0},
		{"Catcher 3", "Catch", 0},
		{"2001: A Space Odyssey", "Space Odyssey", 0},
		{"2001: A Space Odyssey", "2001", 0},
		{"Monsters 1999", "Monsters", 0},
		{"Monsters 4th Edition", "Monsters", 0},
		{"Monsters 4.Edition", "Monsters", 0},
		{"Monsters of 4 Kingdoms", "Monsters", 0},
		{"Monsters 0", "Monsters", 0},
		{"Jack Reacher 61 Hours", "Jack Reacher", 0},
		{"Dork Diaries 1-12", "Dork Diaries", 0},
		{"Dork Diaries 1 \u2013 3", "Dork Diaries", 0},
		{"Saga 1, 2 and 3", "Saga", 0},
		{"Artemis Fowl 1 - Artemis Fowl", "Artemis Fowl", 1},
		{"Expeditionary Force 06", "Expeditionary Force", 6},
		{"Spacers, Part 2", "Spacers", 2},
		{"Unintended Cultivator, Volume 2", "Unintended Cultivator", 2},
		{"Monsters 4 A LitRPG Adventure", "Monsters", 0},
		{"Leviathan Wakes (The Expanse 1)", "The Expanse", 0},
		{"Anything 3", "", 0},
		{"Monsters", "Monsters", 0},
	} {
		got, ok := PositionInTitle(tc.title, tc.series)
		if got != tc.want || ok != (tc.want > 0) {
			t.Errorf("PositionInTitle(%q, %q) = %v, %v; want %v", tc.title, tc.series, got, ok, tc.want)
		}
	}
}
