package metadata

import (
	"slices"
	"testing"
)

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

func TestDiscOrder(t *testing.T) {
	t.Parallel()
	want := []string{
		"Book/CD01", // reads as "CD1"; the path breaks the tie
		"Book/CD1",
		"Book/cd 2", // case and spaces don't count
		"Book/CD10",
		"Book/Disc 3",
	}
	got := slices.Clone(want)
	slices.Reverse(got)
	slices.SortFunc(got, DiscOrder)
	if !slices.Equal(got, want) {
		t.Fatalf("order = %q\nwant %q", got, want)
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"Part 2", "Part 10", -1},
		{"Disc 01", "Disc 1", 0},
		{"CD2", "cd 2", 0},
		{"a10b", "a9c", 1},
		{"Track", "Track 1", -1},
	} {
		if got := naturalCompare(tc.a, tc.b); got != tc.want {
			t.Errorf("naturalCompare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
