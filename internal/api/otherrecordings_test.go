package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// recordingsEnv is the Saga library with three recordings of work "two":
// Saga/2 (its ASIN's work, B0TWO), "Saga/2 Abe" (its ASIN's work, in the
// member's grant) and "Saga/2 Kemper" (matched in the console, outside the
// grant). Saga/1, the book just finished, is read by Kemper.
func recordingsEnv(t *testing.T, metadataOn bool) *sagaEnv {
	t.Helper()
	e := newSagaEnv(t, metadataOn,
		&catalog.Book{RelPath: "Saga/1", Series: "Saga", SeriesIndex: 1, ASIN: "B0ONE", Narrator: "Kemper"},
		&catalog.Book{RelPath: "Saga/2", Series: "Saga", SeriesIndex: 2, ASIN: "B0TWO", Narrator: "Bray"},
		&catalog.Book{RelPath: "Saga/2 Abe", Series: "Saga", SeriesIndex: 2, ASIN: "B0TWOABE", Narrator: "Abe"},
		&catalog.Book{RelPath: "Saga/2 Kemper", Series: "Saga", SeriesIndex: 2, Narrator: "Kemper"},
	)
	ctx := context.Background()
	owned := []catalog.UpcomingOwned{
		{Ref: catalog.Ref{LibraryID: e.lib.ID, Path: "Saga/1"}, IDKey: "a:B0ONE", WorkID: "one", CheckedAt: "now"},
		{Ref: catalog.Ref{LibraryID: e.lib.ID, Path: "Saga/2"}, IDKey: "a:B0TWO", WorkID: "two", CheckedAt: "now"},
		{Ref: catalog.Ref{LibraryID: e.lib.ID, Path: "Saga/2 Abe"}, IDKey: "a:B0TWOABE", WorkID: "two", CheckedAt: "now"},
	}
	if err := e.cat.SaveUpcomingOwned(ctx, owned, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.EditBook(ctx, e.lib.ID, "Saga/2 Kemper", catalog.BookEdit{
		Match: &catalog.MatchAnchor{WorkID: "two"}, Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	// The member's grant (Saga/1, Saga/2) gains Saga/2 Abe, never Saga/2 Kemper.
	share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Abe"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.AddSharePaths(ctx, share.ID, []catalog.PathRule{{LibraryID: e.lib.ID, Path: "Saga/2 Abe"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, e.memberID, share.ID); err != nil {
		t.Fatal(err)
	}
	return e
}

// itemRecordings is GET /item's work_id and other_recordings paths.
func itemRecordings(t *testing.T, e *sagaEnv, path, token string) (work string, others []string, raw string) {
	t.Helper()
	resp, body := e.do(t, "GET", e.url("item", path), token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("item %s = %d %s", path, resp.StatusCode, body)
	}
	var b struct {
		WorkID          string `json:"work_id"`
		OtherRecordings []struct {
			LibraryID int64  `json:"library_id"`
			RelPath   string `json:"rel_path"`
			WorkID    string `json:"work_id"`
			Files     []any  `json:"files"`
		} `json:"other_recordings"`
	}
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	for _, o := range b.OtherRecordings {
		if o.LibraryID != e.lib.ID || o.WorkID != b.WorkID || o.Files != nil {
			t.Fatalf("other recording is not a list book of the work: %+v", o)
		}
		others = append(others, o.RelPath)
	}
	return b.WorkID, others, body
}

// TestItemOtherRecordings is the allowed+denied pair for other_recordings on
// /item: the admin gets every other recording of the work (a console match and
// an ASIN's work alike), the member only those in their grant, nothing outside
// it leaking; with metadata off the field is absent.
func TestItemOtherRecordings(t *testing.T) {
	t.Parallel()
	e := recordingsEnv(t, true)

	work, others, _ := itemRecordings(t, e, "Saga/2", e.adminTok)
	if want := []string{"Saga/2 Abe", "Saga/2 Kemper"}; work != "two" || !reflect.DeepEqual(others, want) {
		t.Fatalf("admin: work %q, others %q, want two, %q", work, others, want)
	}
	// The console-matched book sees the others from its side.
	if work, others, _ = itemRecordings(t, e, "Saga/2 Kemper", e.adminTok); work != "two" ||
		!reflect.DeepEqual(others, []string{"Saga/2", "Saga/2 Abe"}) {
		t.Fatalf("admin from Kemper: work %q, others %q", work, others)
	}

	// Denied: the member's share misses Saga/2 Kemper.
	work, others, raw := itemRecordings(t, e, "Saga/2", e.memberTok)
	if want := []string{"Saga/2 Abe"}; work != "two" || !reflect.DeepEqual(others, want) {
		t.Fatalf("member: work %q, others %q, want two, %q", work, others, want)
	}
	if strings.Contains(raw, "Kemper") {
		t.Fatalf("a recording outside the grant leaked: %s", raw)
	}

	// A book with no other recording carries none.
	if _, others, raw = itemRecordings(t, e, "Saga/1", e.adminTok); others != nil || strings.Contains(raw, "other_recordings") {
		t.Fatalf("Saga/1: others %q: %s", others, raw)
	}

	// Metadata off: no other_recordings (work_id still rides on the book).
	off := recordingsEnv(t, false)
	if work, others, raw = itemRecordings(t, off, "Saga/2", off.adminTok); work != "two" || others != nil || strings.Contains(raw, "other_recordings") {
		t.Fatalf("metadata off: work %q, others %q: %s", work, others, raw)
	}
}

// listWorkIDs is the list's work_id per path.
func listWorkIDs(t *testing.T, e *sagaEnv) map[string]string {
	t.Helper()
	resp, body := e.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/books?limit=200", e.lib.ID), e.adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("books = %d %s", resp.StatusCode, body)
	}
	var page struct {
		Books []struct {
			RelPath string `json:"rel_path"`
			WorkID  string `json:"work_id"`
		} `json:"books"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, b := range page.Books {
		out[b.RelPath] = b.WorkID
	}
	return out
}

// TestListWorkIDCacheMiss: work_id rides on every list book, and a changed
// work, or identifiers, in the upcoming job's resolution of it is never
// answered from the list cache, while a look-up that only renews when it was checked leaves the cache
// alone (each kind of change: store's
// TestUserStateWritesLeaveTheContentGeneration).
func TestListWorkIDCacheMiss(t *testing.T) {
	t.Parallel()
	e := recordingsEnv(t, true)
	ctx := context.Background()
	want := map[string]string{"Saga/1": "one", "Saga/2": "two", "Saga/2 Abe": "two", "Saga/2 Kemper": "two"}
	if got := listWorkIDs(t, e); !reflect.DeepEqual(got, want) {
		t.Fatalf("work ids = %v, want %v", got, want)
	}
	save := func(o ...catalog.UpcomingOwned) {
		t.Helper()
		if err := e.cat.SaveUpcomingOwned(ctx, o, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Checked again, same answer: the cached list stands.
	save(catalog.UpcomingOwned{Ref: catalog.Ref{LibraryID: e.lib.ID, Path: "Saga/2"}, IDKey: "a:B0TWO", WorkID: "two", CheckedAt: "later"})
	e.cachedBodies(`{"books":[{"rel_path":"cached","work_id":"x"}]}`)
	if got := listWorkIDs(t, e); !reflect.DeepEqual(got, map[string]string{"cached": "x"}) {
		t.Fatalf("a re-check emptied the cache: %v", got)
	}
	// A changed work: answered afresh.
	save(catalog.UpcomingOwned{Ref: catalog.Ref{LibraryID: e.lib.ID, Path: "Saga/2"}, IDKey: "a:B0TWO", WorkID: "two-merged", CheckedAt: "later"})
	want["Saga/2"] = "two-merged"
	if got := listWorkIDs(t, e); !reflect.DeepEqual(got, want) {
		t.Fatalf("after a changed work: %v, want %v", got, want)
	}
	// Resolved from identifiers the book no longer has: no work, answered afresh.
	e.cachedBodies(`{"books":[{"rel_path":"cached","work_id":"x"}]}`)
	save(catalog.UpcomingOwned{Ref: catalog.Ref{LibraryID: e.lib.ID, Path: "Saga/2"}, IDKey: "a:B0OLDTAG", WorkID: "two-merged", CheckedAt: "later"})
	want["Saga/2"] = ""
	if got := listWorkIDs(t, e); !reflect.DeepEqual(got, want) {
		t.Fatalf("after a re-keyed resolution: %v, want %v", got, want)
	}
}

// railEntry is one /meta rail entry's books: its local path ("" = not owned)
// and its local_others paths.
type railEntry struct {
	local  string
	others []string
}

// railEntries is the main rail's entries off /meta, by work id.
func railEntries(t *testing.T, e *sagaEnv, path, token string) (entries map[string]railEntry, raw string) {
	t.Helper()
	resp, body := e.do(t, "GET", e.url("meta", path), token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta %s = %d %s", path, resp.StatusCode, body)
	}
	var env struct {
		Series []struct {
			Works []struct {
				ID          string        `json:"id"`
				Local       *catalog.Ref  `json:"local"`
				LocalOthers []catalog.Ref `json:"local_others"`
			} `json:"works"`
		} `json:"series"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || len(env.Series) == 0 {
		t.Fatalf("decode %v: %s", err, body)
	}
	entries = map[string]railEntry{}
	for _, w := range env.Series[0].Works {
		var en railEntry
		if w.Local != nil {
			if w.Local.LibraryID != e.lib.ID {
				t.Fatalf("local library = %d", w.Local.LibraryID)
			}
			en.local = w.Local.Path
		}
		for _, o := range w.LocalOthers {
			en.others = append(en.others, o.Path)
		}
		entries[w.ID] = en
	}
	return entries, body
}

// TestMetaLocalOthers is the allowed+denied pair for local_others: the admin's
// entry for work "two" is the pick (the narrator of the book the envelope is
// for) with every other recording beside it; the member's holds only the books
// in their grant; the cached envelope carries neither.
func TestMetaLocalOthers(t *testing.T) {
	t.Parallel()
	e := recordingsEnv(t, true)

	entries, _ := railEntries(t, e, "Saga/1", e.adminTok)
	if got, want := entries["two"], (railEntry{local: "Saga/2 Kemper", others: []string{"Saga/2", "Saga/2 Abe"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("admin: entry two = %+v, want %+v", got, want)
	}
	entries, raw := railEntries(t, e, "Saga/1", e.memberTok)
	if got, want := entries["two"], (railEntry{local: "Saga/2", others: []string{"Saga/2 Abe"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("member: entry two = %+v, want %+v", got, want)
	}
	if strings.Contains(raw, "Kemper") {
		t.Fatalf("a recording outside the grant leaked: %s", raw)
	}
	env, err := e.api.meta.Enrich(context.Background(), "B0ONE", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, rail := range env.Series {
		for _, w := range rail.Works {
			if w.Local != nil || w.LocalOthers != nil {
				t.Fatalf("cached envelope annotated: %+v", w)
			}
		}
	}
}

// TestItemAndMetaAgree: the book page's other recordings (/item) and the
// rail's (/meta, the entry the envelope is for) are the same books, neither
// holding a copy of the book sharing its ASIN. A book matched in the console to
// another work than its ASIN's is the one case they differ: /item follows the
// match while /meta stays about the ASIN's work, and each rail entry lists only
// recordings of its own work, so no book is on two entries.
func TestItemAndMetaAgree(t *testing.T) {
	t.Parallel()
	e := recordingsEnv(t, true)
	ctx := context.Background()
	addBooks(t, e.testEnv, e.lib.ID,
		&catalog.Book{RelPath: "Saga/2 copy", Series: "Saga", SeriesIndex: 2, ASIN: "b0two", Narrator: "Bray"},
		&catalog.Book{RelPath: "Saga/3", Series: "Saga", SeriesIndex: 3, ASIN: "B0THREE", Narrator: "Lee"},
	)
	if err := e.cat.SaveUpcomingOwned(ctx, []catalog.UpcomingOwned{
		{Ref: catalog.Ref{LibraryID: e.lib.ID, Path: "Saga/2 copy"}, IDKey: "a:B0TWO", WorkID: "two", CheckedAt: "now"},
		{Ref: catalog.Ref{LibraryID: e.lib.ID, Path: "Saga/3"}, IDKey: "a:B0THREE", WorkID: "three", CheckedAt: "now"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// rail checks /meta for Saga/2: about the ASIN's work and placed as want,
	// each path on one entry only.
	rail := func(token string, want map[string]railEntry) {
		t.Helper()
		entries, raw := railEntries(t, e, "Saga/2", token)
		if !strings.Contains(raw, `"work":{"id":"two"`) {
			t.Fatalf("/meta is no longer about the ASIN's work: %s", raw)
		}
		if !reflect.DeepEqual(entries, want) {
			t.Fatalf("/meta entries = %+v, want %+v", entries, want)
		}
	}
	item := func(token, wantWork string, want []string) {
		t.Helper()
		work, items, _ := itemRecordings(t, e, "Saga/2", token)
		slices.Sort(items)
		if work != wantWork || !reflect.DeepEqual(items, want) {
			t.Fatalf("/item: work %q others %q, want %q %q", work, items, wantWork, want)
		}
	}
	two := railEntry{local: "Saga/2", others: []string{"Saga/2 Abe", "Saga/2 Kemper"}}
	adminRail := map[string]railEntry{"one": {local: "Saga/1"}, "two": two, "three": {local: "Saga/3"}, "four": {}}
	item(e.adminTok, "two", two.others)
	rail(e.adminTok, adminRail)
	// Denied: the member's grant (Saga/1, Saga/2, Saga/2 Abe) holds neither
	// the copy, Saga/2 Kemper nor Saga/3.
	memberTwo := railEntry{local: "Saga/2", others: []string{"Saga/2 Abe"}}
	memberRail := map[string]railEntry{"one": {local: "Saga/1"}, "two": memberTwo, "three": {}, "four": {}}
	item(e.memberTok, "two", memberTwo.others)
	rail(e.memberTok, memberRail)

	// Matched in the console to "three": /item lists that work's recordings,
	// while entry "two" keeps its own and Saga/3 stays on entry "three" alone.
	if err := e.cat.EditBook(ctx, e.lib.ID, "Saga/2", catalog.BookEdit{
		Match: &catalog.MatchAnchor{WorkID: "three"}, Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	item(e.adminTok, "three", []string{"Saga/3"})
	rail(e.adminTok, adminRail)
	item(e.memberTok, "three", nil)
	rail(e.memberTok, memberRail)
}

// TestOtherRecordingsOneEach (server#241): a recording held twice, Saga/2 Abe
// and its copy in a second library (the same ASIN), is listed once on another
// recording's /item and /meta, the copy in the book's own library (/item) or
// the pick's (/meta) kept.
func TestOtherRecordingsOneEach(t *testing.T) {
	t.Parallel()
	e := recordingsEnv(t, true)
	ctx := context.Background()
	second, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Second", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	addBooks(t, e.testEnv, second.ID, &catalog.Book{RelPath: "Saga/2 Abe", Series: "Saga", SeriesIndex: 2, ASIN: "b0twoabe", Narrator: "Abe"})
	if err := e.cat.SaveUpcomingOwned(ctx, []catalog.UpcomingOwned{
		{Ref: catalog.Ref{LibraryID: second.ID, Path: "Saga/2 Abe"}, IDKey: "a:B0TWOABE", WorkID: "two", CheckedAt: "now"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// itemRecordings fails on a recording outside e.lib: the copy never rides.
	if _, others, _ := itemRecordings(t, e, "Saga/2 Kemper", e.adminTok); !reflect.DeepEqual(others, []string{"Saga/2", "Saga/2 Abe"}) {
		t.Fatalf("/item others = %q", others)
	}
	entries, raw := railEntries(t, e, "Saga/1", e.adminTok)
	if got, want := entries["two"], (railEntry{local: "Saga/2 Kemper", others: []string{"Saga/2", "Saga/2 Abe"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("/meta entry two = %+v, want %+v", got, want)
	}
	if strings.Contains(raw, fmt.Sprintf(`"library_id":%d`, second.ID)) {
		t.Fatalf("/meta lists the second library's copy: %s", raw)
	}
}

// TestNextPicksRecording: after a book, Next opens the pick among the next
// work's recordings: the same narrator first; for a caller without that one, a
// recording they started and haven't finished, else the first by path.
func TestNextPicksRecording(t *testing.T) {
	t.Parallel()
	e := recordingsEnv(t, true)

	if n := getNext(t, e.testEnv, e.url("next", "Saga/1"), e.adminTok); n.Source != nextCommunity || n.nextPath() != "Saga/2 Kemper" {
		t.Fatalf("admin next = %s", n.raw)
	}
	// The member can't open Kemper's: first by path.
	if n := getNext(t, e.testEnv, e.url("next", "Saga/1"), e.memberTok); n.Source != nextCommunity || n.nextPath() != "Saga/2" {
		t.Fatalf("member next = %s", n.raw)
	}
	// Started (by the member, not finished) beats path order.
	ctx := context.Background()
	if _, err := e.cat.SaveProgress(ctx, e.memberID, catalog.Progress{Ref: catalog.Ref{LibraryID: e.lib.ID, Path: "Saga/2 Abe"},
		Position: 30, Duration: 600, UpdatedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if n := getNext(t, e.testEnv, e.url("next", "Saga/1"), e.memberTok); n.nextPath() != "Saga/2 Abe" {
		t.Fatalf("member next after starting Abe's = %s", n.raw)
	}
	// Another listener's progress is not the admin's: the narrator still decides.
	if n := getNext(t, e.testEnv, e.url("next", "Saga/1"), e.adminTok); n.nextPath() != "Saga/2 Kemper" {
		t.Fatalf("admin next after the member's progress = %s", n.raw)
	}
}
