package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// A book split across disc folders: Health lists it (not as duplicates) until an
// admin sets its folder to one book; then the joined book's chapters point at the
// files in its disc folders, which stream through the book's scope (allowed) and
// not outside it (denied).
func TestJoinedDiscBook(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, memberTok, memberID := adminAndMember(t, e)
	root := t.TempDir()
	audio, err := os.ReadFile(filepath.Join("..", "..", "testdata", "library", "Will Wight", "Cradle", "01 - Unsouled.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"A/Book/CD1/01.m4b", "A/Book/CD2/01.m4b", "Other/01.m4b"} {
		abs := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, audio, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	base := "/api/v1/admin/libraries/" + strconv.FormatInt(lib.ID, 10)
	libPath := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)
	if _, err := e.api.scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}

	// The folder listing marks the folder whose discs a `book` override would join
	// (the console offers it only there), and a joined folder as the book.
	bookEntry := func() (isBook, split bool) {
		t.Helper()
		resp, body := e.do(t, "GET", libPath+"/fs?path=A", adminTok, "")
		var listing struct {
			Entries []struct {
				Path       string `json:"path"`
				IsBook     bool   `json:"is_book"`
				SplitDiscs bool   `json:"split_discs"`
			} `json:"entries"`
		}
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &listing) != nil ||
			len(listing.Entries) != 1 || listing.Entries[0].Path != "A/Book" {
			t.Fatalf("fs A = %d %s", resp.StatusCode, body)
		}
		return listing.Entries[0].IsBook, listing.Entries[0].SplitDiscs
	}
	if isBook, split := bookEntry(); isBook || !split {
		t.Fatalf("A/Book before the join: is_book %v, split_discs %v", isBook, split)
	}

	// Health: the split book is listed by its first disc, its identical discs are
	// not offered as duplicates, and the list is admin-only.
	counts := func() map[string]int {
		t.Helper()
		resp, body := e.do(t, "GET", "/api/v1/admin/issues", adminTok, "")
		var out issuesResp
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("issues = %d %s", resp.StatusCode, body)
		}
		m := map[string]int{}
		for _, c := range out.Categories {
			m[c.Kind] = c.Count
		}
		return m
	}
	if c := counts(); c[catalog.IssueSplitDiscs] != 1 || c[catalog.IssueDuplicate] != 0 {
		t.Fatalf("issues = %v, want one split book and no duplicates", c)
	}
	list := "/api/v1/admin/books?issue=split_discs"
	if resp, _ := e.do(t, "GET", list, memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member split list = %d, want 403", resp.StatusCode)
	}
	resp, body := e.do(t, "GET", list, adminTok, "")
	var page struct {
		Books []struct {
			Path string `json:"path"`
		} `json:"books"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &page) != nil ||
		len(page.Books) != 1 || page.Books[0].Path != "A/Book/CD1" {
		t.Fatalf("split list = %d %s", resp.StatusCode, body)
	}

	// The fix: the folder becomes one book (the scan the override queues runs here).
	if resp, body := e.do(t, "PUT", base+"/folder-override?path="+escape("A/Book"), adminTok, `{"mode":"book"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("set override = %d %s", resp.StatusCode, body)
	}
	if c := counts(); c[catalog.IssueSplitDiscs] != 0 {
		t.Fatalf("issues after the fix = %v", c)
	}
	res, err := e.api.scanner.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	// A reshape, not new books: no "books added" notification follows
	// (notify.ScanFinished announces Counts.Added).
	if res.Added != 0 || len(res.AddedTitles) != 0 {
		t.Fatalf("the join counted as added: %+v %q", res.ScanCounts, res.AddedTitles)
	}
	if isBook, split := bookEntry(); !isBook || split {
		t.Fatalf("A/Book joined: is_book %v, split_discs %v", isBook, split)
	}

	// A member granted the book's folder reads it as one book and streams its discs.
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "The book"})
	if err := e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: "A/Book"}); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, memberID, share.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"A/Book", "A/Book/CD2"} {
		resp, body := e.do(t, "GET", libPath+"/chapters?path="+escape(p), memberTok, "")
		var ch struct {
			Path  string             `json:"path"`
			Files []catalog.BookFile `json:"files"`
		}
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &ch) != nil || ch.Path != "A/Book" {
			t.Fatalf("chapters(%s) = %d %s", p, resp.StatusCode, body)
		}
		var files []string
		for _, f := range ch.Files {
			files = append(files, f.RelPath)
		}
		if !slices.Equal(files, []string{"A/Book/CD1/01.m4b", "A/Book/CD2/01.m4b"}) {
			t.Fatalf("chapters(%s) files = %q", p, files)
		}
	}
	resp, _ = e.do(t, "GET", libPath+"/stream?path="+escape("A/Book/CD2/01.m4b"), memberTok, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/mp4" {
		t.Fatalf("stream a disc file = %d %q, want 200 audio/mp4", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp, _ := e.do(t, "GET", libPath+"/stream?path="+escape("Other/01.m4b"), memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stream outside the share = %d, want 403", resp.StatusCode)
	}

	// Reading the joined book again works on its folder; members can't.
	rescan := base + "/book/rescan?path=" + escape("A/Book")
	if resp, _ := e.do(t, "POST", rescan, memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member rescan = %d, want 403", resp.StatusCode)
	}
	if resp, body := e.do(t, "POST", rescan, adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin rescan = %d %s", resp.StatusCode, body)
	}
}

// split_discs is the console's, not the player's: an admin's folder listing marks
// the folder whose discs a `book` override would join (allowed), while a member's
// listing of the same folder carries no split_discs key at all (denied), so the
// player's wire is what it was before joining existed.
func TestSplitDiscsAdminOnly(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, memberTok, memberID := adminAndMember(t, e)
	root := t.TempDir()
	audio, err := os.ReadFile(filepath.Join("..", "..", "testdata", "library", "Will Wight", "Cradle", "01 - Unsouled.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"A/Book/CD1/01.m4b", "A/Book/CD2/01.m4b"} {
		abs := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, audio, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	if _, err := e.api.scanner.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "A"})
	if err := e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, memberID, share.ID); err != nil {
		t.Fatal(err)
	}
	fs := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/fs?path=A"
	entry := func(tok string) map[string]json.RawMessage {
		t.Helper()
		resp, body := e.do(t, "GET", fs, tok, "")
		var listing struct {
			Entries []map[string]json.RawMessage `json:"entries"`
		}
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &listing) != nil ||
			len(listing.Entries) != 1 || string(listing.Entries[0]["path"]) != `"A/Book"` {
			t.Fatalf("fs A = %d %s", resp.StatusCode, body)
		}
		return listing.Entries[0]
	}
	if got := entry(adminTok)["split_discs"]; string(got) != "true" {
		t.Fatalf("admin listing split_discs = %s, want true", got)
	}
	if got, ok := entry(memberTok)["split_discs"]; ok {
		t.Fatalf("member listing carries split_discs = %s, want no key", got)
	}
}

