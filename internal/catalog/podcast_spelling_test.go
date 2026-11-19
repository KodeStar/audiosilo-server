package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

// PodcastShowSpelling reads a show path by Spelling's rule for a book: the
// show indexed in the other Unicode spelling, never case, never one the
// caller's scope refuses, never a guess between two, and never when the path
// as given is a folder of its own on disk that no scan has indexed yet.
func TestPodcastShowSpelling(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	nfc, nfd := "Café Show", "Café Show"
	indexShowAt(t, c, ctx, lib.ID, nfd, "")
	indexShowAt(t, c, ctx, lib.ID, "Example Show", "0123456789abcdef")
	refuse := func(string) bool { return false }
	for _, tc := range []struct {
		name, p, want string
		allow         func(string) bool
	}{
		{"the other spelling", nfc, nfd, nil},
		{"as indexed", nfd, nfd, nil},
		{"ASCII, no lookup", "Example Show", "Example Show", nil},
		{"no show reads as it", "Zoë Show", "Zoë Show", nil},
		{"never case", "CAFÉ SHOW", "CAFÉ SHOW", nil},
		{"refused by the scope", nfc, nfc, refuse},
		{"admitted by the scope", nfc, nfd, func(s string) bool { return s == nfd }},
	} {
		if got, err := c.PodcastShowSpelling(ctx, lib.ID, tc.p, tc.allow); err != nil || got != tc.want {
			t.Errorf("%s: %+q %v, want %+q", tc.name, got, err, tc.want)
		}
	}

	// A folder of its own in the spelling asked for (a disk that keeps the two
	// apart, its folder not indexed yet) is never taken for the indexed show
	// beside it. (Another library: the first's root listing is kept.)
	own, err := c.CreateLibrary(ctx, Library{Name: "Own", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(own.Root, nfc), 0o700); err != nil {
		t.Fatal(err)
	}
	indexShowAt(t, c, ctx, own.ID, nfd, "")
	if got, err := c.PodcastShowSpelling(ctx, own.ID, nfc, nil); err != nil || got != nfc {
		t.Errorf("own entry: %+q %v", got, err)
	}

	// Two shows that both read as a third spelling leave it as given.
	mixed := "Café Café"
	indexShowAt(t, c, ctx, lib.ID, "Café Café", "1111111111111111")
	indexShowAt(t, c, ctx, lib.ID, "Café Café", "2222222222222222")
	if got, err := c.PodcastShowSpelling(ctx, lib.ID, mixed, nil); err != nil || got != mixed {
		t.Errorf("two spellings: %+q %v", got, err)
	}
	if got, err := c.PodcastShowSpelling(ctx, lib.ID, mixed, func(s string) bool { return s == "Café Café" }); err != nil || got != "Café Café" {
		t.Errorf("one admitted of two: %+q %v", got, err)
	}
}
