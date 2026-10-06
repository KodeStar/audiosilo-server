package catalog

import (
	"errors"
	"math"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// seedNamedUser adds a user called name and returns its id.
func seedNamedUser(t *testing.T, c *Catalog, name string) int64 {
	t.Helper()
	res, err := c.db.ExecContext(t.Context(),
		`INSERT INTO users(username, password_hash, role, created_at, updated_at) VALUES(?,'x','user','t','t')`, name)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// Joining disc books carries listening state onto the joined book's timeline: the
// furthest progress per listener (a finished disc counts as its end; only the last
// one finishes the book), bookmarks, notes, history and sessions offset, favourites
// once; the discs' config is copied, the earliest disc winning field by field.
func TestJoinDurableState(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	ann, bob, cat, dan := seedNamedUser(t, c, "ann"), seedNamedUser(t, c, "bob"), seedNamedUser(t, c, "cat"), seedNamedUser(t, c, "dan")
	const into = "Author/Book"
	cd1, cd2, cd3 := into+"/CD1", into+"/CD2", into+"/CD3"
	ref := func(p string) Ref { return Ref{LibraryID: lib.ID, Path: p} }

	// The discs as they were indexed, with an edit or two, and the joined book.
	for _, p := range []string{cd1, cd2, cd3} {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Disc",
			Chapters: []metadata.Chapter{{Index: 0, Title: "01", FilePath: p + "/01.mp3"}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EditBook(ctx, lib.ID, cd1, BookEdit{Set: map[string]string{FieldTitle: "How to Speak Dragonese"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, cd2, BookEdit{Set: map[string]string{FieldTitle: "Disc two", FieldNarrator: "David Tennant"},
		ChapterSet: map[int]string{0: "The dragon speaks"}}); err != nil {
		t.Fatal(err)
	}
	// The first disc has only an ISBN: it mustn't keep the third disc's ASIN out.
	if err := c.SetEnrichment(ctx, lib.ID, cd1, "", "9780340999073"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetEnrichment(ctx, lib.ID, cd3, "B0DRAGON", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: into, IsFolder: true, Title: "Book",
		Files: []BookFile{{RelPath: cd1 + "/01.mp3"}, {RelPath: cd2 + "/01.mp3", Seq: 1}, {RelPath: cd3 + "/01.mp3", Seq: 2}},
		Chapters: []metadata.Chapter{
			{Index: 0, Title: "CD1 - 01", FilePath: cd1 + "/01.mp3"},
			{Index: 1, Title: "CD2 - 01", FilePath: cd2 + "/01.mp3", FileIndex: 1, BookOffset: 100},
			{Index: 2, Title: "CD3 - 01", FilePath: cd3 + "/01.mp3", FileIndex: 2, BookOffset: 250},
		}}); err != nil {
		t.Fatal(err)
	}

	save := func(user int64, p string, pos float64, finished bool, at string) {
		t.Helper()
		if _, err := c.SaveProgress(ctx, user, Progress{Ref: ref(p), Position: pos, Finished: finished, UpdatedAt: at, Version: 3}); err != nil {
			t.Fatal(err)
		}
	}
	// ann listened into disc 2 after a pass over disc 1: disc 2 is further on.
	save(ann, cd1, 80, false, "2026-01-02T10:00:00Z")
	save(ann, cd2, 30, false, "2026-01-01T10:00:00Z")
	// bob finished disc 1 (left at 0) and nothing else: the end of disc 1.
	save(bob, cd1, 0, true, "2026-01-03T10:00:00Z")
	// cat finished the last disc: the book is finished.
	save(cat, cd3, 200, true, "2026-01-04T10:00:00Z")
	// dan already has progress on the joined book, further than his disc.
	save(dan, into, 260, false, "2026-01-05T10:00:00Z")
	save(dan, cd2, 10, false, "2026-01-06T10:00:00Z")

	if _, err := c.AddBookmark(ctx, ann, Bookmark{Ref: ref(cd2), Position: 12, Note: "dragon"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddNote(ctx, ann, Note{Ref: ref(cd3), Position: 5, Body: "ending"}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHistory(ctx, ann, ref(cd2), 1, 2, "2026-01-01T10:00:00Z", "2026-01-01T10:01:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO listening_sessions(user_id, library_id, rel_path, started_at,
		    last_at, start_pos, end_pos, duration) VALUES(?, ?, ?, 't', 't', 3, 9, 200)`, ann, lib.ID, cd3); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cd1, cd2} {
		if err := c.AddFavourite(ctx, ann, ref(p)); err != nil {
			t.Fatal(err)
		}
	}

	parts := []JoinPart{
		{Path: cd1, Offset: 0, Duration: 100},
		{Path: cd2, Offset: 100, Duration: 150},
		{Path: cd3, Offset: 250, Duration: 200, Last: true},
	}
	if err := c.JoinDurableState(ctx, lib.ID, into, parts, 450); err != nil {
		t.Fatal(err)
	}

	// A listener's rows that merge take a version above them all; a lone row keeps its own.
	for _, tc := range []struct {
		name     string
		user     int64
		pos      float64
		finished bool
		updated  string
		version  int64
	}{
		// Disc 1's save is the newer, but a join keeps the furthest (unlike a move).
		{"furthest disc wins over the newer save", ann, 130, false, "2026-01-02T10:00:00Z", 4},
		{"a finished disc is its end", bob, 100, false, "2026-01-03T10:00:00Z", 3},
		{"the last disc finished finishes the book", cat, 450, true, "2026-01-04T10:00:00Z", 3},
		{"the joined book's own progress counts", dan, 260, false, "2026-01-06T10:00:00Z", 4},
	} {
		p, err := c.GetProgress(ctx, tc.user, ref(into))
		if err != nil || p == nil {
			t.Fatalf("%s: no progress on the joined book (err %v)", tc.name, err)
		}
		if !near(p.Position, tc.pos) || p.Finished != tc.finished || p.Duration != 450 || p.UpdatedAt != tc.updated || p.Version != tc.version {
			t.Errorf("%s: progress = %+v, want position %v finished %v updated %s version %d",
				tc.name, p, tc.pos, tc.finished, tc.updated, tc.version)
		}
		for _, d := range []string{cd1, cd2, cd3} {
			if left, _ := c.GetProgress(ctx, tc.user, ref(d)); left != nil {
				t.Errorf("%s: progress left on %s: %+v", tc.name, d, left)
			}
		}
	}
	var finishedAt *string
	if err := c.db.QueryRowContext(ctx, `SELECT finished_at FROM progress WHERE user_id = ? AND rel_path = ?`, cat, into).
		Scan(&finishedAt); err != nil || finishedAt == nil {
		t.Fatalf("a finished join has no finished_at (err %v)", err)
	}

	if bms, _ := c.ListBookmarks(ctx, ann, ref(into)); len(bms) != 1 || !near(bms[0].Position, 112) || bms[0].Note != "dragon" {
		t.Errorf("bookmarks = %+v, want one at 112", bms)
	}
	if notes, _ := c.ListNotes(ctx, ann, ref(into)); len(notes) != 1 || !near(notes[0].Position, 255) {
		t.Errorf("notes = %+v, want one at 255", notes)
	}
	if h, _ := c.ListHistory(ctx, ann, ref(into), 10); len(h) != 1 || !near(h[0].From, 101) || !near(h[0].To, 102) {
		t.Errorf("history = %+v, want 101-102", h)
	}
	var start, end, length float64
	if err := c.db.QueryRowContext(ctx, `SELECT start_pos, end_pos, duration FROM listening_sessions WHERE rel_path = ?`, into).
		Scan(&start, &end, &length); err != nil || !near(start, 253) || !near(end, 259) || length != 450 {
		t.Errorf("session = %v-%v of %v (err %v), want 253-259 of 450", start, end, length, err)
	}
	favs, _ := c.ListAllFavourites(ctx, ann, []Scope{{LibraryID: lib.ID, AllowAll: true}})
	if len(favs) != 1 || favs[0].Path != into {
		t.Errorf("favourites = %+v, want the joined book once", favs)
	}

	// Config: the first disc's title and ISBN, the second's narrator, the third's ASIN, the
	// second disc's chapter rename on its file in the joined book; the discs keep theirs.
	b, err := c.GetBookByPath(ctx, lib.ID, into)
	if err != nil {
		t.Fatal(err)
	}
	if b.Title != "How to Speak Dragonese" || b.Narrator != "David Tennant" || b.ASIN != "B0DRAGON" ||
		b.ISBN != "9780340999073" {
		t.Errorf("joined book = %q / %q / %q / %q", b.Title, b.Narrator, b.ASIN, b.ISBN)
	}
	if b.Chapters[1].Title != "The dragon speaks" || b.Chapters[0].Title != "CD1 - 01" {
		t.Errorf("joined chapters = %+v", b.Chapters)
	}
	if d, _ := c.GetBookByPath(ctx, lib.ID, cd2); d.Title != "Disc two" {
		t.Errorf("disc 2 lost its own edit: %q", d.Title)
	}

	// A part outside the joined folder is refused.
	if err := c.JoinDurableState(ctx, lib.ID, into, []JoinPart{{Path: "Elsewhere/CD1"}}, 0); err == nil {
		t.Fatal("joined a book from outside the folder")
	}
}

// With the joined book's length unknown, a placed disc's carried progress and
// sessions take where the disc ends on the joined timeline (its offset plus its
// length, or the length its row recorded) as their length, not the disc's own: a
// position moved onto the joined timeline must never read past 100%.
func TestJoinUnknownTotalKeepsPositionWithinDuration(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	ann, bob := seedNamedUser(t, c, "ann"), seedNamedUser(t, c, "bob")
	const into = "Author/Book"
	cd1, cd2, cd3 := into+"/CD1", into+"/CD2", into+"/CD3"
	ref := func(p string) Ref { return Ref{LibraryID: lib.ID, Path: p} }
	// ann is into disc 2 (its length known), bob into disc 3 (its length unknown,
	// so the row's own counts).
	for _, s := range []struct {
		user          int64
		path          string
		pos, duration float64
	}{{ann, cd2, 30, 150}, {bob, cd3, 40, 200}} {
		if _, err := c.SaveProgress(ctx, s.user, Progress{Ref: ref(s.path), Position: s.pos, Duration: s.duration}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.db.ExecContext(ctx, `INSERT INTO listening_sessions(user_id, library_id, rel_path, started_at,
			    last_at, start_pos, end_pos, duration) VALUES(?, ?, ?, 't', 't', 3, ?, ?)`,
			s.user, lib.ID, s.path, s.pos, s.duration); err != nil {
			t.Fatal(err)
		}
	}
	parts := []JoinPart{
		{Path: cd1, Offset: 0, Duration: 100},
		{Path: cd2, Offset: 100, Duration: 150},
		{Path: cd3, Offset: 250, Last: true},
	}
	if err := c.JoinDurableState(ctx, lib.ID, into, parts, 0); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		user          int64
		pos, duration float64
	}{
		{"a disc of known length ends at its offset plus its length", ann, 130, 250},
		{"a disc of unknown length ends at its offset plus the row's length", bob, 290, 450},
	} {
		p, err := c.GetProgress(ctx, tc.user, ref(into))
		if err != nil || p == nil {
			t.Fatalf("%s: no progress on the joined book (err %v)", tc.name, err)
		}
		if !near(p.Position, tc.pos) || !near(p.Duration, tc.duration) || p.Position > p.Duration {
			t.Errorf("%s: progress at %v of %v, want %v of %v", tc.name, p.Position, p.Duration, tc.pos, tc.duration)
		}
		var end, length float64
		if err := c.db.QueryRowContext(ctx, `SELECT end_pos, duration FROM listening_sessions WHERE user_id = ? AND rel_path = ?`,
			tc.user, into).Scan(&end, &length); err != nil || !near(end, tc.pos) || !near(length, tc.duration) {
			t.Errorf("%s: session to %v of %v (err %v), want %v of %v", tc.name, end, length, err, tc.pos, tc.duration)
		}
	}
}

// An Unplaced part (its offset unknown) keeps its listening state on its own path,
// while a placed one carries; its config is still copied.
func TestJoinLeavesUnplacedPartState(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	ann := seedNamedUser(t, c, "ann")
	const into = "Author/Book"
	cd1, cd2 := into+"/CD1", into+"/CD2"
	ref := func(p string) Ref { return Ref{LibraryID: lib.ID, Path: p} }
	for _, p := range []string{cd1, cd2} {
		if _, err := c.SaveProgress(ctx, ann, Progress{Ref: ref(p), Position: 5}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.AddBookmark(ctx, ann, Bookmark{Ref: ref(p), Position: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetEnrichment(ctx, lib.ID, cd2, "B0DISC2", ""); err != nil {
		t.Fatal(err)
	}
	parts := []JoinPart{{Path: cd1}, {Path: cd2, Last: true, Unplaced: true}}
	if err := c.JoinDurableState(ctx, lib.ID, into, parts, 0); err != nil {
		t.Fatal(err)
	}
	if p, _ := c.GetProgress(ctx, ann, ref(into)); p == nil || p.Position != 5 {
		t.Errorf("joined progress = %+v, want disc 1's", p)
	}
	if p, _ := c.GetProgress(ctx, ann, ref(cd2)); p == nil || p.Position != 5 {
		t.Errorf("the unplaced disc's progress = %+v, want it left on its path", p)
	}
	if bms, _ := c.ListBookmarks(ctx, ann, ref(into)); len(bms) != 1 || bms[0].Position != 2 {
		t.Errorf("joined bookmarks = %+v, want disc 1's only", bms)
	}
	if bms, _ := c.ListBookmarks(ctx, ann, ref(cd2)); len(bms) != 1 {
		t.Errorf("the unplaced disc's bookmarks = %+v, want them left on its path", bms)
	}
	var asin string
	if err := c.db.QueryRowContext(ctx, `SELECT asin FROM book_enrichment WHERE library_id = ? AND path = ?`,
		lib.ID, into).Scan(&asin); err != nil || asin != "B0DISC2" {
		t.Errorf("the unplaced disc's config wasn't copied: %q (err %v)", asin, err)
	}
}

// A book split across disc folders (books.split_parent, which the scan sets; see
// library.markSplitDiscs) is listed once, by its first disc, until its folder gets
// an override; its discs are not also offered as duplicates.
func TestSplitDiscsIssue(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	uid := seedUser(t, c, ctx)
	add := func(p, splitParent string) {
		t.Helper()
		// Alike: same author, title and length, which alone would make them duplicates.
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Dragonese",
			Author: "Cressida Cowell", Duration: 3000, SplitParent: splitParent}); err != nil {
			t.Fatal(err)
		}
	}
	// Split: the issue.
	add("Cowell/Dragonese/CD 1", "Cowell/Dragonese")
	add("Cowell/Dragonese/CD 2", "Cowell/Dragonese")
	add("Cowell/Dragonese/cd-10", "Cowell/Dragonese")
	// Not split: alike books that are copies of each other.
	add("Extras/Book/CD1", "")
	add("Extras/Book/CD2", "")
	// Split, but settled: the folder has an override (any mode).
	add("Settled/Book/Disk 1", "Settled/Book")
	add("Settled/Book/Disk 2", "Settled/Book")
	if err := c.SetFolderOverride(ctx, lib.ID, "Settled/Book", OverrideCollection); err != nil {
		t.Fatal(err)
	}

	listed := func() []string {
		t.Helper()
		page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{Issue: IssueSplitDiscs}})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range page.Books {
			out = append(out, b.Path)
		}
		return out
	}
	counts, err := c.IssueCounts(ctx, []string{IssueSplitDiscs, IssueDuplicate})
	if err != nil {
		t.Fatal(err)
	}
	if got := listed(); len(got) != 1 || got[0] != "Cowell/Dragonese/CD 1" {
		t.Fatalf("split discs listed = %v, want the first disc of Dragonese", got)
	}
	if counts[0].Count != 1 || len(counts[0].Samples) != 1 || counts[0].Samples[0].Path != "Cowell/Dragonese/CD 1" {
		t.Fatalf("split discs count = %+v", counts[0])
	}
	// The other alike books (the settled discs too) still group as duplicates; the
	// Dragonese discs don't.
	groups, _ := c.DuplicateGroups(ctx, lib.ID, false)
	grouped := map[string]bool{}
	for _, g := range groups {
		for _, m := range g.Books {
			grouped[m.Path] = true
		}
	}
	for _, p := range []string{"Cowell/Dragonese/CD 1", "Cowell/Dragonese/CD 2", "Cowell/Dragonese/cd-10"} {
		if grouped[p] {
			t.Fatalf("a split disc is offered as a duplicate: %s in %+v", p, groups)
		}
	}
	for _, p := range []string{"Extras/Book/CD1", "Settled/Book/Disk 1"} {
		if !grouped[p] {
			t.Fatalf("%s is no longer offered as a duplicate: %+v", p, groups)
		}
	}

	// Ignored: gone from the open list, counted as ignored.
	first := []Ref{{LibraryID: lib.ID, Path: "Cowell/Dragonese/CD 1"}}
	if err := c.IgnoreIssue(ctx, IssueSplitDiscs, first, uid); err != nil {
		t.Fatal(err)
	}
	if counts, _ = c.IssueCounts(ctx, []string{IssueSplitDiscs}); counts[0].Count != 0 || counts[0].Ignored != 1 {
		t.Fatalf("after ignoring: %+v", counts[0])
	}
	if err := c.UnignoreIssue(ctx, IssueSplitDiscs, first); err != nil {
		t.Fatal(err)
	}

	// Fixed: the folder set to one book settles it at once.
	if err := c.SetFolderOverride(ctx, lib.ID, "Cowell/Dragonese", OverrideBook); err != nil {
		t.Fatal(err)
	}
	if got := listed(); len(got) != 0 {
		t.Fatalf("still listed after the fix: %v", got)
	}
}

// GetBookHolding answers a path below a folder book from the index: a disc folder
// of a joined book, or a file in it, is that book (the innermost folder book
// holding it); a folder's single-file books, a path no book's files are under, a
// sibling whose name only starts like a disc's, and a LIKE wildcard ("CD_") match
// nothing.
func TestGetBookHolding(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	other, _ := c.CreateLibrary(ctx, Library{Name: "M", Root: "/tmp/m"})
	books := []*Book{
		{LibraryID: lib.ID, RelPath: "A/Book", IsFolder: true, Title: "Joined",
			Files: []BookFile{{RelPath: "A/Book/CD1/01.mp3"}, {RelPath: "A/Book/CD2/01.mp3", Seq: 1}}},
		{LibraryID: lib.ID, RelPath: "A/Book/CD1/Bonus", IsFolder: true, Title: "Bonus",
			Files: []BookFile{{RelPath: "A/Book/CD1/Bonus/01.mp3"}}},
		{LibraryID: lib.ID, RelPath: "A/Single.m4b", Title: "Single", Files: []BookFile{{RelPath: "A/Single.m4b"}}},
	}
	for _, b := range books {
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	for p, want := range map[string]string{
		"A/Book/CD1":              "A/Book",
		"A/Book/CD2/01.mp3":       "A/Book",
		"A/Book/CD1/Bonus/01.mp3": "A/Book/CD1/Bonus",
	} {
		if b, err := c.GetBookHolding(ctx, lib.ID, p); err != nil || b.RelPath != want {
			t.Errorf("GetBookHolding(%q) = %+v (err %v), want %s", p, b, err, want)
		}
	}
	for _, p := range []string{"A/Book", "A/Book/CD3", "A/Book/CD", "A/Bookx/CD1", "A/Book/CD_", "A/Book/CD%", "A", "A/Single.m4b/x"} {
		if b, err := c.GetBookHolding(ctx, lib.ID, p); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetBookHolding(%q) = %+v (err %v), want ErrNotFound", p, b, err)
		}
	}
	if _, err := c.GetBookHolding(ctx, other.ID, "A/Book/CD1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetBookHolding in another library: err %v, want ErrNotFound", err)
	}
}
