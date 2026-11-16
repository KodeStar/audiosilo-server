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

// railTwo is the main rail's entry for work "two" off /meta: its local path and
// its local_others paths.
func railTwo(t *testing.T, e *sagaEnv, path, token string) (local string, others []string, raw string) {
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
	for _, w := range env.Series[0].Works {
		if w.ID != "two" {
			continue
		}
		if w.Local != nil {
			local = w.Local.Path
		}
		for _, o := range w.LocalOthers {
			others = append(others, o.Path)
		}
	}
	return local, others, body
}

// TestMetaLocalOthers is the allowed+denied pair for local_others: the admin's
// entry for work "two" is the pick (the narrator of the book the envelope is
// for) with every other recording beside it; the member's holds only the books
// in their grant; the cached envelope carries neither.
func TestMetaLocalOthers(t *testing.T) {
	t.Parallel()
	e := recordingsEnv(t, true)

	local, others, _ := railTwo(t, e, "Saga/1", e.adminTok)
	if want := []string{"Saga/2", "Saga/2 Abe"}; local != "Saga/2 Kemper" || !reflect.DeepEqual(others, want) {
		t.Fatalf("admin: local %q, others %q", local, others)
	}
	local, others, raw := railTwo(t, e, "Saga/1", e.memberTok)
	if want := []string{"Saga/2 Abe"}; local != "Saga/2" || !reflect.DeepEqual(others, want) {
		t.Fatalf("member: local %q, others %q", local, others)
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
// rail's (/meta, the entry the envelope is for) are the same books: neither
// holds a copy of the book sharing its ASIN, and a book matched in the console
// to another work than its ASIN's lists that work's recordings on both, while
// /meta stays about its ASIN's work.
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
	agree := func(wantWork string, want []string) {
		t.Helper()
		work, items, _ := itemRecordings(t, e, "Saga/2", e.adminTok)
		local, rail, raw := railTwo(t, e, "Saga/2", e.adminTok)
		slices.Sort(items)
		slices.Sort(rail)
		if work != wantWork || local != "Saga/2" || !reflect.DeepEqual(items, want) || !reflect.DeepEqual(rail, want) {
			t.Fatalf("work %q: /item %q, /meta local %q others %q, want %q", work, items, local, rail, want)
		}
		if !strings.Contains(raw, `"work":{"id":"two"`) {
			t.Fatalf("/meta is no longer about the ASIN's work: %s", raw)
		}
	}
	agree("two", []string{"Saga/2 Abe", "Saga/2 Kemper"})
	if err := e.cat.EditBook(ctx, e.lib.ID, "Saga/2", catalog.BookEdit{
		Match: &catalog.MatchAnchor{WorkID: "three"}, Source: catalog.SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	agree("three", []string{"Saga/3"})
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
