package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Path case (server#210). Share rules match paths byte for byte, case
// included: a rule spelled in another case than the disk's names nothing
// there, as a rule naming a missing folder does. A disk that ignores case
// (macOS's, a Windows or SMB share) still opens "émile zola/..." as "Émile
// Zola/...", so every route answers a path the disk doesn't spell as given as
// a path with nothing there: the index's routes find no book at it, and the
// stream and /fs, which open the path itself, look at the listings
// (asGiven). All NFC: case, not the Unicode spelling (pathspelling_test).
const (
	pcAuthor      = "Émile Zola"
	pcBook        = pcAuthor + "/Germinal"
	pcTrack       = pcBook + "/01.mp3"
	pcLowAuthor   = "émile zola" // hal's rule: the author folder in lower case
	pcLowBook     = pcLowAuthor + "/Germinal"
	pcLowTrack    = pcLowBook + "/01.mp3"
	pcLowMissing  = pcLowAuthor + "/Nana"
	pcLowMissingT = pcLowMissing + "/01.mp3"
	pcBookVariant = pcAuthor + "/germinal" // inside ida's rule, the book in lower case
	pcBookVarT    = pcBookVariant + "/01.mp3"
	pcMissing     = pcAuthor + "/Nana"
	pcMissingT    = pcMissing + "/01.mp3"
	pcUnindexed   = pcAuthor + "/Therese" // on disk, put there after the scan
	pcUnindexedT  = pcUnindexed + "/01.mp3"
)

// caseEnv: a library with one book under an accented author folder, scanned,
// then a folder no scan has reached added beside it. sam reaches the whole
// library; hal a share whose rule is the author folder in lower case; ida one
// whose rule is the author folder as the disk spells it.
type caseEnv struct {
	*testEnv
	lib                    *catalog.Library
	samTok, halTok, idaTok string
	// folds is whether the temp dir ignores case itself; when it doesn't, the
	// env stands in for a disk that does (foldCase).
	folds bool
	// read is how the API's listings (a.listed) read a folder (listWith).
	read func(ctx context.Context, dir string, timeout time.Duration) ([]string, error)
}

// listWith makes the API read its listings with read, in a cache of its own,
// empty.
func (c *caseEnv) listWith(read func(ctx context.Context, dir string, timeout time.Duration) ([]string, error)) {
	c.read = read
	c.api.listed = metadata.NewDirNames(read)
}

func newCaseEnv(t *testing.T) *caseEnv {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, pcTrack, pcBook+"/02.mp3", "Other/Book/01.mp3")
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	if _, err := e.cat.GetBookByPath(ctx, lib.ID, pcBook); err != nil {
		t.Fatalf("fixture book: %v", err)
	}
	writeTree(t, root, pcUnindexedT)
	c := &caseEnv{testEnv: e, lib: lib, folds: ignoresCase(t, root)}
	c.listWith(metadata.ReadDirNamesBounded)
	sam, samTok := newUserWithSession(t, e, "sam", false)
	if err := e.cat.GrantWholeLibrary(ctx, sam, lib.ID); err != nil {
		t.Fatal(err)
	}
	c.samTok = samTok
	for name, rule := range map[string]string{"hal": pcLowAuthor, "ida": pcAuthor} {
		user, tok := newUserWithSession(t, e, name, false)
		share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Only " + name,
			Paths: []catalog.PathRule{{LibraryID: lib.ID, Path: rule}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.cat.GrantShare(ctx, user, share.ID); err != nil {
			t.Fatal(err)
		}
		if name == "hal" {
			c.halTok = tok
		} else {
			c.idaTok = tok
		}
	}
	return c
}

// ignoresCase reports whether dir's filesystem finds a name in another case.
func ignoresCase(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Stat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil
}

// foldCase makes the library's disk answer to each of variants (a path in
// another case than the disk's) as one that ignores case would, when the temp
// dir doesn't already: a link at the variant to the folder the disk has (the
// open finds it), kept out of the listings the API reads (listWith), which on
// such a disk name only the stored spelling. Links made after the scan, so
// none is indexed.
func (c *caseEnv) foldCase(t *testing.T, variants map[string]string) {
	t.Helper()
	if c.folds {
		return
	}
	hidden := map[string]bool{}
	for variant, target := range variants {
		at := filepath.Join(c.lib.Root, filepath.FromSlash(variant))
		if err := os.Symlink(filepath.Base(filepath.FromSlash(target)), at); err != nil {
			t.Fatal(err)
		}
		hidden[at] = true
	}
	c.listWith(func(ctx context.Context, dir string, timeout time.Duration) ([]string, error) {
		names, err := metadata.ReadDirNamesBounded(ctx, dir, timeout)
		return slices.DeleteFunc(names, func(name string) bool { return hidden[filepath.Join(dir, name)] }), err
	})
}

// same asserts tok's request at path answers exactly as the one at missing, a
// path with nothing there: status, type and body (each path in the body read
// as <P>, every number as N: ids and times differ).
func (c *caseEnv) same(t *testing.T, tok, method string, at func(string) (string, string), path, missing string) {
	t.Helper()
	u, body := at(path)
	got, gotBody := c.do(t, method, u, tok, body)
	u, body = at(missing)
	want, wantBody := c.do(t, method, u, tok, body)
	norm := func(s, p string) string {
		return numbers.ReplaceAllString(strings.ReplaceAll(s, p, "<P>"), "N")
	}
	if got.StatusCode != want.StatusCode || norm(gotBody, path) != norm(wantBody, missing) ||
		got.Header.Get("Content-Type") != want.Header.Get("Content-Type") {
		t.Errorf("%s %+q = %d %.120q (%s), want %d %.120q (%s) as for a path with nothing there", method, path,
			got.StatusCode, gotBody, got.Header.Get("Content-Type"), want.StatusCode, wantBody, want.Header.Get("Content-Type"))
	}
}

var numbers = regexp.MustCompile(`[0-9]+`)

// get is a GET of endpoint at the path.
func (c *caseEnv) get(endpoint string, extra ...string) func(string) (string, string) {
	return func(p string) (string, string) { return bookURL(c.lib.ID, endpoint, p) + strings.Join(extra, ""), "" }
}

// with is endpoint at the path with body.
func (c *caseEnv) with(endpoint, body string) func(string) (string, string) {
	return func(p string) (string, string) { return bookURL(c.lib.ID, endpoint, p), body }
}

// streamForms are the stream's forms that open the file: the plain stream, a
// download, a transcode, the stored bytes of a remux, a watch rendition.
var streamForms = []string{"", "&download=1", "&transcode=1", "&original=1", "&rendition=watch"}

// TestCaseVariantDiskRoutes (security, every disk): the stream in each form
// and /fs, the routes that open the path given, answer a case variant of a
// folder the disk has as a path with nothing there, for a rule spelled in that
// case (hal) and for a variant inside a rule that names the folder (ida). On a
// temp dir that tells case apart, foldCase stands in for one that doesn't.
// Allowed: the paths as the disk spells them, indexed or not, stream and list.
func TestCaseVariantDiskRoutes(t *testing.T) {
	t.Parallel()
	c := newCaseEnv(t)
	c.foldCase(t, map[string]string{pcLowAuthor: pcAuthor, pcBookVariant: pcBook})
	// The variant opens the book's file: what the denied checks below refuse.
	if _, err := os.Stat(filepath.Join(c.lib.Root, filepath.FromSlash(pcLowTrack))); err != nil {
		t.Fatalf("the disk does not answer to the variant: %v", err)
	}
	for _, form := range streamForms {
		c.same(t, c.halTok, "GET", c.get("stream", form), pcLowTrack, pcLowMissingT)
		c.same(t, c.idaTok, "GET", c.get("stream", form), pcBookVarT, pcMissingT)
	}
	for _, p := range []string{pcLowAuthor, pcLowBook} {
		c.same(t, c.halTok, "GET", c.get("fs"), p, pcLowMissing)
	}
	c.same(t, c.idaTok, "GET", c.get("fs"), pcBookVariant, pcMissing)

	// Allowed: as the disk spells them, an indexed track and one no scan has
	// reached, and their folders.
	for _, tok := range []string{c.samTok, c.idaTok} {
		for _, p := range []string{pcTrack, pcUnindexedT} {
			for _, form := range []string{"", "&download=1"} {
				if resp, body := c.do(t, "GET", bookURL(c.lib.ID, "stream", p)+form, tok, ""); resp.StatusCode != http.StatusOK ||
					!strings.HasPrefix(body, "placeholder ") {
					t.Errorf("stream%s %+q = %d %.80q, want the file", form, p, resp.StatusCode, body)
				}
			}
		}
		for _, p := range []string{pcAuthor, pcBook, pcUnindexed} {
			if resp, body := c.do(t, "GET", bookURL(c.lib.ID, "fs", p), tok, ""); resp.StatusCode != http.StatusOK {
				t.Errorf("fs %+q = %d %s, want the folder", p, resp.StatusCode, body)
			}
		}
	}
}

// TestCaseVariantDiskLookFails: a listing given up on is a 503
// share_not_answering, never the file at the path given.
func TestCaseVariantDiskLookFails(t *testing.T) {
	t.Parallel()
	c := newCaseEnv(t)
	c.listWith(func(context.Context, string, time.Duration) ([]string, error) {
		return nil, metadata.ErrNotAnswered
	})
	for _, path := range []string{bookURL(c.lib.ID, "stream", pcUnindexedT), bookURL(c.lib.ID, "fs", pcUnindexed)} {
		if resp, body := c.do(t, "GET", path, c.samTok, ""); resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d %s, want 503", path, resp.StatusCode, body)
		}
	}
	// An indexed path needs no listing.
	if resp, body := c.do(t, "GET", bookURL(c.lib.ID, "stream", pcTrack), c.samTok, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("stream of an indexed track = %d %s, want 200", resp.StatusCode, body)
	}
}

