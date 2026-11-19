package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// setMetadataSource switches a library's metadata source and re-resolves its
// books (ResolveSources), failing the test on an error.
func setMetadataSource(t *testing.T, c *Catalog, ctx context.Context, libID int64, source string) {
	t.Helper()
	if _, stale, err := c.UpdateLibrary(ctx, libID, LibraryPatch{MetadataSource: &source}); err != nil {
		t.Fatal(err)
	} else if stale {
		t.Fatal("a new metadata source asked for a rescan")
	}
	if err := c.ResolveSources(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestMetadataSourceResolvesInBatches: a new metadata source marks the library's
// books due, in the edit's transaction, rather than re-resolving them there (a
// large library takes longer than a request may, server#213); ResolveSources
// re-resolves every book, a batch a transaction, and a stop before it does
// leaves them due for the next start. The last change of several wins.
func TestMetadataSourceResolvesInBatches(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	c, ctx := New(db, time.Now), context.Background()
	lib, err := c.CreateLibrary(ctx, Library{Name: "Big", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	const n = resolveBatch + 5
	for i := range n {
		p := fmt.Sprintf("Author/Series/%03d - Book", i+1)
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Tagged " + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	titles := func(c *Catalog) map[string]int {
		t.Helper()
		out := map[string]int{}
		for i := range n {
			out[mustBook(t, c, ctx, lib.ID, fmt.Sprintf("Author/Series/%03d - Book", i+1)).Title]++
		}
		return out
	}
	for _, source := range []string{MetadataFromPath, MetadataFromTags, MetadataFromPath} {
		if _, _, err := c.UpdateLibrary(ctx, lib.ID, LibraryPatch{MetadataSource: &source}); err != nil {
			t.Fatal(err)
		}
	}
	if got := titles(c); got["Book"] != 0 {
		t.Fatalf("the edit re-resolved books itself: %v", got)
	}
	// A restart: another catalog over the same database finds them due.
	again := New(db, time.Now)
	if err := again.ResolveSources(ctx); err != nil {
		t.Fatal(err)
	}
	if got := titles(again); got["Book"] != n {
		t.Fatalf("titles after the re-resolve = %v, want the path's for all %d", got, n)
	}
	if due, err := getServerState[map[int64]int64](ctx, db, resolveKey); err != nil || len(due) != 0 {
		t.Fatalf("still due: %v, %v", due, err)
	}
}

// TestResolveSourcesOvertaken: a run for a change a later one has overtaken
// writes nothing and leaves the library due, so the next pass re-resolves it
// for the later change.
func TestResolveSourcesOvertaken(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	const p = "Author/Series/01 - Book"
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Tagged"}); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{MetadataFromPath, MetadataFromTags, MetadataFromPath} {
		if _, _, err := c.UpdateLibrary(ctx, lib.ID, LibraryPatch{MetadataSource: &source}); err != nil {
			t.Fatal(err)
		}
	}
	due, err := getServerState[map[int64]int64](ctx, c.db, resolveKey)
	if err != nil || due[lib.ID] != 3 {
		t.Fatalf("due = %v, %v, want generation 3", due, err)
	}
	if err := c.resolveLibrary(ctx, lib.ID, 2); err != nil {
		t.Fatal(err)
	}
	if got := mustBook(t, c, ctx, lib.ID, p).Title; got != "Tagged" {
		t.Fatalf("an overtaken run re-resolved the book: %q", got)
	}
	if due, err := getServerState[map[int64]int64](ctx, c.db, resolveKey); err != nil || due[lib.ID] != 3 {
		t.Fatalf("an overtaken run changed the mark: %v, %v", due, err)
	}
	if err := c.ResolveSources(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mustBook(t, c, ctx, lib.ID, p).Title; got != "Book" {
		t.Fatalf("after the pass the title = %q, want the path's", got)
	}
}

// TestResolveSourcesFailingLibrary: a library whose books can't be re-resolved
// (its writes fail) stays due and is reported, while the other libraries due in
// the same pass are still re-resolved and cleared.
func TestResolveSourcesFailingLibrary(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	const p = "Author/Series/01 - Book"
	var libs [2]*Library
	for i := range libs {
		lib, err := c.CreateLibrary(ctx, Library{Name: "L" + strconv.Itoa(i), Root: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Tagged"}); err != nil {
			t.Fatal(err)
		}
		libs[i] = lib
	}
	bad, good := libs[0], libs[1]
	if _, err := c.db.ExecContext(ctx, `CREATE TRIGGER fail_resolve BEFORE UPDATE OF title ON books
		WHEN OLD.library_id = `+strconv.FormatInt(bad.ID, 10)+` BEGIN SELECT RAISE(ABORT, 'disk says no'); END`); err != nil {
		t.Fatal(err)
	}
	source := MetadataFromPath
	for _, lib := range libs {
		if _, _, err := c.UpdateLibrary(ctx, lib.ID, LibraryPatch{MetadataSource: &source}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.ResolveSources(ctx); err == nil {
		t.Fatal("a library that can't be re-resolved reported nothing")
	}
	if got := mustBook(t, c, ctx, good.ID, p).Title; got != "Book" {
		t.Fatalf("the other library's book = %q, want the path's title", got)
	}
	due, err := getServerState[map[int64]int64](ctx, c.db, resolveKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := due[bad.ID]; !ok || len(due) != 1 {
		t.Fatalf("due = %v, want the failing library alone", due)
	}
}

// TestResolveSourcesSkipsUnreadableBook: a book whose stored scan doesn't
// decode, among readable ones, is passed over with a read problem naming its
// first file (so the next scan reads it again and Health lists it), going over
// a problem on a later part (which may never clear) but not one on that first
// file; the rest of the library is re-resolved, and the library is no longer
// due, the pass reporting nothing.
func TestResolveSourcesSkipsUnreadableBook(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"Author/Series/01 - One", "Author/Series/02 - Two", "Author/Series/03 - Three",
		"Author/Series/04 - Four", "Author/Series/05 - Five"}
	broken, later, keeps := paths[1], paths[2], paths[4]
	for _, p := range paths {
		b := &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Tagged",
			Files: []BookFile{{RelPath: p + "/01.mp3"}, {RelPath: p + "/02.mp3", Seq: 1}}}
		switch p {
		case later:
			b.ScanError, b.ScanErrorFile, b.ScanErrorDetail = "probe_failed", p+"/02.mp3", "bad data"
		case keeps:
			b.ScanError, b.ScanErrorFile, b.ScanErrorDetail = ProblemUnreadable, p+"/01.mp3", "permission denied"
		}
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// A single-file book's problem names the book's own file.
	const loose = "Author/Loose.m4b"
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: loose, Title: "Tagged"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET scanned = '{broken' WHERE library_id = ? AND rel_path IN (?, ?, ?, ?)`,
		lib.ID, broken, later, keeps, loose); err != nil {
		t.Fatal(err)
	}
	setMetadataSource(t, c, ctx, lib.ID, MetadataFromPath)
	want := map[string]string{paths[0]: "One", broken: "Tagged", later: "Tagged", paths[3]: "Four", keeps: "Tagged"}
	for p, title := range want {
		if got := mustBook(t, c, ctx, lib.ID, p).Title; got != title {
			t.Errorf("%s title = %q, want %q", p, got, title)
		}
	}
	problem := func(p string) [3]string {
		t.Helper()
		var out [3]string
		if err := c.db.QueryRowContext(ctx,
			`SELECT scan_error, scan_error_file, scan_error_detail FROM books WHERE library_id = ? AND rel_path = ?`,
			lib.ID, p).Scan(&out[0], &out[1], &out[2]); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := problem(broken); got != [3]string{ProblemUnreadable, broken + "/01.mp3", scannedUnreadableDetail} {
		t.Errorf("the unreadable book's problem = %q, want unreadable on its first file", got)
	}
	if got := problem(loose); got != [3]string{ProblemUnreadable, loose, scannedUnreadableDetail} {
		t.Errorf("the unreadable single-file book's problem = %q, want unreadable on its file", got)
	}
	if got := problem(later); got != [3]string{ProblemUnreadable, later + "/01.mp3", scannedUnreadableDetail} {
		t.Errorf("the later part's problem = %q, want it replaced by the first file's", got)
	}
	if got := problem(keeps); got != [3]string{ProblemUnreadable, keeps + "/01.mp3", "permission denied"} {
		t.Errorf("the first file's recorded problem = %q, want it kept", got)
	}
	if got := problem(paths[0]); got != [3]string{} {
		t.Errorf("a readable book's problem = %q, want none", got)
	}
	if due, err := getServerState[map[int64]int64](ctx, c.db, resolveKey); err != nil || len(due) != 0 {
		t.Fatalf("still due: %v, %v", due, err)
	}
}

// fieldValues is a book's resolved values and sources, for comparing.
func fieldValues(t *testing.T, c *Catalog, ctx context.Context, libID int64, path string) map[string][2]string {
	t.Helper()
	d, err := c.AdminBookDetail(ctx, libID, path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]string{}
	for _, f := range []string{FieldTitle, FieldAuthor, FieldNarrator, FieldSeries, FieldSeriesIndex} {
		out[f] = [2]string{d.Fields[f].Value, d.Fields[f].Source}
	}
	return out
}

func assertFields(t *testing.T, got, want map[string][2]string) {
	t.Helper()
	for f, w := range want {
		if got[f] != w {
			t.Errorf("%s = %q, want %q", f, got[f], w)
		}
	}
}

// TestPreferPathResolvesFromTheLayout: a library that prefers its folders takes the
// title, author, series and position from the layout over junk tags, on the row
// every reader sees (search included) and as path provenance; the tags still fill
// what the path doesn't say (the narrator), an edit stays the lock, and switching
// back restores the tags - all without a rescan.
func TestPreferPathResolvesFromTheLayout(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GetLibrary(ctx, lib.ID); got.MetadataSource != MetadataFromTags {
		t.Fatalf("a new library's metadata source = %q, want tags", got.MetadataSource)
	}
	if _, err := c.CreateLibrary(ctx, Library{Name: "Bad", Root: "/tmp/b", MetadataSource: "folders"}); !errors.Is(err, ErrInvalidMetadataSource) {
		t.Fatalf("create with a bogus source = %v, want ErrInvalidMetadataSource", err)
	}
	bogus := "folders"
	if _, _, err := c.UpdateLibrary(ctx, lib.ID, LibraryPatch{MetadataSource: &bogus}); !errors.Is(err, ErrInvalidMetadataSource) {
		t.Fatalf("update with a bogus source = %v, want ErrInvalidMetadataSource", err)
	}
	uid := seedUser(t, c, ctx)
	const p = "James S. A. Corey/The Expanse/03 - Abaddon's Gate"
	b := scannedBook(lib.ID, p)
	b.Title, b.Author, b.Narrator = "Abaddon's Gate: Expanse, Book 3 (Unabridged)", "James S. A. Corey", "Jefferson Mays"
	b.Series, b.SeriesIndex = "The Expanse, book #3", 0
	if _, err := c.UpsertBook(ctx, b); err != nil {
		t.Fatal(err)
	}
	tags := map[string][2]string{
		FieldTitle: {"Abaddon's Gate: Expanse, Book 3 (Unabridged)", SourceTag},
		// The tag agrees with the author folder, so it reads as the path's.
		FieldAuthor:      {"James S. A. Corey", SourcePath},
		FieldNarrator:    {"Jefferson Mays", SourceTag},
		FieldSeries:      {"The Expanse, book #3", SourceTag},
		FieldSeriesIndex: {"", ""},
	}
	assertFields(t, fieldValues(t, c, ctx, lib.ID, p), tags)

	setMetadataSource(t, c, ctx, lib.ID, MetadataFromPath)
	path := map[string][2]string{
		FieldTitle:       {"Abaddon's Gate", SourcePath},
		FieldAuthor:      {"James S. A. Corey", SourcePath},
		FieldNarrator:    {"Jefferson Mays", SourceTag},
		FieldSeries:      {"The Expanse", SourcePath},
		FieldSeriesIndex: {"3", SourcePath},
	}
	assertFields(t, fieldValues(t, c, ctx, lib.ID, p), path)
	if row := mustBook(t, c, ctx, lib.ID, p); row.Title != "Abaddon's Gate" || row.Series != "The Expanse" || row.SeriesIndex != 3 {
		t.Fatalf("row = %q / %q #%v, want the path's values", row.Title, row.Series, row.SeriesIndex)
	}
	if got := searchTitles(t, c, ctx, lib.ID, "Unabridged"); len(got) != 0 {
		t.Fatalf("search still finds the tag title: %v", got)
	}
	if got := searchTitles(t, c, ctx, lib.ID, "Abaddon"); !slices.Equal(got, []string{"Abaddon's Gate"}) {
		t.Fatalf("search = %v, want the path title", got)
	}

	// A rescan resolves the same way.
	if _, err := c.UpsertBook(ctx, b); err != nil {
		t.Fatal(err)
	}
	assertFields(t, fieldValues(t, c, ctx, lib.ID, p), path)

	// An edit beats the path, and survives the switch back.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: map[string]string{FieldTitle: "Mine"}, UserID: uid}); err != nil {
		t.Fatal(err)
	}
	setMetadataSource(t, c, ctx, lib.ID, MetadataFromTags)
	tags[FieldTitle] = [2]string{"Mine", SourceEdited}
	assertFields(t, fieldValues(t, c, ctx, lib.ID, p), tags)

	// Reverting it in a path library restores the path's value.
	setMetadataSource(t, c, ctx, lib.ID, MetadataFromPath)
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Revert: []string{FieldTitle}, UserID: uid}); err != nil {
		t.Fatal(err)
	}
	assertFields(t, fieldValues(t, c, ctx, lib.ID, p), path)
	if d, _ := c.AdminBookDetail(ctx, lib.ID, p); d.Fields[FieldTitle].Scanned != "Abaddon's Gate" {
		t.Fatalf("scanned title = %q, want the path's", d.Fields[FieldTitle].Scanned)
	}
}

// TestPreferPathKeepsWhatThePathDoesNotSay: where the layout names no author or
// series, the tags' stay; a tag's position stays only beside the same series.
func TestPreferPathKeepsWhatThePathDoesNotSay(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l", MetadataSource: MetadataFromPath})
	upsert := func(path string, isFolder bool, title, author, series string, idx float64) {
		t.Helper()
		b := scannedBook(lib.ID, path)
		b.IsFolder, b.Title, b.Author, b.Series, b.SeriesIndex = isFolder, title, author, series, idx
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, path    string
		isFolder      bool
		title, author string
		series        string
		idx           float64
		want          map[string][2]string
	}{
		{
			// A root file says only its title.
			name: "root file", path: "Dune.m4b", title: "Dune (Unabridged)", author: "Frank Herbert",
			series: "Dune Chronicles", idx: 1,
			want: map[string][2]string{
				FieldTitle: {"Dune", SourcePath}, FieldAuthor: {"Frank Herbert", SourceTag},
				FieldSeries: {"Dune Chronicles", SourceTag}, FieldSeriesIndex: {"1", SourceTag},
			},
		},
		{
			// One folder deep is the author, not a series: the tag's series stays.
			name: "author folder", path: "George Orwell/Animal Farm", isFolder: true, title: "Animal Farm: A Fairy Story",
			author: "Orwell, George", series: "Penguin Modern Classics", idx: 2,
			want: map[string][2]string{
				FieldTitle: {"Animal Farm", SourcePath}, FieldAuthor: {"George Orwell", SourcePath},
				FieldSeries: {"Penguin Modern Classics", SourceTag}, FieldSeriesIndex: {"2", SourceTag},
			},
		},
		{
			// Untagged, one folder deep: the scan's baseline and the layout both
			// read the folder as the author, and no series.
			name: "untagged author folder", path: "George Orwell/Nineteen Eighty-Four", isFolder: true,
			title: "Nineteen Eighty-Four", author: "George Orwell",
			want: map[string][2]string{
				FieldTitle: {"Nineteen Eighty-Four", SourcePath}, FieldAuthor: {"George Orwell", SourcePath},
				FieldSeries: {"", ""}, FieldSeriesIndex: {"", ""},
			},
		},
		{
			// A disc folder: the baseline takes the book's folder for a series, the
			// layout reads it as the book and no series, and that wins.
			name: "untagged disc folder", path: "Frank Herbert/Dune/CD1", isFolder: true,
			title: "CD1", author: "Frank Herbert", series: "Dune",
			want: map[string][2]string{
				FieldTitle: {"Dune", SourcePath}, FieldAuthor: {"Frank Herbert", SourcePath},
				FieldSeries: {"", ""}, FieldSeriesIndex: {"", ""},
			},
		},
		{
			// A tag's series the layout doesn't replace keeps the tag's position,
			// even one equal to the leaf's number.
			name: "tagged numbered leaf", path: "Brandon Sanderson/01 - The Way of Kings", isFolder: true,
			title: "The Way of Kings", author: "Brandon Sanderson", series: "The Stormlight Archive", idx: 1,
			want: map[string][2]string{
				FieldTitle: {"The Way of Kings", SourcePath}, FieldSeries: {"The Stormlight Archive", SourceTag},
				FieldSeriesIndex: {"1", SourcePath},
			},
		},
		{
			// A tag title that is the folder's name, number and all, says the
			// number is part of the title: it stays, and numbers no series.
			name: "number in the title", path: "Jay Asher/Teen Fiction/13 Reasons Why", isFolder: true,
			title: "13 Reasons Why", author: "Jay Asher", series: "Teen Fiction",
			want: map[string][2]string{
				FieldTitle: {"13 Reasons Why", SourcePath}, FieldSeries: {"Teen Fiction", SourcePath},
				FieldSeriesIndex: {"", ""},
			},
		},
		{
			// The same series unnumbered by the path keeps the tag's position.
			name: "same series", path: "James S. A. Corey/The Expanse/Abaddon's Gate", isFolder: true,
			title: "Abaddon's Gate", author: "James S. A. Corey", series: "the expanse", idx: 3,
			want: map[string][2]string{
				FieldSeries: {"The Expanse", SourcePath}, FieldSeriesIndex: {"3", SourceTag},
			},
		},
		{
			// Another series' position doesn't number the path's.
			name: "other series", path: "James S. A. Corey/The Expanse/Cibola Burn", isFolder: true,
			title: "Cibola Burn", author: "James S. A. Corey", series: "Space Opera Favourites", idx: 9,
			want: map[string][2]string{
				FieldSeries: {"The Expanse", SourcePath}, FieldSeriesIndex: {"", ""},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upsert(tc.path, tc.isFolder, tc.title, tc.author, tc.series, tc.idx)
			assertFields(t, fieldValues(t, c, ctx, lib.ID, tc.path), tc.want)
		})
	}
}

// TestLeafSeriesSources: a leaf named "Author; Series; Book NN[; Title]" states
// the series and position (metadata.DeriveFromPath, metadata.FromPathLayout),
// so the scan's reading of them reads as the path's; a library that prefers its
// folders keeps them (its layout reads the leaf too) and takes the title part
// over the tags' title, and a range is the leaf's in both. An untagged book's
// title is the whole leaf: the folders' library still takes its title part and
// position, beside a series tag spelled another way.
func TestLeafSeriesSources(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	const (
		tufo  = "Mark Tufo/Mark Tufo; Indian Hill; Book 04; From The Ashes"
		rules = "Aaron Oster/Aaron Oster; The Rules; Book 01-03"
		spell = "Larry Correia/Larry Correia; Grimnoir Chronicles; Book 02; Spellbound"
	)
	for _, src := range []string{MetadataFromTags, MetadataFromPath} {
		lib, _ := c.CreateLibrary(ctx, Library{Name: src, Root: "/tmp/" + src, MetadataSource: src})
		for _, b := range []*Book{
			{RelPath: tufo, Title: "From The Ashes (Unabridged)", Author: "Mark Tufo", Series: "Indian Hill", SeriesIndex: 4},
			{RelPath: rules, Title: "The Rules: The Complete Series", Author: "Aaron Oster", Series: "The Rules",
				SeriesIndex: 1, SeriesIndexEnd: 3},
			{RelPath: spell, Title: "Larry Correia; Grimnoir Chronicles; Book 02; Spellbound", Author: "Larry Correia",
				Series: "Grimnoir", SeriesIndex: 2},
		} {
			b.LibraryID, b.IsFolder, b.AddedAt = lib.ID, true, "2020-01-01"
			if _, err := c.UpsertBook(ctx, b); err != nil {
				t.Fatal(err)
			}
		}
		title := [2]string{"From The Ashes (Unabridged)", SourceTag}
		untagged := map[string][2]string{
			FieldTitle:  {"Larry Correia; Grimnoir Chronicles; Book 02; Spellbound", SourcePath},
			FieldSeries: {"Grimnoir", SourceTag}, FieldSeriesIndex: {"2", SourcePath},
		}
		if src == MetadataFromPath {
			title = [2]string{"From The Ashes", SourcePath}
			untagged = map[string][2]string{
				FieldTitle:  {"Spellbound", SourcePath},
				FieldSeries: {"Grimnoir Chronicles", SourcePath}, FieldSeriesIndex: {"2", SourcePath},
			}
		}
		t.Run(src, func(t *testing.T) {
			assertFields(t, fieldValues(t, c, ctx, lib.ID, tufo), map[string][2]string{
				FieldTitle: title, FieldAuthor: {"Mark Tufo", SourcePath},
				FieldSeries: {"Indian Hill", SourcePath}, FieldSeriesIndex: {"4", SourcePath},
			})
			assertFields(t, fieldValues(t, c, ctx, lib.ID, rules), map[string][2]string{
				FieldTitle:  {"The Rules: The Complete Series", SourceTag},
				FieldSeries: {"The Rules", SourcePath}, FieldSeriesIndex: {"1-3", SourcePath},
			})
			assertFields(t, fieldValues(t, c, ctx, lib.ID, spell), untagged)
		})
	}
}

// TestBatchBackfillsSkipAnUnreadableBook: the scan's batch backfills (genres,
// the path reading: eachPathBatch) pass over a book whose stored scan doesn't
// decode, recording its read problem as ResolveSources does, rather than
// failing every other book of the batch with it (server#453).
func TestBatchBackfillsSkipAnUnreadableBook(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"Author/One", "Author/Broken", "Author/Three"}
	for _, p := range paths {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Tagged",
			Files: []BookFile{{RelPath: p + "/01.mp3"}}}); err != nil {
			t.Fatal(err)
		}
	}
	const broken = "Author/Broken"
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET scanned = '{broken' WHERE library_id = ? AND rel_path = ?`,
		lib.ID, broken); err != nil {
		t.Fatal(err)
	}
	genres := map[string][]string{}
	for _, p := range paths {
		genres[p] = []string{"Fantasy"}
	}
	if err := c.SetGenres(ctx, lib.ID, genres); err != nil {
		t.Fatalf("SetGenres with one unreadable book: %v, want the rest written", err)
	}
	for _, p := range []string{"Author/One", "Author/Three"} {
		if got := mustBook(t, c, ctx, lib.ID, p).Genres; len(got) != 1 || got[0] != "Fantasy" {
			t.Errorf("%s genres = %v, want Fantasy", p, got)
		}
	}
	var problem, file, detail string
	if err := c.db.QueryRowContext(ctx, `SELECT scan_error, scan_error_file, scan_error_detail FROM books
		WHERE library_id = ? AND rel_path = ?`, lib.ID, broken).Scan(&problem, &file, &detail); err != nil {
		t.Fatal(err)
	}
	if problem != ProblemUnreadable || file != broken+"/01.mp3" || detail != scannedUnreadableDetail {
		t.Errorf("the unreadable book's problem = %q on %q (%q), want unreadable on its first file", problem, file, detail)
	}
	if err := c.SetPathReading(ctx, lib.ID, map[string]*metadata.Metadata{"Author/One": nil, broken: nil}); err != nil {
		t.Errorf("SetPathReading with one unreadable book: %v, want the rest written", err)
	}
}
