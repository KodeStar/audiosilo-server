package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The book-path rule (server#210): bookmarks, notes and history follow
// progress (timelinePath: a track's are its book's, a disc folder keeps its
// own timeline), a rating's GET and DELETE read the book's like its PUT, a
// whole-list replace and a remove map a part path or another spelling to its
// book, and a favourite stays on the path given (spelling only).

// annotationBody is a valid POST body of a bookmark or a note.
var annotationBody = map[string]string{"bookmarks": `{"position":42,"label":"x"}`, "notes": `{"position":42,"body":"x"}`}

// annotation is one bookmark or note as the routes answer it.
type annotation struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
}

// annotations GETs a per-book list (bookmarks, notes, history) at path as tok.
func (e *progressEnv) annotations(t *testing.T, tok, endpoint, path string) (int, []annotation) {
	t.Helper()
	resp, raw := e.do(t, "GET", bookURL(e.lib.ID, endpoint, path), tok, "")
	var out map[string][]annotation
	_ = json.Unmarshal([]byte(raw), &out)
	return resp.StatusCode, out[endpoint]
}

// TestAnnotationsBookPath (allowed): a bookmark, a note or a history span
// posted at a track lands on its book and answers the book's path; GET at any
// track reads the book's. A disc folder of a joined book keeps its own (a
// download of that disc writes there, on the disc's timeline).
func TestAnnotationsBookPath(t *testing.T) {
	t.Parallel()
	e := newProgressEnv(t)
	ctx := context.Background()
	uid, tok := newUserWithSession(t, e.testEnv, "sam", false)
	if err := e.cat.GrantWholeLibrary(ctx, uid, e.lib.ID); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"bookmarks", "notes"} {
		resp, raw := e.do(t, "POST", bookURL(e.lib.ID, endpoint, pPart2), tok, annotationBody[endpoint])
		var saved annotation
		if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(raw), &saved) != nil || saved.Path != pBook {
			t.Fatalf("POST %s at a track = %d %s, want the book's path", endpoint, resp.StatusCode, raw)
		}
		for _, p := range []string{pBook, pPart1, pPart2} {
			if status, got := e.annotations(t, tok, endpoint, p); status != http.StatusOK || len(got) != 1 || got[0].Path != pBook {
				t.Errorf("GET %s at %s = %d %+v, want the book's one", endpoint, p, status, got)
			}
		}
		resp, raw = e.do(t, "POST", bookURL(e.lib.ID, endpoint, pDisc2), tok, annotationBody[endpoint])
		if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(raw), &saved) != nil || saved.Path != pDisc2 {
			t.Fatalf("POST %s at a disc folder = %d %s, want the disc's own path", endpoint, resp.StatusCode, raw)
		}
		if _, got := e.annotations(t, tok, endpoint, pJoined); len(got) != 0 {
			t.Errorf("the joined book's %s = %+v, want none (the disc keeps its own)", endpoint, got)
		}
	}
	span := `{"from_pos":10,"to_pos":70,"started_at":"2026-01-01T10:00:00Z","ended_at":"2026-01-01T10:01:00Z"}`
	if resp, raw := e.do(t, "POST", bookURL(e.lib.ID, "history", pPart1), tok, span); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST history at a track = %d %s", resp.StatusCode, raw)
	}
	var atBook, elsewhere int
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE rel_path = ?), COUNT(*) FILTER (WHERE rel_path <> ?)
		FROM listening_history WHERE user_id = ?`, pBook, pBook, uid).Scan(&atBook, &elsewhere); err != nil {
		t.Fatal(err)
	}
	if atBook != 1 || elsewhere != 0 {
		t.Fatalf("history rows: %d at the book, %d elsewhere; want 1 and 0", atBook, elsewhere)
	}
	resp, raw := e.do(t, "GET", bookURL(e.lib.ID, "history", pPart2), tok, "")
	var hist struct {
		History []json.RawMessage `json:"history"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(raw), &hist) != nil || len(hist.History) != 1 {
		t.Fatalf("GET history at a track = %d %s, want the book's span", resp.StatusCode, raw)
	}
}

