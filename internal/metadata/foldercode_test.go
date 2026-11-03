package metadata

import "testing"

func TestFolderCodePosition(t *testing.T) {
	for _, tc := range []struct {
		name, series string
		want, end    float64 // want 0 = none; end 0 = a single position
	}{
		{"DH06 - Land of Magic", "Dragon Heart", 6, 0},
		{"BDM01 - Oathbound Healer", "Beneath the Dragoneye Moons", 1, 0},
		{"FWM01 - He Who Fights With Monsters - Book 1", "He Who Fights With Monsters", 1, 0},
		{"DF15.5 Brief Cases", "Dresden Files", 15.5, 0},
		{"PH15 - 15", "Primal Hunter", 15, 0},
		// A range (an omnibus) keeps its end.
		{"LAM05-06", "Legend of the Arch Magus", 5, 6},
		{"LAM05–06 - Omnibus", "Legend of the Arch Magus", 5, 6},
		{"DF15.5-16", "Dresden Files", 15.5, 16},
		{"LAM06-05", "Legend of the Arch Magus", 0, 0},
		{"LAM05-05", "Legend of the Arch Magus", 0, 0},
		{"LAM05-06x", "Legend of the Arch Magus", 0, 0},
		{"LAM05-0612", "Legend of the Arch Magus", 0, 0},
		{"DH06 - 07 Land", "Dragon Heart", 6, 0},
		{"DH06-Land", "Dragon Heart", 6, 0},
		{"HALO 07-08", "Halo", 0, 0},
		{"EE07 - Forged in Blood Part 2", "Emperor's Edge", 7, 0},
		{"RADT01 - Reborn as a Demonic Tree", "Reborn as a Demonic Tree", 1, 0},
		{"WT01", "World-Tree", 1, 0},
		{"C04 - Skysworn", "Cradle", 4, 0},
		{"dh06 - land of magic.m4b", "Dragon Heart", 6, 0},
		// A franchise's own numbering: the code isn't the series' initials.
		{"HALO 07 - Evolutions (M4B)", "Halo", 0, 0},
		{"MB01 - The Final Empire", "Mistborn", 0, 0},
		{"AF05 - Legion's Fifth Vault", "Divine Apostasy", 0, 0},
		{"CD1", "02 - How to Be a Pirate", 0, 0},
		// No code, or not one.
		{"01. The Enchanted Wood", "The Faraway Tree", 0, 0},
		{"Brief Cases", "Dresden Files", 0, 0},
		{"DH06x - Land", "Dragon Heart", 0, 0},
		{"DF15.5x Brief Cases", "Dresden Files", 0, 0},
		{"DF15. Brief Cases", "Dresden Files", 15, 0},
		{"DH1234 - Land", "Dragon Heart", 0, 0},
		{"DH0 - Prologue", "Dragon Heart", 0, 0},
		{"DH1999", "Dragon Heart", 0, 0},
		{"DH06", "", 0, 0},
	} {
		got, end, ok := FolderCodePosition(tc.name, tc.series)
		if ok != (tc.want != 0) || got != tc.want || end != tc.end {
			t.Errorf("FolderCodePosition(%q, %q) = %v, %v, %v; want %v, %v", tc.name, tc.series, got, end, ok, tc.want, tc.end)
		}
	}
}