// TestCaseVariantCostsWhatMissingDoes (security): on the stream and /fs, a
// case variant of a folder the disk has and a path with nothing there take the
// same steps, the same listings in the same order (asGiven, each name looked
// for in its folder's listing before any lstat a variant would answer and a
// missing name would not: TestSafeJoinListedVariantCostsWhatMissingDoes counts
// the lstats too), so how long the check takes never
// tells a rule spelled in another case (hal) or a variant inside a rule (ida)
// that the folder is there. A path the index holds reads no listing, and a
// folder is read once while the listings keep it: a client asking for missing
// paths in a loop reads the root once.
func TestCaseVariantCostsWhatMissingDoes(t *testing.T) {
	t.Parallel()
	c := newCaseEnv(t)
	c.foldCase(t, map[string]string{pcLowAuthor: pcAuthor, pcBookVariant: pcBook})
	inner := c.read
	var (
		mu     sync.Mutex
		listed []string
	)
	root, err := filepath.EvalSymlinks(c.lib.Root) // the walk lists below the resolved root
	if err != nil {
		t.Fatal(err)
	}
	read := func(ctx context.Context, dir string, timeout time.Duration) ([]string, error) {
		mu.Lock()
		listed = append(listed, strings.TrimPrefix(dir, root))
		mu.Unlock()
		return inner(ctx, dir, timeout)
	}
	reset := func() { // a fresh cache, so each request reads its own listings
		c.listWith(read)
		mu.Lock()
		listed = nil
		mu.Unlock()
	}
	steps := func(tok, u string, want int) []string {
		t.Helper()
		reset()
		if resp, body := c.do(t, "GET", u, tok, ""); resp.StatusCode != want {
			t.Errorf("GET %s = %d %s, want %d", u, resp.StatusCode, body, want)
		}
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(listed)
	}
	for _, k := range []struct {
		tok, endpoint, variant, missing string
	}{
		{c.halTok, "stream", pcLowTrack, pcLowMissingT},
		{c.halTok, "fs", pcLowBook, pcLowMissing},
		{c.halTok, "fs", pcLowAuthor, pcLowMissing},
		{c.idaTok, "stream", pcBookVarT, pcMissingT},
		{c.idaTok, "fs", pcBookVariant, pcMissing},
	} {
		v := steps(k.tok, bookURL(c.lib.ID, k.endpoint, k.variant), http.StatusNotFound)
		m := steps(k.tok, bookURL(c.lib.ID, k.endpoint, k.missing), http.StatusNotFound)
		if len(v) == 0 || !slices.Equal(v, m) {
			t.Errorf("%s: the variant %+q listed %q, the missing %+q %q; want the same listings", k.endpoint, k.variant, v, k.missing, m)
		}
	}
	if got := steps(c.samTok, bookURL(c.lib.ID, "stream", pcTrack), http.StatusOK); len(got) != 0 {
		t.Errorf("an indexed track listed %q, want no listing", got)
	}
	// Allowed: a folder no scan has reached is listed as the disk spells it.
	if got := steps(c.samTok, bookURL(c.lib.ID, "fs", pcUnindexed), http.StatusOK); len(got) == 0 {
		t.Error("an unindexed folder was answered with no listing")
	}
	reset()
	at := time.Now() // the listings' clock held, so a slow run never outlives the TTL
	c.api.listed.SetClockForTests(func() time.Time { return at })
	for range 20 {
		for _, p := range []string{pcMissingT, pcLowMissingT} {
			c.do(t, "GET", bookURL(c.lib.ID, "stream", p), c.samTok, "")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	counts := map[string]int{}
	for _, d := range listed {
		counts[d]++
	}
	if counts[""] != 1 || len(listed) != 2 {
		t.Errorf("40 requests for missing paths listed %q, want the root and the author folder once each", listed)
	}
}

// TestFSOutsideScopeIsMissing (security): /fs answers a folder outside the
// caller's share rules as a missing one, with no listing read: hal, whose rule
// is the author folder in lower case, can't tell from a 200 that the folder as
// the disk spells it, its book or another author's folder is there. Allowed:
// the library's root (their entries only) and a folder in their rule (ida).
func TestFSOutsideScopeIsMissing(t *testing.T) {
	t.Parallel()
	c := newCaseEnv(t)
	inner := c.read
	var reads atomic.Int32
	c.listWith(func(ctx context.Context, dir string, timeout time.Duration) ([]string, error) {
		reads.Add(1)
		return inner(ctx, dir, timeout)
	})
	for _, p := range []string{pcAuthor, pcBook, "Other", "Other/Book", "Nowhere"} {
		if resp, body := c.do(t, "GET", bookURL(c.lib.ID, "fs", p), c.halTok, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("hal's fs %+q = %d %s, want 404", p, resp.StatusCode, body)
		}
	}
	if n := reads.Load(); n != 0 {
		t.Errorf("folders outside the rule read %d listings, want none", n)
	}
	for _, p := range []string{pcBook, "Other/Book"} { // as a missing folder in the rule
		c.same(t, c.halTok, "GET", c.get("fs"), p, pcLowMissing)
	}
	if resp, body := c.do(t, "GET", bookURL(c.lib.ID, "fs", ""), c.halTok, ""); resp.StatusCode != http.StatusOK ||
		strings.Contains(body, "Other") {
		t.Errorf("hal's root = %d %s, want 200 without the folder outside the rule", resp.StatusCode, body)
	}
	if resp, body := c.do(t, "GET", bookURL(c.lib.ID, "fs", pcBook), c.idaTok, ""); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, "01.mp3") {
		t.Errorf("ida's book folder = %d %s, want its listing", resp.StatusCode, body)
	}
}

// TestFSLinkOutOfRootIsMissing (security): /fs through a link inside the
// library that points out of it answers as a missing folder, whatever the
// folder it points to holds, and lists nothing there; the stream answers a
// file through it the same whether the file is there or not. Allowed: a link
// to a folder inside the library is listed and streamed through.
func TestFSLinkOutOfRootIsMissing(t *testing.T) {
	t.Parallel()
	c := newCaseEnv(t)
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, outside, "present/01.mp3")
	for link, to := range map[string]string{"Out": outside, "In": filepath.Join(c.lib.Root, pcAuthor)} {
		if err := os.Symlink(to, filepath.Join(c.lib.Root, link)); err != nil {
			t.Skipf("symlinks unsupported on this platform: %v", err)
		}
	}
	inner := c.read
	var (
		mu   sync.Mutex
		dirs []string
	)
	c.listWith(func(ctx context.Context, dir string, timeout time.Duration) ([]string, error) {
		mu.Lock()
		dirs = append(dirs, dir)
		mu.Unlock()
		return inner(ctx, dir, timeout)
	})
	for _, p := range []string{"Out", "Out/present", "Out/absent"} {
		c.same(t, c.samTok, "GET", c.get("fs"), p, "Nowhere")
	}
	c.same(t, c.samTok, "GET", c.get("stream"), "Out/present/01.mp3", "Out/absent/01.mp3")
	mu.Lock()
	for _, d := range dirs {
		if strings.HasPrefix(d, outside) {
			t.Errorf("listed %q, a folder out of the library", d)
		}
	}
	mu.Unlock()

	if resp, body := c.do(t, "GET", bookURL(c.lib.ID, "fs", "In"), c.samTok, ""); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, "Germinal") {
		t.Errorf("fs through a link inside the library = %d %s, want its listing", resp.StatusCode, body)
	}
	if resp, body := c.do(t, "GET", bookURL(c.lib.ID, "stream", "In/Germinal/01.mp3"), c.samTok, ""); resp.StatusCode != http.StatusOK ||
		!strings.HasPrefix(body, "placeholder ") {
		t.Errorf("stream through a link inside the library = %d %.80q, want the file", resp.StatusCode, body)
	}
}

