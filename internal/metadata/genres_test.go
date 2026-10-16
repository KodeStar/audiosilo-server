package metadata

import (
	"reflect"
	"testing"
)

func TestSplitGenres(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"Fantasy", []string{"Fantasy"}},
		{"Fantasy;Epic", []string{"Fantasy", "Epic"}},
		// Cleaning (trim, spacing, repeats) is catalog.CleanLabels': the parts as spelled.
		{" Science  Fiction ; Space Opera ;; ", []string{" Science  Fiction ", " Space Opera "}},
		// A comma is part of a store's category name, not a joiner.
		{"Mystery, Thriller & Suspense", []string{"Mystery, Thriller & Suspense"}},
		{"Mystery\x00Thriller", []string{"Mystery", "Thriller"}},
		// The placeholders say only that it is an audiobook, in any case.
		{"Audiobook", nil},
		{"AUDIOBOOKS; Audio Book; speech; Spoken Word; other", nil},
		{"Audiobook;Fantasy", []string{"Fantasy"}},
		{" audio   BOOK ; Fantasy", []string{" Fantasy"}},
		{"Fantasy; fantasy", []string{"Fantasy", " fantasy"}},
		// A word that merely contains a placeholder is a genre.
		{"Other Worlds", []string{"Other Worlds"}},
	} {
		if got := SplitGenres(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("SplitGenres(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestGenresFromTagsAndProbe: the genre tag is read by both readers, ffprobe's
// over dhowden's when it gives any (as for the other tags), and a placeholder
// from one doesn't hide the other's real genres.
func TestGenresFromTagsAndProbe(t *testing.T) {
	t.Parallel()
	m := &Metadata{}
	applyTags(m, fakeTags{genre: "Audiobook;Fantasy"})
	if !reflect.DeepEqual(m.Genres, []string{"Fantasy"}) {
		t.Fatalf("tags: Genres = %q, want [Fantasy]", m.Genres)
	}
	applyProbe(m, &probeResult{Tags: map[string]string{"genre": "Audiobook"}})
	if !reflect.DeepEqual(m.Genres, []string{"Fantasy"}) {
		t.Errorf("a placeholder from ffprobe replaced the tag's genres: %q", m.Genres)
	}
	applyProbe(m, &probeResult{Tags: map[string]string{"genre": "Fantasy;Epic"}})
	if !reflect.DeepEqual(m.Genres, []string{"Fantasy", "Epic"}) {
		t.Errorf("probe: Genres = %q, want [Fantasy Epic]", m.Genres)
	}
}
