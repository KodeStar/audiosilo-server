package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	unorm "golang.org/x/text/unicode/norm"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Path spelling (server#210): a path in another Unicode spelling of an indexed
// path reads as the index's, on every path-addressed route, after the same
// scope and hidden checks. Spellings from explicit code points: NFC the
// precomposed letter, NFD the letter then U+0301.
const (
	spNFCBook   = "Émile Zola/Germinal" // NFC on disk
	spNFDBook   = "Émile Zola/Germinal"
	spNFCTrack  = spNFCBook + "/02.mp3"
	spNFDTrack  = spNFDBook + "/02.mp3"
	spNFDAuthor = "Émile Zola"
	spNFCAuthor = "Émile Zola"
	spNFDOnDisk = "Amélie/Le Fabuleux" // NFD on disk
	spNFCOfNFD  = "Amélie/Le Fabuleux"
	spOther     = "Other/Book"
)

// spellingEnv: a library with an NFC book (two tracks), a book NFD on disk and
// an ASCII one, scanned; sam reaches the whole library, kid only "Other", and
// vari a share whose rule spells the NFC author folder in NFD (a rule matches
// byte for byte, so it reaches no indexed book).
type spellingEnv struct {
	*testEnv
	lib                  *catalog.Library
	sam, kid             int64
	samTok, kidTok, vTok string
}

func newSpellingEnv(t *testing.T) *spellingEnv {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, spNFCBook+"/01.mp3", spNFCTrack, spNFDOnDisk+"/01.mp3", spOther+"/01.mp3")
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	for _, p := range []string{spNFCBook, spNFDOnDisk, spOther} {
		if _, err := e.cat.GetBookByPath(ctx, lib.ID, p); err != nil {
			t.Fatalf("fixture book %+q: %v", p, err)
		}
	}
	s := &spellingEnv{testEnv: e, lib: lib}
	s.sam, s.samTok = newUserWithSession(t, e, "sam", false)
	if err := e.cat.GrantWholeLibrary(ctx, s.sam, lib.ID); err != nil {
		t.Fatal(err)
	}
	s.kid, s.kidTok = newUserWithSession(t, e, "kid", false)
	vari, vTok := newUserWithSession(t, e, "vari", false)
	s.vTok = vTok
	for user, rule := range map[int64]string{s.kid: "Other", vari: spNFDAuthor} {
		share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Only " + strconv.FormatInt(user, 10),
			Paths: []catalog.PathRule{{LibraryID: lib.ID, Path: rule}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.cat.GrantShare(ctx, user, share.ID); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *spellingEnv) get(t *testing.T, tok, endpoint, path string) (int, string) {
	t.Helper()
	resp, body := s.do(t, "GET", bookURL(s.lib.ID, endpoint, path), tok, "")
	return resp.StatusCode, body
}

// TestSpellingContentRoutes (allowed): item, chapters, stream, cover and next
// answer another spelling of a path exactly as its index spelling, both ways
// round (an NFD request for an NFC book, an NFC one for a book NFD on disk).
func TestSpellingContentRoutes(t *testing.T) {
	t.Parallel()
	s := newSpellingEnv(t)
	for _, tc := range []struct{ endpoint, index, other string }{
		{"item", spNFCBook, spNFDBook},
		{"chapters", spNFCBook, spNFDBook},
		{"item", spNFDOnDisk, spNFCOfNFD},
		{"item", spNFCTrack, spNFDTrack},
		{"stream", spNFCTrack, spNFDTrack},
		{"cover", spNFCBook, spNFDBook},
		{"next", spNFCBook, spNFDBook},
	} {
		wantStatus, wantBody := s.get(t, s.samTok, tc.endpoint, tc.index)
		status, body := s.get(t, s.samTok, tc.endpoint, tc.other)
		if status != wantStatus || body != wantBody {
			t.Errorf("%s %+q = %d %.200s\nwant (as %+q) %d %.200s", tc.endpoint, tc.other, status, body, tc.index, wantStatus, wantBody)
		}
		if tc.endpoint == "item" && wantStatus != http.StatusOK {
			t.Errorf("item %+q = %d, want 200", tc.index, wantStatus)
		}
	}
	// The answer is the index's book.
	var item catalog.Book
	_, body := s.get(t, s.samTok, "item", spNFCOfNFD)
	if err := json.Unmarshal([]byte(body), &item); err != nil || item.RelPath != spNFDOnDisk {
		t.Fatalf("item by NFC of an NFD book = %+q %v, want %+q", item.RelPath, err, spNFDOnDisk)
	}
}

// TestSpellingContentRoutesDenied (security): another spelling reaches nothing
// its index spelling could not. A book outside the caller's scope is the same
// 403 in either spelling; a hidden path in another spelling is 404 in scope and
// 403 outside it; and a spelling the caller's rule names byte for byte while
// the index's lies outside it reads as missing, exactly as a path with nothing
// there, so the answer never tells them the book exists.
func TestSpellingContentRoutesDenied(t *testing.T) {
	t.Parallel()
	s := newSpellingEnv(t)
	for _, p := range []string{spNFCBook, spNFDBook, spNFDTrack, spNFCOfNFD} {
		for _, endpoint := range []string{"item", "chapters", "stream", "cover", "next"} {
			if status, body := s.get(t, s.kidTok, endpoint, p); status != http.StatusForbidden {
				t.Errorf("kid %s %+q = %d %s, want 403", endpoint, p, status, body)
			}
		}
	}
	hidden := spNFDBook + "/.audiosilo/readalong.json"
	if status, _ := s.get(t, s.samTok, "item", hidden); status != http.StatusNotFound {
		t.Errorf("sam item of a hidden path = %d, want 404", status)
	}
	if status, _ := s.get(t, s.kidTok, "item", hidden); status != http.StatusForbidden {
		t.Errorf("kid item of a hidden path = %d, want 403", status)
	}
	// vari's rule is the NFD author folder: the NFD book path is inside it, the
	// indexed NFC book is not. It reads as a path with nothing there, answer for
	// answer (status, body, type), on the routes that open the file on disk too
	// (a disk that ignores the spelling, macOS's or a share's, would open the
	// NFC book's files by the NFD path) and on the listener's own state.
	missingBook, missingTrack := spNFDAuthor+"/Nana", spNFDAuthor+"/Nana/01.mp3"
	same := func(what, path, missing string) {
		t.Helper()
		want, wantBody := s.do(t, "GET", missing, s.vTok, "")
		got, body := s.do(t, "GET", path, s.vTok, "")
		if got.StatusCode != want.StatusCode || body != wantBody || got.Header.Get("Content-Type") != want.Header.Get("Content-Type") {
			t.Errorf("vari %s = %d %.120s (%s), want %d %.120s (%s) as for a path with nothing there", what,
				got.StatusCode, body, got.Header.Get("Content-Type"), want.StatusCode, wantBody, want.Header.Get("Content-Type"))
		}
	}
	for _, tc := range []struct{ endpoint, path, missing string }{
		{"item", spNFDBook, missingBook},
		{"chapters", spNFDBook, missingBook},
		{"cover", spNFDBook, missingBook},
		{"next", spNFDBook, missingBook},
		{"stream", spNFDTrack, missingTrack},
		{"ebook", spNFDBook, missingBook},
		{"readalong", spNFDBook, missingBook},
		{"meta", spNFDBook, missingBook},
		{"progress", spNFDBook, missingBook},
		{"bookmarks", spNFDBook, missingBook},
		{"notes", spNFDBook, missingBook},
		{"history", spNFDBook, missingBook},
		{"rating", spNFDBook, missingBook},
		{"reading-progress", spNFDBook, missingBook},
	} {
		same(tc.endpoint+" "+tc.path, bookURL(s.lib.ID, tc.endpoint, tc.path), bookURL(s.lib.ID, tc.endpoint, tc.missing))
		if status, _ := s.get(t, s.vTok, tc.endpoint, unorm.NFC.String(tc.path)); status != http.StatusForbidden {
			t.Errorf("vari %s %+q = %d, want 403", tc.endpoint, unorm.NFC.String(tc.path), status)
		}
	}
	// The stream's other forms open the file too: a download, a transcode, a
	// rendition. And /fs lists the disk: the NFD folder is not listed from it.
	for _, extra := range []string{"&download=1", "&transcode=1", "&original=1", "&rendition=watch"} {
		same("stream"+extra, bookURL(s.lib.ID, "stream", spNFDTrack)+extra, bookURL(s.lib.ID, "stream", missingTrack)+extra)
	}
	for _, p := range []string{spNFDBook, spNFDAuthor} {
		same("fs "+p, bookURL(s.lib.ID, "fs", p), bookURL(s.lib.ID, "fs", missingBook))
	}
	// Over the cap is still 400 before any lookup.
	long := spNFDBook + "/" + string(make([]byte, catalog.MaxPathBytes))
	if status, _ := s.get(t, s.samTok, "item", long); status != http.StatusBadRequest {
		t.Errorf("item over the cap = %d, want 400", status)
	}
}

// TestSpellingUserState: progress, sync events, favourites, the queue and a
// cast token given another spelling key on the index's path: no side row in the
// spelling given. Denied: a caller outside scope writes nothing in either.
func TestSpellingUserState(t *testing.T) {
	t.Parallel()
	s := newSpellingEnv(t)
	ctx := context.Background()
	ref := func(p string) catalog.Ref { return catalog.Ref{LibraryID: s.lib.ID, Path: p} }

	// Progress: PUT in NFD lands on the NFC book; GET in either spelling reads it.
	resp, body := s.do(t, "PUT", bookURL(s.lib.ID, "progress", spNFDBook), s.samTok, `{"position":12,"duration":300}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT progress NFD = %d %s", resp.StatusCode, body)
	}
	for _, p := range []string{spNFCBook, spNFDBook, spNFDTrack} {
		var out struct {
			Progress *catalog.Progress `json:"progress"`
		}
		if _, body := s.get(t, s.samTok, "progress", p); json.Unmarshal([]byte(body), &out) != nil ||
			out.Progress == nil || out.Progress.Position != 12 || out.Progress.Path != spNFCBook {
			t.Errorf("GET progress %+q = %s, want the book's row", p, body)
		}
	}
	if p, err := s.cat.GetProgress(ctx, s.sam, ref(spNFDBook)); err != nil || p != nil {
		t.Fatalf("a side row in the NFD spelling: %+v %v", p, err)
	}

	// A sync event on an NFD track keys on the book.
	resp, body = s.do(t, "POST", syncURL, s.samTok, syncBody("dev-1", listenJSON("e1", s.lib.ID, spNFDTrack, 20, 80, "")))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync = %d %s", resp.StatusCode, body)
	}
	var events int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM listening_events WHERE user_id = ? AND rel_path <> ?`, s.sam, spNFCBook).Scan(&events); err != nil || events != 0 {
		t.Fatalf("sync events off the book's path: %d %v", events, err)
	}

	// Favourites: in the index spelling, so the list finds the book.
	if resp, body := s.do(t, "POST", bookURL(s.lib.ID, "favourites", spNFDBook), s.samTok, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST favourite NFD = %d %s", resp.StatusCode, body)
	}
	var favs struct {
		Favourites []struct {
			Path   string `json:"path"`
			IsBook bool   `json:"is_book"`
		} `json:"favourites"`
	}
	_, body = s.do(t, "GET", "/api/v1/me/favourites", s.samTok, "")
	if err := json.Unmarshal([]byte(body), &favs); err != nil || len(favs.Favourites) != 1 ||
		favs.Favourites[0].Path != spNFCBook || !favs.Favourites[0].IsBook {
		t.Fatalf("favourites = %s, want the NFC book with its book", body)
	}

	// The queue: an add in NFD stores the book; a remove in NFD takes it off.
	add, _ := json.Marshal(map[string]any{"library_id": s.lib.ID, "path": spNFDBook})
	if resp, body := s.do(t, "POST", "/api/v1/me/queue", s.samTok, string(add)); resp.StatusCode != http.StatusOK {
		t.Fatalf("queue add NFD = %d %s", resp.StatusCode, body)
	}
	queued := func() []string {
		return storedPaths(t, s.testEnv, `SELECT rel_path FROM up_next WHERE user_id = ? ORDER BY position`, s.sam)
	}
	if got := queued(); len(got) != 1 || got[0] != spNFCBook {
		t.Fatalf("queued = %+q, want the NFC book", got)
	}
	remove := "/api/v1/me/queue?library_id=" + strconv.FormatInt(s.lib.ID, 10) + "&path=" + url.QueryEscape(spNFDBook)
	if resp, body := s.do(t, "DELETE", remove, s.samTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("queue remove NFD = %d %s", resp.StatusCode, body)
	}
	if got := queued(); len(got) != 0 {
		t.Fatalf("after the NFD remove the queue holds %+q", got)
	}

	// A cast token's grant is the index's path.
	mint, _ := json.Marshal(map[string]any{"library_id": s.lib.ID, "path": spNFDBook})
	if resp, body := s.do(t, "POST", "/api/v1/cast/token", s.samTok, string(mint)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("cast token NFD = %d %s", resp.StatusCode, body)
	}
	var granted string
	if err := s.db.QueryRowContext(ctx, `SELECT rel_path FROM cast_grants ORDER BY token_id DESC LIMIT 1`).Scan(&granted); err != nil || granted != spNFCBook {
		t.Fatalf("cast grant = %+q %v, want %+q", granted, err, spNFCBook)
	}

	// Denied: kid writes nothing, in either spelling.
	for _, p := range []string{spNFDBook, spNFCBook} {
		if resp, _ := s.do(t, "PUT", bookURL(s.lib.ID, "progress", p), s.kidTok, `{"position":5,"duration":300}`); resp.StatusCode != http.StatusForbidden {
			t.Errorf("kid PUT progress %+q = %d, want 403", p, resp.StatusCode)
		}
		if resp, _ := s.do(t, "POST", bookURL(s.lib.ID, "favourites", p), s.kidTok, ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("kid POST favourite %+q = %d, want 403", p, resp.StatusCode)
		}
	}
	var kidRows int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM progress WHERE user_id = ?1) +
		(SELECT COUNT(*) FROM favourites WHERE user_id = ?1)`, s.kid).Scan(&kidRows); err != nil || kidRows != 0 {
		t.Fatalf("kid's rows = %d %v, want none", kidRows, err)
	}
}

// TestSpellingBrowseFS: /fs asked for a folder in another spelling lists it in
// the index's, so its entries are the indexed books' paths and read as books,
// not as folders no scan has reached. A caller who can't see the folder is
// listed nothing of it.
func TestSpellingBrowseFS(t *testing.T) {
	t.Parallel()
	s := newSpellingEnv(t)
	resp, body := s.do(t, "GET", bookURL(s.lib.ID, "fs", spNFDAuthor), s.samTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fs NFD = %d %s", resp.StatusCode, body)
	}
	var listing struct {
		Path    string `json:"path"`
		Entries []struct {
			Path    string `json:"path"`
			IsBook  bool   `json:"is_book"`
			Pending bool   `json:"pending_book"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Path != spNFCAuthor || len(listing.Entries) != 1 || listing.Entries[0].Path != spNFCBook ||
		!listing.Entries[0].IsBook || listing.Entries[0].Pending {
		t.Fatalf("fs NFD = %s, want the NFC folder with its book", body)
	}
	if resp, body := s.do(t, "GET", bookURL(s.lib.ID, "fs", spNFDAuthor), s.kidTok, ""); resp.StatusCode == http.StatusOK {
		_ = json.Unmarshal([]byte(body), &listing)
		if len(listing.Entries) != 0 {
			t.Fatalf("kid's fs of the folder = %s, want nothing of it", body)
		}
	}
}

// TestSpellingLookupFails: when the index can't say how it spells a path, the
// route answers 500 rather than reading or saving under the spelling given.
func TestSpellingLookupFails(t *testing.T) {
	t.Parallel()
	s := newSpellingEnv(t)
	ctx := context.Background()
	// A track is looked up among its book's files: without the table it fails.
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE book_files RENAME TO book_files_gone`); err != nil {
		t.Fatal(err)
	}
	if status, _ := s.get(t, s.samTok, "item", spNFDTrack); status != http.StatusInternalServerError {
		t.Errorf("item with the lookup failing = %d, want 500", status)
	}
	if resp, _ := s.do(t, "PUT", bookURL(s.lib.ID, "progress", spNFDTrack), s.samTok, `{"position":5,"duration":300}`); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("PUT progress with the lookup failing = %d, want 500", resp.StatusCode)
	}
	if p, err := s.cat.GetProgress(ctx, s.sam, catalog.Ref{LibraryID: s.lib.ID, Path: spNFDTrack}); err != nil || p != nil {
		t.Errorf("a row was saved in the spelling given: %+v %v", p, err)
	}
}

// storedPaths runs q (one text column) with args and returns its rows.
func storedPaths(t *testing.T, e *testEnv, q string, args ...any) []string {
	t.Helper()
	rows, err := e.db.QueryContext(context.Background(), q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSpellingStrayRows: a row an older server stored in another spelling than
// the index's (a side row) can still be cleaned up by the path the lists answer
// it with: a favourite's and a rating's DELETE take off the row at the path
// given as well as the index spelling's, and an admin's progress edit names
// the person's row by its own path (exact), never moving to the index's.
func TestSpellingStrayRows(t *testing.T) {
	t.Parallel()
	s := newSpellingEnv(t)
	ctx := context.Background()
	count := func(table string) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE user_id = ? AND rel_path = ?`, s.sam, spNFDBook).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO favourites(user_id, library_id, rel_path, created_at) VALUES(?, ?, ?, 't')`,
		s.sam, s.lib.ID, spNFDBook); err != nil {
		t.Fatal(err)
	}
	if _, err := s.cat.SetRating(ctx, s.sam, catalog.Ref{LibraryID: s.lib.ID, Path: spNFDBook}, 3, ""); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"favourites", "rating"} {
		if resp, body := s.do(t, "DELETE", bookURL(s.lib.ID, endpoint, spNFDBook), s.samTok, ""); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE %s NFD = %d %s", endpoint, resp.StatusCode, body)
		}
	}
	if fav, rated := count("favourites"), count("ratings"); fav != 0 || rated != 0 {
		t.Fatalf("side rows left after the removes: %d favourites, %d ratings", fav, rated)
	}

	if _, err := s.cat.SaveProgress(ctx, s.sam, catalog.Progress{Ref: catalog.Ref{LibraryID: s.lib.ID, Path: spNFDBook},
		Position: 7, Duration: 300}); err != nil {
		t.Fatal(err)
	}
	edit := "/api/v1/admin/libraries/" + strconv.FormatInt(s.lib.ID, 10) + "/progress?path=" +
		url.QueryEscape(spNFDBook) + "&user_id=" + strconv.FormatInt(s.sam, 10)
	if resp, body := s.do(t, "PATCH", edit, adminSession(t, s.testEnv), `{"finished":true}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin edit of the side row = %d %s", resp.StatusCode, body)
	}
	side, err := s.cat.GetProgress(ctx, s.sam, catalog.Ref{LibraryID: s.lib.ID, Path: spNFDBook})
	if err != nil || side == nil || !side.Finished {
		t.Fatalf("the side row after the admin's edit = %+v %v, want it finished", side, err)
	}
	if p, err := s.cat.GetProgress(ctx, s.sam, catalog.Ref{LibraryID: s.lib.ID, Path: spNFCBook}); err != nil || p != nil {
		t.Fatalf("the admin's edit of the side row wrote the index spelling's row: %+v %v", p, err)
	}
}