// TestCaseVariantRoutes (security, a disk that ignores case only): every
// path-addressed route a listener reaches answers hal, whose rule is the
// author folder in lower case, at the book in that case exactly as at a path
// with nothing there: the content routes, the stream's forms, /fs, the
// listener's own state, the queue, a cast token and a sync event. Allowed: ida,
// whose rule spells the folder as the disk does, reaches the book.
func TestCaseVariantRoutes(t *testing.T) {
	t.Parallel()
	c := newCaseEnv(t)
	if !c.folds {
		t.Skip("the temp dir tells case apart; TestCaseVariantDiskRoutes covers the routes that open the path on a disk that doesn't")
	}
	lib := strconv.FormatInt(c.lib.ID, 10)
	jsonBody := func(p string) string {
		b, _ := json.Marshal(map[string]any{"library_id": c.lib.ID, "path": p})
		return string(b)
	}
	events := 0
	routes := []struct {
		method string
		at     func(string) (string, string)
		book   bool // at the book (else at its first track)
	}{
		{"GET", c.get("item"), true},
		{"GET", c.get("chapters"), true},
		{"GET", c.get("next"), true},
		{"GET", c.get("meta"), true},
		{"GET", c.get("cover"), true},
		{"GET", c.get("renditions", "&kind=watch"), true},
		{"POST", c.with("renditions", `{"kind":"watch"}`), true},
		{"GET", c.get("ebook"), true},
		{"GET", c.get("readalong"), true},
		{"GET", c.get("reading-progress"), true},
		{"PUT", c.with("reading-progress", `{"progression":0.5,"cfi":"epubcfi(/6/2)"}`), true},
		{"GET", c.get("progress"), true},
		{"PUT", c.with("progress", `{"position":7,"duration":90}`), true},
		{"PATCH", c.with("progress", `{"position":8}`), true},
		{"GET", c.get("bookmarks"), true},
		{"POST", c.with("bookmarks", `{"position":9,"note":"x"}`), true},
		{"GET", c.get("notes"), true},
		{"POST", c.with("notes", `{"position":9,"body":"x"}`), true},
		{"GET", c.get("history"), true},
		{"POST", c.with("history", `{"from_pos":1,"to_pos":5}`), true},
		{"POST", c.get("favourites"), true},
		{"DELETE", c.get("favourites"), true},
		{"GET", c.get("rating"), true},
		{"PUT", c.with("rating", `{"rating":4}`), true},
		{"DELETE", c.get("rating"), true},
		{"GET", c.get("fs"), true},
		{"POST", func(p string) (string, string) { return "/api/v1/me/queue", jsonBody(p) }, true},
		{"DELETE", func(p string) (string, string) {
			return "/api/v1/me/queue?library_id=" + lib + "&path=" + url.QueryEscape(p), ""
		}, true},
		{"POST", func(p string) (string, string) { return "/api/v1/cast/token", jsonBody(p) }, true},
		{"POST", func(p string) (string, string) {
			events++ // an event id of its own each time, else the second is a duplicate
			return syncURL, syncBody("dev-1", listenJSON("e"+strconv.Itoa(events), c.lib.ID, p, 20, 80, ""))
		}, true},
	}
	for _, form := range streamForms {
		routes = append(routes, struct {
			method string
			at     func(string) (string, string)
			book   bool
		}{"GET", c.get("stream", form), false})
	}
	for _, r := range routes {
		path, missing := pcLowTrack, pcLowMissingT
		if r.book {
			path, missing = pcLowBook, pcLowMissing
		}
		c.same(t, c.halTok, r.method, r.at, path, missing)
	}
	// hal stored nothing at the book as the disk spells it.
	var rows int
	if err := c.db.QueryRowContext(context.Background(), `SELECT
		(SELECT COUNT(*) FROM progress WHERE substr(rel_path, 1, length(?1)) = ?1) +
		(SELECT COUNT(*) FROM bookmarks WHERE substr(rel_path, 1, length(?1)) = ?1) +
		(SELECT COUNT(*) FROM up_next WHERE substr(rel_path, 1, length(?1)) = ?1) +
		(SELECT COUNT(*) FROM cast_grants WHERE substr(rel_path, 1, length(?1)) = ?1)`,
		pcBook).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows at the book as the disk spells it: %d %v", rows, err)
	}
	// Allowed: ida's rule spells the folder as the disk does.
	for _, endpoint := range []string{"item", "chapters", "fs"} {
		if resp, body := c.do(t, "GET", bookURL(c.lib.ID, endpoint, pcBook), c.idaTok, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("ida %s = %d %s, want 200", endpoint, resp.StatusCode, body)
		}
	}
	for _, form := range []string{"", "&download=1"} {
		if resp, _ := c.do(t, "GET", bookURL(c.lib.ID, "stream", pcTrack)+form, c.idaTok, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("ida stream%s = %d, want 200", form, resp.StatusCode)
		}
	}
}
