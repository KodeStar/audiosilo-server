package metadata

import "testing"

func TestIsDiscFolder(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"CD1", "CD 1", "cd01", "Disc 2", "disk3", "CD"} {
		if !IsDiscFolder(s) {
			t.Errorf("IsDiscFolder(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"The Big Book", "CD Projekt", "Disco Inferno", "Author Name"} {
		if IsDiscFolder(s) {
			t.Errorf("IsDiscFolder(%q) = true, want false", s)
		}
	}
}
