package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// writeTree writes placeholder files (not real audio: the scan without ffprobe
// reads them as books all the same) at rels under root.
func writeTree(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("placeholder "+rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fsEntries GETs /fs?path= as tok and returns its entries as raw JSON objects by
// name, so an absent field reads as absent.
func fsEntries(t *testing.T, e *testEnv, tok string, libID int64, rel string) map[string]map[string]json.RawMessage {
	t.Helper()
	resp, body := e.do(t, "GET", bookURL(libID, "fs", rel), tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fs %q = %d %s", rel, resp.StatusCode, body)
	}
	var listing struct {
		Entries []map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]json.RawMessage{}
	for _, en := range listing.Entries {
		var name string
		_ = json.Unmarshal(en["name"], &name)
		out[name] = en
	}
	return out
}

// TestBrowseFSPendingBook (#193): /fs marks a folder the scanner would read as a
// book that isn't indexed yet pending_book: true, for any caller allowed it, and
// leaves the field out everywhere else (an indexed book, a plain folder, a file).
// A scoped caller who sees a folder only on the way down to their grant gets no
// flag on it (denied), and gets it on the granted folder (allowed).
func TestBrowseFSPendingBook(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, "Author X/Indexed/01.mp3")
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	writeTree(t, root, "Author X/New Book/01.mp3", "Author X/New Book/02.mp3", "Author X/Artwork/cover.jpg",
		"Author X/Loose/a.mp3", "Author X/Shared/Granted/01.mp3", "Author X/Shared/01.mp3")
	if err := e.cat.SetFolderOverride(ctx, lib.ID, "Author X/Loose", catalog.OverrideCollection); err != nil {
		t.Fatal(err)
	}
	admin := adminSession(t, e)

	entries := fsEntries(t, e, admin, lib.ID, "Author X")
	if got := string(entries["New Book"]["pending_book"]); got != "true" {
		t.Fatalf("New Book pending_book = %q, want true (%v)", got, entries["New Book"])
	}
	if _, ok := entries["New Book"]["is_book"]; ok {
		t.Fatal("a pending book also says is_book")
	}
	if string(entries["Indexed"]["is_book"]) != "true" {
		t.Fatalf("Indexed is no book: %v", entries["Indexed"])
	}
	for _, name := range []string{"Indexed", "Artwork", "Loose"} {
		if _, ok := entries[name]["pending_book"]; ok {
			t.Errorf("%s carries pending_book: %v", name, entries[name])
		}
	}
	files := fsEntries(t, e, admin, lib.ID, "Author X/New Book")
	for name, en := range files {
		if _, ok := en["pending_book"]; ok {
			t.Errorf("file %s carries pending_book", name)
		}
	}

	// A player's session sees the same (no capability, no client check).
	uid, tok := newUserWithSession(t, e, "sam", false)
	if err := e.cat.GrantWholeLibrary(ctx, uid, lib.ID); err != nil {
		t.Fatal(err)
	}
	if got := string(fsEntries(t, e, tok, lib.ID, "Author X")["New Book"]["pending_book"]); got != "true" {
		t.Fatalf("a listener's New Book pending_book = %q", got)
	}

	// Scoped: granted only Author X/Shared/Granted.
	kid, kidTok := newUserWithSession(t, e, "kid", false)
	share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Granted",
		Paths: []catalog.PathRule{{LibraryID: lib.ID, Path: "Author X/Shared/Granted"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, kid, share.ID); err != nil {
		t.Fatal(err)
	}
	scoped := fsEntries(t, e, kidTok, lib.ID, "Author X")
	if en, ok := scoped["Shared"]; !ok {
		t.Fatalf("the way down is not listed: %v", scoped)
	} else if _, flagged := en["pending_book"]; flagged {
		t.Fatal("denied: a folder seen only on the way down is a pending book for the scoped caller")
	}
	if _, ok := scoped["New Book"]; ok {
		t.Fatal("an ungranted folder is listed")
	}
	if got := string(fsEntries(t, e, kidTok, lib.ID, "Author X/Shared")["Granted"]["pending_book"]); got != "true" {
		t.Fatalf("allowed: the granted folder's pending_book = %q", got)
	}
	// The admin sees Shared (it has audio of its own) as pending.
	if got := string(entries["Shared"]["pending_book"]); got != "true" {
		t.Fatalf("admin's Shared pending_book = %q", got)
	}
}

// TestBrowseFSPendingBookNotInPodcasts: a podcast library's listing never
// carries pending_book (it is BrowsePodcasts', from the index).
func TestBrowseFSPendingBookNotInPodcasts(t *testing.T) {
	t.Parallel()
	b, _, _ := podcastEnv(t, false)
	writeTree(t, b.lib.Root, "Unindexed Show/episode.mp3", "Example Show/Loose Folder/01.mp3")
	for _, rel := range []string{"", "Example Show"} {
		resp, body := b.doHeaders(t, "GET", bookURL(b.lib.ID, "fs", rel), b.adminTok, "", podcastHeaders())
		if resp.StatusCode != http.StatusOK || strings.Contains(body, "pending_book") {
			t.Fatalf("podcast fs %q = %d %s", rel, resp.StatusCode, body)
		}
	}
}

// progressEnv: a library with a folder book (A/Book: two parts), a book joined
// from its disc folders (Joined/Disc Book), a single-file book at the root and a
// navigation folder (Nav/Series), scanned; then a file no scan has seen yet.
type progressEnv struct {
	*testEnv
	lib *catalog.Library
}

const (
	pBook     = "A/Book"
	pPart1    = "A/Book/01 - one.mp3"
	pPart2    = "A/Book/02 - two.mp3"
	pJoined   = "Joined/Disc Book"
	pDisc2    = "Joined/Disc Book/CD2"
	pSingle   = "Loose.mp3"
	pNav      = "Nav/Series"
	pUnseen   = "Later/New/01.mp3"
	pNavChild = "Nav/Series/Book 2/01.mp3"
)

func newProgressEnv(t *testing.T) *progressEnv {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, pPart1, pPart2, pJoined+"/CD1/01.mp3", pDisc2+"/01.mp3", pSingle, pNavChild)
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.SetFolderOverride(ctx, lib.ID, pJoined, catalog.OverrideBook); err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	writeTree(t, root, pUnseen)
	for _, p := range []string{pBook, pJoined, pSingle, "Nav/Series/Book 2"} {
		if _, err := e.cat.GetBookByPath(ctx, lib.ID, p); err != nil {
			t.Fatalf("fixture book %s: %v", p, err)
		}
	}
	return &progressEnv{testEnv: e, lib: lib}
}

// put PUTs progress at path as tok; it returns the status and the answer's path.
func (e *progressEnv) put(t *testing.T, tok, path string, pos float64) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"position": pos, "duration": 300})
	resp, raw := e.do(t, "PUT", bookURL(e.lib.ID, "progress", path), tok, string(body))
	var out struct {
		Progress catalog.Progress `json:"progress"`
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return resp.StatusCode, out.Progress.Path
}

