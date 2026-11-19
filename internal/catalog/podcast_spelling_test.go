package catalog

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
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
		{"as indexed, refused by the scope", nfd, nfd, refuse},
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

	// A show at the path given that the scope refuses counts as none: the
	// answer is the one a caller who can't see it gets with it gone.
	indexShowAt(t, c, ctx, lib.ID, nfc, "3333333333333333")
	for _, tc := range []struct {
		name, want string
		allow      func(string) bool
	}{
		{"both seen", nfc, nil},
		{"only the other seen", nfd, func(s string) bool { return s == nfd }},
		{"neither seen", nfc, refuse},
	} {
		if got, err := c.PodcastShowSpelling(ctx, lib.ID, nfc, tc.allow); err != nil || got != tc.want {
			t.Errorf("a show at the path given, %s: %+q %v, want %+q", tc.name, got, err, tc.want)
		}
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

// UnfollowPodcast reads the caller's own follows for a path's other Unicode
// spelling: the one follow of theirs that reads as it goes (no show indexed
// there, so none their scope could spell it by), never another user's, never
// a guess between two, and never when the path as given is a folder of its
// own on disk.
func TestUnfollowPodcastByTheOtherSpelling(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	nfc, nfd := "Café Show", "Café Show"
	user, other := seedUserNamed(t, c, ctx, "u"), seedUserNamed(t, c, ctx, "o")
	follows := func(lib, user int64) []string {
		t.Helper()
		rows, err := queryRows(ctx, c.db, func(r *sql.Rows, s *string) error { return r.Scan(s) },
			`SELECT show_path FROM podcast_follows WHERE library_id=? AND user_id=? ORDER BY show_path`, lib, user)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	for _, u := range []int64{user, other} {
		if err := c.FollowPodcast(ctx, u, lib.ID, nfd, "0123456789abcdef"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.UnfollowPodcast(ctx, user, lib.ID, nfc); err != nil {
		t.Fatal(err)
	}
	if got := follows(lib.ID, user); len(got) != 0 {
		t.Errorf("own follow by the other spelling: %+q", got)
	}
	if got := follows(lib.ID, other); !slices.Equal(got, []string{nfd}) {
		t.Errorf("another user's follow: %+q", got)
	}

	// Two follows of theirs that both read as a third spelling stay.
	a, b := "Café Café", "Café Café"
	for _, p := range []string{a, b} {
		if err := c.FollowPodcast(ctx, user, lib.ID, p, "1111111111111111"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.UnfollowPodcast(ctx, user, lib.ID, "Café Café"); err != nil {
		t.Fatal(err)
	}
	if got := follows(lib.ID, user); len(got) != 2 {
		t.Errorf("two spellings: %+q", got)
	}

	// A folder of its own in the spelling given is not the followed one.
	own, err := c.CreateLibrary(ctx, Library{Name: "Own", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(own.Root, nfc), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := c.FollowPodcast(ctx, user, own.ID, nfd, "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := c.UnfollowPodcast(ctx, user, own.ID, nfc); err != nil {
		t.Fatal(err)
	}
	if got := follows(own.ID, user); !slices.Equal(got, []string{nfd}) {
		t.Errorf("own entry: %+q", got)
	}

	// Nor is a show indexed at the spelling given, when the disk can't say
	// (its folder gone, a share not mounted).
	indexed, err := c.CreateLibrary(ctx, Library{Name: "Indexed", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	indexShowAt(t, c, ctx, indexed.ID, nfc, "")
	if err := c.FollowPodcast(ctx, user, indexed.ID, nfd, "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := c.UnfollowPodcast(ctx, user, indexed.ID, nfc); err != nil {
		t.Fatal(err)
	}
	if got := follows(indexed.ID, user); !slices.Equal(got, []string{nfd}) {
		t.Errorf("show indexed at the path given: %+q", got)
	}
}
