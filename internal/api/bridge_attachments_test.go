package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

const mistbornPart = mistbornBook + "/01 - The Final Empire.m4b"

// newBridgeEnvCopy is newBridgeEnv over a copy of the fixture library (cradle
// and Mistborn), which a test may change.
func newBridgeEnvCopy(t *testing.T) (*bridgeEnv, string) {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	root := copyFixtures(t, cradlePart, mistbornPart)
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Copy", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	b := &bridgeEnv{testEnv: e, lib: lib}
	b.adminTok, _ = e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "console", 0)
	b.bridgeTok = b.pair(t)
	b.beat(t, bridge.KindVerify)
	return b, root
}

func (b *bridgeEnv) rescan(t *testing.T, body string) (int, string) {
	t.Helper()
	resp, out := b.do(t, "POST", "/api/v1/bridge/rescan", b.bridgeTok, body)
	return resp.StatusCode, out
}

// TestBridgeRescanAttachments: mode attachments reads only the book's
// .audiosilo/ files and answers what is now indexed; a path with no indexed
// book is 404 not_indexable; a bad mode, or from with this mode, is 400.
func TestBridgeRescanAttachments(t *testing.T) {
	t.Parallel()
	b, root := newBridgeEnvCopy(t)
	dir := filepath.Join(root, filepath.FromSlash(mistbornBook), ".audiosilo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "characters.json"), []byte(`{"characters":[{"id":"vin","name":"Vin","reveal":{"chapter":1}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bridge.json"), []byte(`{"generator":"audiosilo-bridge","job_id":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := b.rescan(t, fmt.Sprintf(`{"library_id":%d,"path":%q,"mode":"attachments"}`, b.lib.ID, mistbornBook))
	want := fmt.Sprintf(`{"path":%q,"attachments":{"characters":true,"recaps":false,"bridge":true}}`, mistbornBook) + "\n"
	if code != http.StatusOK || body != want {
		t.Fatalf("attachments rescan = %d %s, want %s", code, body, want)
	}
	book, err := b.cat.GetBookByPath(context.Background(), b.lib.ID, mistbornBook)
	if err != nil {
		t.Fatal(err)
	}
	if a, err := b.cat.BookAttachments(context.Background(), book.ID); err != nil || a == nil || a.Characters == nil || a.Provenance.JobID != 3 {
		t.Errorf("stored = %+v, %v", a, err)
	}
	for _, p := range []string{"Nobody/Here", mistbornBook + "/.audiosilo", "../etc"} {
		if code, body := b.rescan(t, fmt.Sprintf(`{"library_id":%d,"path":%q,"mode":"attachments"}`, b.lib.ID, p)); code != http.StatusNotFound {
			t.Errorf("attachments rescan of %q = %d %s, want 404", p, code, body)
		}
	}
	for _, req := range []string{
		fmt.Sprintf(`{"library_id":%d,"path":%q,"mode":"audio"}`, b.lib.ID, mistbornBook),
		fmt.Sprintf(`{"library_id":%d,"path":%q,"mode":"attachments","from":"Old/Path"}`, b.lib.ID, mistbornBook),
		fmt.Sprintf(`{"library_id":%d,"path":%q,"from":"/"}`, b.lib.ID, mistbornBook),
	} {
		if code, body := b.rescan(t, req); code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", req, code, body)
		}
	}
}

// TestBridgeRescanFrom: a full rescan with from carries a moved book's
// listening state and Bridge jobs to its new path and drops the old row; a from
// whose book is still on disk is ignored.
func TestBridgeRescanFrom(t *testing.T) {
	t.Parallel()
	b, root := newBridgeEnvCopy(t)
	ctx := context.Background()
	job := b.queue(t, mistbornBook, `{}`)
	if _, err := b.cat.SaveProgress(ctx, b.adminID, catalog.Progress{Ref: catalog.Ref{LibraryID: b.lib.ID, Path: mistbornBook}, Position: 77}); err != nil {
		t.Fatal(err)
	}
	const moved = "Brandon Sanderson/Mistborn Era 1"
	if err := os.Rename(filepath.Join(root, filepath.FromSlash(mistbornBook)), filepath.Join(root, filepath.FromSlash(moved))); err != nil {
		t.Fatal(err)
	}
	code, body := b.rescan(t, fmt.Sprintf(`{"library_id":%d,"path":%q,"mode":"full","from":%q}`, b.lib.ID, moved, mistbornBook))
	if code != http.StatusOK || body != fmt.Sprintf(`{"path":%q}`, moved)+"\n" {
		t.Fatalf("rescan from = %d %s", code, body)
	}
	if p, err := b.cat.GetProgress(ctx, b.adminID, catalog.Ref{LibraryID: b.lib.ID, Path: moved}); err != nil || p == nil || p.Position != 77 {
		t.Errorf("progress at the new path = %+v, %v", p, err)
	}
	if got, err := b.api.bridge.Job(ctx, job.ID); err != nil || got.Path == nil || *got.Path != moved {
		t.Errorf("job = %+v, %v; want moved", got, err)
	}
	if _, err := b.cat.GetBookByPath(ctx, b.lib.ID, mistbornBook); err == nil {
		t.Error("the old row stayed")
	}

	// A copy whose source is still there: the source keeps its state.
	cradle := "Will Wight/Cradle"
	if _, err := b.cat.SaveProgress(ctx, b.adminID, catalog.Progress{Ref: catalog.Ref{LibraryID: b.lib.ID, Path: cradle}, Position: 5}); err != nil {
		t.Fatal(err)
	}
	copyFixture(t, root, cradlePart, "Will Wight/Cradle Copy/01 - Unsouled.m4b")
	if code, body := b.rescan(t, fmt.Sprintf(`{"library_id":%d,"path":"Will Wight/Cradle Copy","from":%q}`, b.lib.ID, cradle)); code != http.StatusOK {
		t.Fatalf("rescan of a copy = %d %s", code, body)
	}
	if p, _ := b.cat.GetProgress(ctx, b.adminID, catalog.Ref{LibraryID: b.lib.ID, Path: cradle}); p == nil || p.Position != 5 {
		t.Errorf("the source lost its progress: %+v", p)
	}
	if p, _ := b.cat.GetProgress(ctx, b.adminID, catalog.Ref{LibraryID: b.lib.ID, Path: "Will Wight/Cradle Copy"}); p != nil {
		t.Errorf("the copy took progress: %+v", p)
	}
	if _, err := b.cat.GetBookByPath(ctx, b.lib.ID, cradle); err != nil {
		t.Errorf("the source's row went: %v", err)
	}
}

// TestHiddenPathsAreNotContent is the allowed+denied pair for the hidden-folder
// rule on every path-taking content route: a path through a dot folder (a
// book's .audiosilo/, a .hidden folder) is refused like a missing file, even
// where the file exists and the route would serve it directly (stream); a path
// with a dot inside a name ("Vol. 1") is not hidden.
func TestHiddenPathsAreNotContent(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	root := copyFixtures(t, cradlePart)
	allowed := "Series/Vol. 1/Book.v2.m4b"
	copyFixture(t, root, cradlePart, allowed)
	copyFixture(t, root, cradlePart, "Will Wight/Cradle/.audiosilo/x.m4b")
	copyFixture(t, root, cradlePart, "Will Wight/.hidden/x.mp3")
	if err := os.WriteFile(filepath.Join(root, "Series", "Vol. 1", "cover.jpg"), []byte("\xff\xd8\xff\xe0 not much of a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Copy", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	routes := []struct{ method, endpoint, body string }{
		{"GET", "stream", ""}, {"GET", "cover", ""}, {"GET", "item", ""}, {"GET", "chapters", ""},
		{"GET", "meta", ""}, {"GET", "next", ""}, {"GET", "progress", ""},
		{"PUT", "progress", `{"position":1,"duration":10}`}, {"GET", "bookmarks", ""},
		{"POST", "bookmarks", `{"position":1}`},
	}
	for _, rel := range []string{"Will Wight/Cradle/.audiosilo/x.m4b", "Will Wight/.hidden/x.mp3", "Will Wight/.hidden"} {
		for _, r := range routes {
			// The rule's own 404, not a route's "no book there".
			if resp, body := e.do(t, r.method, bookURL(lib.ID, r.endpoint, rel), tok, r.body); resp.StatusCode != http.StatusNotFound ||
				!strings.Contains(body, `"not found"`) {
				t.Errorf("%s %s %q = %d %s, want the hidden-path 404", r.method, r.endpoint, rel, resp.StatusCode, body)
			}
		}
		if resp, body := e.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/fs?path=%s", lib.ID, url.QueryEscape(rel)), tok, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("fs %q = %d %s, want 404", rel, resp.StatusCode, body)
		}
	}
	// Allowed: the same routes serve a path with dots inside its names.
	for _, r := range routes {
		if resp, body := e.do(t, r.method, bookURL(lib.ID, r.endpoint, allowed), tok, r.body); resp.StatusCode >= 300 {
			t.Errorf("%s %s %q = %d %s, want success", r.method, r.endpoint, allowed, resp.StatusCode, body)
		}
	}
	if resp, body := e.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/fs?path=%s", lib.ID, url.QueryEscape("Series/Vol. 1")), tok, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("fs of a dotted folder = %d %s", resp.StatusCode, body)
	}
}