// get GETs progress at path as tok: the status and the row (nil for none).
func (e *progressEnv) get(t *testing.T, tok, path string) (int, *catalog.Progress) {
	t.Helper()
	resp, raw := e.do(t, "GET", bookURL(e.lib.ID, "progress", path), tok, "")
	var out struct {
		Progress *catalog.Progress `json:"progress"`
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return resp.StatusCode, out.Progress
}

// stored is uid's progress row at exactly path (nil for none).
func (e *progressEnv) stored(t *testing.T, uid int64, path string) *catalog.Progress {
	t.Helper()
	p, err := e.cat.GetProgress(context.Background(), uid, catalog.Ref{LibraryID: e.lib.ID, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProgressPartPathKeysTheBook (#199, security): a part path's progress is the
// book's, read and written under the book's path, a file in a joined book's disc
// folder too; a disc folder itself (written to by a download of that disc, on the
// disc's own timeline), a single-file book, a navigation folder and a path no
// scan has seen keep the path given, as before. The heartbeat's session is the
// book's too.
func TestProgressPartPathKeysTheBook(t *testing.T) {
	t.Parallel()
	e := newProgressEnv(t)
	uid, tok := newUserWithSession(t, e.testEnv, "sam", false)
	if err := e.cat.GrantWholeLibrary(context.Background(), uid, e.lib.ID); err != nil {
		t.Fatal(err)
	}

	// Allowed: PUT on a part lands on the book.
	if status, path := e.put(t, tok, pPart2, 123); status != http.StatusOK || path != pBook {
		t.Fatalf("PUT part = %d path %q, want 200 %q", status, path, pBook)
	}
	if p := e.stored(t, uid, pBook); p == nil || p.Position != 123 {
		t.Fatalf("book row = %+v, want 123", p)
	}
	if p := e.stored(t, uid, pPart2); p != nil {
		t.Fatalf("a row was written at the part path: %+v", p)
	}
	for _, path := range []string{pBook, pPart1, pPart2} {
		if status, p := e.get(t, tok, path); status != http.StatusOK || p == nil || p.Position != 123 || p.Path != pBook {
			t.Errorf("GET %s = %d %+v, want the book's row", path, status, p)
		}
	}
	// The listening session the save started is the book's.
	var sessions, atBook int
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*), COUNT(*) FILTER (WHERE rel_path = ?) FROM listening_sessions WHERE user_id = ?`, pBook, uid).
		Scan(&sessions, &atBook); err != nil {
		t.Fatal(err)
	}
	if sessions == 0 || sessions != atBook {
		t.Fatalf("listening sessions: %d, %d at the book; want all at %s", sessions, atBook, pBook)
	}

	// PATCH on a part edits the book's row.
	resp, raw := e.do(t, "PATCH", bookURL(e.lib.ID, "progress", pPart1), tok, `{"position":42}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(raw, `"path":"`+pBook+`"`) {
		t.Fatalf("PATCH part = %d %s", resp.StatusCode, raw)
	}
	if p := e.stored(t, uid, pBook); p == nil || p.Position != 42 {
		t.Fatalf("after PATCH the book row = %+v, want 42", p)
	}
	if p := e.stored(t, uid, pPart1); p != nil {
		t.Fatalf("PATCH wrote a row at the part path: %+v", p)
	}

	// A file in a disc folder of a joined book is the joined book's.
	if status, got := e.put(t, tok, pDisc2+"/01.mp3", 77); status != http.StatusOK || got != pJoined {
		t.Errorf("PUT a file of a disc = %d path %q, want %q", status, got, pJoined)
	}
	// Unchanged: a disc folder (its position is on the disc's timeline, not the
	// joined book's), a single-file book, a navigation folder, a path no scan has
	// seen.
	for _, path := range []string{pDisc2, pSingle, pNav, pUnseen} {
		if status, got := e.put(t, tok, path, 9); status != http.StatusOK || got != path {
			t.Errorf("PUT %s = %d path %q, want its own path", path, status, got)
		}
		if p := e.stored(t, uid, path); p == nil || p.Position != 9 {
			t.Errorf("%s row = %+v, want 9 at its own path", path, p)
		}
	}
	if p := e.stored(t, uid, pJoined); p == nil || p.Position != 77 {
		t.Errorf("joined book row = %+v, want 77: the disc folder's save is not the book's", p)
	}
}

// TestProgressPathLookupFails (#199): when the index can't say which book a path
// is, progress answers 500 rather than reading or saving under the path given.
func TestProgressPathLookupFails(t *testing.T) {
	t.Parallel()
	e := newProgressEnv(t)
	ctx := context.Background()
	uid, tok := newUserWithSession(t, e.testEnv, "sam", false)
	if err := e.cat.GrantWholeLibrary(ctx, uid, e.lib.ID); err != nil {
		t.Fatal(err)
	}
	// A part path is looked up in book_files: without the table the lookup fails.
	if _, err := e.db.ExecContext(ctx, `ALTER TABLE book_files RENAME TO book_files_gone`); err != nil {
		t.Fatal(err)
	}
	if status, _ := e.put(t, tok, pPart2, 5); status != http.StatusInternalServerError {
		t.Errorf("PUT part with the lookup failing = %d, want 500", status)
	}
	if status, _ := e.get(t, tok, pPart2); status != http.StatusInternalServerError {
		t.Errorf("GET part with the lookup failing = %d, want 500", status)
	}
	if resp, _ := e.do(t, "PATCH", bookURL(e.lib.ID, "progress", pPart2), tok, `{"position":5}`); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("PATCH part with the lookup failing = %d, want 500", resp.StatusCode)
	}
	if p := e.stored(t, uid, pPart2); p != nil {
		t.Errorf("a row was saved at the part path: %+v", p)
	}
}

// TestProgressPartPathDenied (#199, security): a path outside the caller's scope
// is 403 as before; a grant reaching only one part (the book outside it) reads
// and writes that part's own row and never touches the book's; and no caller
// ever reads another listener's row through a part path.
func TestProgressPartPathDenied(t *testing.T) {
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
	// The kid's own row on the book, written while they still had it.
	if _, err := e.cat.SaveProgress(ctx, kid, catalog.Progress{Ref: catalog.Ref{LibraryID: e.lib.ID, Path: pBook}, Position: 999}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{pPart2, pBook, pSingle} {
		if status, _ := e.put(t, kidTok, path, 5); status != http.StatusForbidden {
			t.Errorf("PUT outside scope %s = %d, want 403", path, status)
		}
		if status, _ := e.get(t, kidTok, path); status != http.StatusForbidden {
			t.Errorf("GET outside scope %s = %d, want 403", path, status)
		}
		if resp, _ := e.do(t, "PATCH", bookURL(e.lib.ID, "progress", path), kidTok, `{"position":5}`); resp.StatusCode != http.StatusForbidden {
			t.Errorf("PATCH outside scope %s = %d, want 403", path, resp.StatusCode)
		}
	}
	// The granted part keeps its own key: the book above the grant is untouched.
	if status, path := e.put(t, kidTok, pPart1, 33); status != http.StatusOK || path != pPart1 {
		t.Fatalf("PUT granted part = %d path %q, want its own path", status, path)
	}
	if status, p := e.get(t, kidTok, pPart1); status != http.StatusOK || p == nil || p.Position != 33 || p.Path != pPart1 {
		t.Fatalf("GET granted part = %d %+v, want its own row", status, p)
	}
	if p := e.stored(t, kid, pBook); p == nil || p.Position != 999 {
		t.Fatalf("the book's row moved: %+v, want 999", p)
	}

	// Another listener's row on the book is never read through a part path.
	sam, samTok := newUserWithSession(t, e.testEnv, "sam", false)
	if err := e.cat.GrantWholeLibrary(ctx, sam, e.lib.ID); err != nil {
		t.Fatal(err)
	}
	if status, p := e.get(t, samTok, pPart2); status != http.StatusOK || p != nil {
		t.Fatalf("sam's GET of a part = %d %+v, want no row (the kid's is theirs)", status, p)
	}
	if _, err := e.cat.SaveProgress(ctx, sam, catalog.Progress{Ref: catalog.Ref{LibraryID: e.lib.ID, Path: pBook}, Position: 555}); err != nil {
		t.Fatal(err)
	}
	if status, p := e.get(t, kidTok, pPart1); status != http.StatusOK || p == nil || p.Position != 33 {
		t.Fatalf("the kid's GET after sam's save = %d %+v, want their own 33", status, p)
	}
	if status, p := e.get(t, samTok, pPart1); status != http.StatusOK || p == nil || p.Position != 555 {
		t.Fatalf("sam's GET of a part = %d %+v, want their own 555", status, p)
	}
}