// TestAnnotationsBookPathDenied (security): a grant reaching only one track
// (its book outside it) keeps that track's own bookmarks, notes and history and
// never reads or writes the book's; a path outside scope is 403 and writes
// nothing.
func TestAnnotationsBookPathDenied(t *testing.T) {
	t.Parallel()
	e := newProgressEnv(t)
	ctx := context.Background()
	kid, kidTok := newUserWithSession(t, e.testEnv, "kid", false)
	share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "One part",
		Paths: []catalog.PathRule{{LibraryID: e.lib.ID, Path: pPart1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, kid, share.ID); err != nil {
		t.Fatal(err)
	}
	// The kid's bookmark on the book, from when they had it.
	if _, err := e.cat.AddBookmark(ctx, kid, catalog.Bookmark{Ref: catalog.Ref{LibraryID: e.lib.ID, Path: pBook}, Position: 999}); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"bookmarks", "notes"} {
		resp, raw := e.do(t, "POST", bookURL(e.lib.ID, endpoint, pPart1), kidTok, annotationBody[endpoint])
		var saved annotation
		if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(raw), &saved) != nil || saved.Path != pPart1 {
			t.Fatalf("kid POST %s at the granted track = %d %s, want its own path", endpoint, resp.StatusCode, raw)
		}
		if status, got := e.annotations(t, kidTok, endpoint, pPart1); status != http.StatusOK || len(got) != 1 || got[0].Path != pPart1 {
			t.Errorf("kid GET %s at the granted track = %d %+v, want only its own", endpoint, status, got)
		}
		for _, p := range []string{pPart2, pBook} {
			if resp, _ := e.do(t, "POST", bookURL(e.lib.ID, endpoint, p), kidTok, annotationBody[endpoint]); resp.StatusCode != http.StatusForbidden {
				t.Errorf("kid POST %s at %s = %d, want 403", endpoint, p, resp.StatusCode)
			}
			if status, _ := e.annotations(t, kidTok, endpoint, p); status != http.StatusForbidden {
				t.Errorf("kid GET %s at %s = %d, want 403", endpoint, p, status)
			}
		}
	}
	marks, err := e.cat.ListBookmarks(ctx, kid, catalog.Ref{LibraryID: e.lib.ID, Path: pBook})
	if err != nil || len(marks) != 1 || marks[0].Position != 999 {
		t.Fatalf("the book's bookmarks = %+v %v, want only the old one", marks, err)
	}
}