// A disc folder's path resolves to the joined book above it, so reaching the book
// takes a grant on the book too. A share granted only "A/Book/CD1" (made before the
// join) gets, for the disc folder or a file in it, exactly what a path outside its
// scope gets (denied: 403, the same body, nothing read or indexed on its behalf),
// whether or not the joined book is indexed yet, while it still streams the real
// files it was granted. A grant on "A/Book", or an admin, resolves the disc path to
// the joined book (allowed). A disc path is answered from the index once the
// joined book is in it: shipped clients keep asking for it, and each on-demand
// read would walk and probe every disc again.
func TestJoinedDiscPathScope(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, memberTok, memberID := adminAndMember(t, e)
	root := t.TempDir()
	audio, err := os.ReadFile(filepath.Join("..", "..", "testdata", "library", "Will Wight", "Cradle", "01 - Unsouled.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"A/Book/CD1/01.m4b", "A/Book/CD2/01.m4b"} {
		abs := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, audio, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The joined book's cover is its folder's own image.
	if err := os.WriteFile(filepath.Join(root, "A", "Book", "cover.jpg"), []byte("\xff\xd8\xff\xe0 jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	libPath := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)
	// Joined, but nothing indexed yet: the first look at a disc path reads it.
	if err := e.cat.SetFolderOverride(ctx, lib.ID, "A/Book", catalog.OverrideBook); err != nil {
		t.Fatal(err)
	}
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "Disc one"})
	if err := e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: "A/Book/CD1"}); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, memberID, share.ID); err != nil {
		t.Fatal(err)
	}
	reads := 0
	index := e.api.indexPath
	e.api.indexPath = func(ctx context.Context, lib catalog.Library, rel string, allow func(string) bool) (*catalog.Book, error) {
		reads++
		return index(ctx, lib, rel, allow)
	}
	indexed := func() bool {
		t.Helper()
		_, err := e.cat.GetBookByPath(ctx, lib.ID, "A/Book")
		if err != nil && !errors.Is(err, catalog.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}

	// Denied: the disc folder and a file in it read as "A/Book" itself does, a path
	// outside the share.
	endpoints := []string{"item", "chapters", "cover", "meta"}
	denied := func(stage string) {
		t.Helper()
		for _, ep := range endpoints {
			outResp, outBody := e.do(t, "GET", libPath+"/"+ep+"?path="+escape("A/Book"), memberTok, "")
			if outResp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s: %s(A/Book) outside the share = %d %s, want 403", stage, ep, outResp.StatusCode, outBody)
			}
			for _, p := range []string{"A/Book/CD1", "A/Book/CD1/01.m4b"} {
				resp, body := e.do(t, "GET", libPath+"/"+ep+"?path="+escape(p), memberTok, "")
				if resp.StatusCode != outResp.StatusCode || body != outBody {
					t.Fatalf("%s: %s(%s) granted only the disc = %d %s, want %d %s", stage, ep, p, resp.StatusCode, body, outResp.StatusCode, outBody)
				}
			}
		}
	}
	denied("not indexed")
	if indexed() {
		t.Fatal("a caller granted only the disc indexed the joined book")
	}

	// Allowed: an admin resolves the disc path to the joined book, read once.
	reads = 0
	for _, p := range []string{"A/Book/CD1", "A/Book/CD2", "A/Book/CD2/01.m4b", "A/Book/CD1"} {
		resp, body := e.do(t, "GET", libPath+"/item?path="+escape(p), adminTok, "")
		var b catalog.Book
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &b) != nil || b.RelPath != "A/Book" {
			t.Fatalf("admin item(%s) = %d %s, want the joined book", p, resp.StatusCode, body)
		}
	}
	if reads != 1 || !indexed() {
		t.Fatalf("admin disc lookups read the book %d times (indexed %v), want once", reads, indexed())
	}

	// Indexed now: still denied, answered from the index without a read.
	reads = 0
	denied("indexed")
	if reads != 0 {
		t.Fatalf("denied lookups read the book %d times, want none", reads)
	}

	// The real files the share grants still stream; the other disc's don't.
	if resp, _ := e.do(t, "GET", libPath+"/stream?path="+escape("A/Book/CD1/01.m4b"), memberTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("stream the granted disc's file = %d, want 200", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", libPath+"/stream?path="+escape("A/Book/CD2/01.m4b"), memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stream the other disc's file = %d, want 403", resp.StatusCode)
	}

	// Allowed: granted the book too, the disc path is the joined book, from the index.
	if err := e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: "A/Book"}); err != nil {
		t.Fatal(err)
	}
	for _, ep := range endpoints {
		resp, body := e.do(t, "GET", libPath+"/"+ep+"?path="+escape("A/Book/CD1"), memberTok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s(A/Book/CD1) granted the book = %d %s, want 200", ep, resp.StatusCode, body)
		}
		if ep == "chapters" {
			var ch struct {
				Path string `json:"path"`
			}
			if json.Unmarshal([]byte(body), &ch) != nil || ch.Path != "A/Book" {
				t.Fatalf("chapters(A/Book/CD1) granted the book = %s, want the joined book", body)
			}
		}
	}
	if reads != 0 {
		t.Fatalf("lookups of an indexed joined book read it %d times, want none", reads)
	}
}
