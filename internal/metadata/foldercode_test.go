package metadata

import "testing"

func TestFolderCodePosition(t *testing.T) {
	for _, tc := range []struct {
		name, series string
		want         float64 // 0 = none
	}{
		{"DH06 - Land of Magic", "Dragon Heart", 6},
		{"BDM01 - Oathbound Healer", "Beneath the Dragoneye Moons", 1},
		{"FWM01 - He Who Fights With Monsters - Book 1", "He Who Fights With Monsters", 1},
		{"DF15.5 Brief Cases", "Dresden Files", 15.5},
		{"PH15 - 15", "Primal Hunter", 15},
		{"LAM05-06", "Legend of the Arch Magus", 5},
		{"EE07 - Forged in Blood Part 2", "Emperor's Edge", 7},
		{"RADT01 - Reborn as a Demonic Tree", "Reborn as a Demonic Tree", 1},
		{"WT01", "World-Tree", 1},
		{"C04 - Skysworn", "Cradle", 4},
		{"dh06 - land of magic.m4b", "Dragon Heart", 6},
		// A franchise's own numbering: the code isn't the series' initials.
		{"HALO 07 - Evolutions (M4B)", "Halo", 0},
		{"MB01 - The Final Empire", "Mistborn", 0},
		{"AF05 - Legion's Fifth Vault", "Divine Apostasy", 0},
		{"CD1", "02 - How to Be a Pirate", 0},
		// No code, or not one.
		{"01. The Enchanted Wood", "The Faraway Tree", 0},
		{"Brief Cases", "Dresden Files", 0},
		{"DH06x - Land", "Dragon Heart", 0},
		{"DH0 - Prologue", "Dragon Heart", 0},
		{"DH1999", "Dragon Heart", 0},
		{"DH06", "", 0},
	} {
		got, ok := FolderCodePosition(tc.name, tc.series)
		if ok != (tc.want != 0) || got != tc.want {
			t.Errorf("FolderCodePosition(%q, %q) = %v, %v; want %v", tc.name, tc.series, got, ok, tc.want)
		}
	}
}
