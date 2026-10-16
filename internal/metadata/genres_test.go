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
		{" Science  Fiction , Space Opera ;; ", []string{"Science Fiction", "Space Opera"}},
		{"Mystery\x00Thriller", []string{"Mystery", "Thriller"}},
		// The placeholders say only that it is an audiobook, in any case.
		{"Audiobook", nil},
		{"AUDIOBOOKS; Audio Book; speech; Spoken Word; other", nil},
		{"Audiobook;Fantasy", []string{"Fantasy"}},
		// A genre once, its first spelling kept.
		{"Fantasy; fantasy; FANTASY; Horror", []string{"Fantasy", "Horror"}},
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
	applyProbe(m, &probeResult{Tags: map[string]string{"genre": "Fantasy; Epic"}})
	if !reflect.DeepEqual(m.Genres, []string{"Fantasy", "Epic"}) {
		t.Errorf("probe: Genres = %q, want [Fantasy Epic]", m.Genres)
	}
}