// TestRatingBookPath: GET and DELETE of a rating at a track (or a disc folder)
// read and clear the book's, as PUT writes it; a grant reaching only the track
// reads the track's own (none) and clears nothing of the book's.
func TestRatingBookPath(t *testing.T) {
	t.Parallel()
	e := newProgressEnv(t)
	ctx := context.Background()
	uid, tok := newUserWithSession(t, e.testEnv, "sam", false)
	if err := e.cat.GrantWholeLibrary(ctx, uid, e.lib.ID); err != nil {
		t.Fatal(err)
	}
	rated := func(tok, p string) *float64 {
		t.Helper()
		resp, raw := e.do(t, "GET", bookURL(e.lib.ID, "rating", p), tok, "")
		var out struct {
			Rating *struct {
				Path   string  `json:"path"`
				Rating float64 `json:"rating"`
			} `json:"rating"`
		}
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(raw), &out) != nil {
			t.Fatalf("GET rating %s = %d %s", p, resp.StatusCode, raw)
		}
		if out.Rating == nil {
			return nil
		}
		return &out.Rating.Rating
	}
	for book, part := range map[string]string{pBook: pPart2, pJoined: pDisc2} {
		if resp, raw := e.do(t, "PUT", bookURL(e.lib.ID, "rating", book), tok, `{"rating":4}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT rating = %d %s", resp.StatusCode, raw)
		}
		if r := rated(tok, part); r == nil || *r != 4 {
			t.Fatalf("GET rating at %s = %v, want the book's 4", part, r)
		}
		if resp, _ := e.do(t, "DELETE", bookURL(e.lib.ID, "rating", part), tok, ""); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE rating at %s = %d", part, resp.StatusCode)
		}
		if r := rated(tok, book); r != nil {
			t.Fatalf("after DELETE at %s the book's rating = %v, want none", part, *r)
		}
	}

	// Denied: a grant reaching only one track.
	kid, kidTok := newUserWithSession(t, e.testEnv, "kid", false)
	share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "One part",
		Paths: []catalog.PathRule{{LibraryID: e.lib.ID, Path: pPart1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, kid, share.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cat.SetRating(ctx, kid, catalog.Ref{LibraryID: e.lib.ID, Path: pBook}, 5, ""); err != nil {
		t.Fatal(err)
	}
	if r := rated(kidTok, pPart1); r != nil {
		t.Fatalf("kid's GET at the granted track = %v, want none (the book is outside the grant)", *r)
	}
	if resp, _ := e.do(t, "DELETE", bookURL(e.lib.ID, "rating", pPart1), kidTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("kid's DELETE at the granted track = %d", resp.StatusCode)
	}
	if r, err := e.cat.GetRating(ctx, kid, catalog.Ref{LibraryID: e.lib.ID, Path: pBook}); err != nil || r == nil {
		t.Fatalf("the kid's DELETE at a track cleared the book's rating: %+v %v", r, err)
	}
	for _, m := range []string{"GET", "DELETE"} {
		if resp, _ := e.do(t, m, bookURL(e.lib.ID, "rating", pPart2), kidTok, ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("kid %s rating outside the grant = %d, want 403", m, resp.StatusCode)
		}
	}
}

// TestFavouriteTrackStaysOnTrack (DECIDE 2): a favourite on a track stays on
// the track, so a shipped player's Folders heart (on by path equality) stays on.
func TestFavouriteTrackStaysOnTrack(t *testing.T) {
	t.Parallel()
	e := newProgressEnv(t)
	ctx := context.Background()
	uid, tok := newUserWithSession(t, e.testEnv, "sam", false)
	if err := e.cat.GrantWholeLibrary(ctx, uid, e.lib.ID); err != nil {
		t.Fatal(err)
	}
	if resp, raw := e.do(t, "POST", bookURL(e.lib.ID, "favourites", pPart1), tok, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST favourite at a track = %d %s", resp.StatusCode, raw)
	}
	paths := storedPaths(t, e.testEnv, `SELECT rel_path FROM favourites WHERE user_id = ?`, uid)
	if !slices.Equal(paths, []string{pPart1}) {
		t.Fatalf("favourites = %+q, want the track itself", paths)
	}
}

// TestListReplaceBookPath (§15, an old player's queue replay): a whole-list
// replace naming a book by a track path or another spelling keeps that book
// (with its added_at), where it used to drop the entry and delete the stored
// one; refs naming one book collapse to its first place; an out-of-scope ref
// is still skipped. A remove by a track path or another spelling takes the
// book's entry off. The same for a collection.
func TestListReplaceBookPath(t *testing.T) {
	t.Parallel()
	s := newSpellingEnv(t)
	ctx := context.Background()
	stored := func(q string, owner int64) []string { return storedPaths(t, s.testEnv, q, owner) }
	queued := func() []string {
		return stored(`SELECT rel_path FROM up_next WHERE user_id = ? ORDER BY position`, s.sam)
	}
	refs := func(paths ...string) string {
		items := make([]catalog.Ref, len(paths))
		for i, p := range paths {
			items[i] = catalog.Ref{LibraryID: s.lib.ID, Path: p}
		}
		b, _ := json.Marshal(map[string]any{"items": items})
		return string(b)
	}
	for _, p := range []string{spNFCBook, spOther, spNFDOnDisk} {
		add, _ := json.Marshal(map[string]any{"library_id": s.lib.ID, "path": p})
		if resp, raw := s.do(t, "POST", "/api/v1/me/queue", s.samTok, string(add)); resp.StatusCode != http.StatusOK {
			t.Fatalf("queue add %+q = %d %s", p, resp.StatusCode, raw)
		}
	}
	var addedBefore string
	if err := s.db.QueryRowContext(ctx, `SELECT added_at FROM up_next WHERE user_id = ? AND rel_path = ?`, s.sam, spNFCBook).Scan(&addedBefore); err != nil {
		t.Fatal(err)
	}
	// A 2.4.0 player's replay: a track of Germinal, Germinal again in NFD, the
	// NFD-on-disk book in NFC, Other by its track.
	if resp, raw := s.do(t, "PUT", "/api/v1/me/queue", s.samTok,
		refs(spNFCTrack, spNFDBook, spNFCOfNFD, spOther+"/01.mp3")); resp.StatusCode != http.StatusOK {
		t.Fatalf("queue replace = %d %s", resp.StatusCode, raw)
	}
	if got := queued(); !slices.Equal(got, []string{spNFCBook, spNFDOnDisk, spOther}) {
		t.Fatalf("queue after the replay = %+q, want every book kept, the duplicate collapsed", got)
	}
	var addedAfter string
	if err := s.db.QueryRowContext(ctx, `SELECT added_at FROM up_next WHERE user_id = ? AND rel_path = ?`, s.sam, spNFCBook).Scan(&addedAfter); err != nil || addedAfter != addedBefore {
		t.Fatalf("added_at = %q %v, want the stored %q", addedAfter, err, addedBefore)
	}
	// A remove by a track path, and one in another spelling.
	for _, p := range []string{spNFDTrack, spNFCOfNFD} {
		remove := "/api/v1/me/queue?library_id=" + strconv.FormatInt(s.lib.ID, 10) + "&path=" + url.QueryEscape(p)
		if resp, raw := s.do(t, "DELETE", remove, s.samTok, ""); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("queue remove %+q = %d %s", p, resp.StatusCode, raw)
		}
	}
	if got := queued(); !slices.Equal(got, []string{spOther}) {
		t.Fatalf("queue after the removes = %+q, want only %s", got, spOther)
	}

	// Denied: kid (only "Other") replaces with a track of a book outside scope:
	// skipped, and nothing of it is stored.
	if resp, raw := s.do(t, "PUT", "/api/v1/me/queue", s.kidTok, refs(spNFCTrack, spNFDBook, spOther+"/01.mp3")); resp.StatusCode != http.StatusOK {
		t.Fatalf("kid's replace = %d %s", resp.StatusCode, raw)
	}
	if got := stored(`SELECT rel_path FROM up_next WHERE user_id = ? ORDER BY position`, s.kid); !slices.Equal(got, []string{spOther}) {
		t.Fatalf("kid's queue = %+q, want only %s", got, spOther)
	}

	// A collection: the same replace and remove rules.
	resp, raw := s.do(t, "POST", "/api/v1/me/collections", s.samTok, `{"name":"Mine"}`)
	var created struct {
		Collection struct {
			ID int64 `json:"id"`
		} `json:"collection"`
	}
	if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(raw), &created) != nil {
		t.Fatalf("create collection = %d %s", resp.StatusCode, raw)
	}
	colURL := "/api/v1/me/collections/" + strconv.FormatInt(created.Collection.ID, 10) + "/items"
	items := func() []string {
		return stored(`SELECT rel_path FROM collection_items WHERE collection_id = ? ORDER BY position`, created.Collection.ID)
	}
	if resp, raw := s.do(t, "PUT", colURL, s.samTok, refs(spNFDTrack, spNFCBook, spNFCOfNFD)); resp.StatusCode != http.StatusOK {
		t.Fatalf("collection replace = %d %s", resp.StatusCode, raw)
	}
	if got := items(); !slices.Equal(got, []string{spNFCBook, spNFDOnDisk}) {
		t.Fatalf("collection = %+q, want both books once", got)
	}
	remove := colURL + "?library_id=" + strconv.FormatInt(s.lib.ID, 10) + "&path=" + url.QueryEscape(spNFCTrack)
	if resp, raw := s.do(t, "DELETE", remove, s.samTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("collection remove = %d %s", resp.StatusCode, raw)
	}
	if got := items(); !slices.Equal(got, []string{spNFDOnDisk}) {
		t.Fatalf("collection after the remove = %+q", got)
	}
}
