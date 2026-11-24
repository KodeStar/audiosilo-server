package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/meta"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/oidc/oidctest"
)

// The variant goldens (audiosilo#96): an envelope a client meets in more than
// one form (an item with and without its optional blocks, a non-member's /me,
// an unmatched /meta, an error), one golden per form, each from an env of its
// own unless it can read the main env without changing another case's shape.
// The coverage guard (wire_coverage_test.go) names what they still miss.

// wireVariantCases are the variant cases; m is the main env (wireCases).
func wireVariantCases(t *testing.T, m *wireMainEnv) []wireCase {
	t.Helper()
	v := newWireSignInEnv(t)
	x := newWireExtrasEnv(t)
	f := newWireFoldersEnv(t)
	c := newWireCopiesEnv(t)
	p := newWireEpisodeEnv(t)
	s := newWireDivergedEnv(t)
	u := newWireUnmatchedEnv(t)
	b := newWireBundleEnv(t)
	libRoute := "/api/v1/libraries/" + strconv.FormatInt(m.lib.ID, 10)
	return []wireCase{
		{name: "me-owner", route: "GET /api/v1/me (an account of its own, no household member)",
			body: wireCall(m.testEnv, "GET", "/api/v1/me", m.samTok, ""),
			// The member fields are null here by design: the me golden guards them.
			empty: []string{"$.managed_by", "$.member_kind"},
			types: wireAt[meUser]("$")},
		{name: "libraries-listener", route: "GET /api/v1/libraries (a listener: no root)",
			body:  wireCall(m.testEnv, "GET", "/api/v1/libraries", m.samTok, ""),
			keys:  []string{"handleListLibraries"},
			types: wireAt[listenerLibrary]("$.libraries[]")},
		{name: "books-page", route: "GET /api/v1/libraries/{id}/books?limit=1 (a page with more after it)",
			body:  wireCall(m.testEnv, "GET", libRoute+"/books?limit=1", m.samTok, ""),
			types: wireAt[catalog.Page]("$")},
		{name: "item-readalong-stale", route: "GET /api/v1/libraries/{id}/item?path=<a book whose read-along map is stale>",
			body:  wireCall(m.testEnv, "GET", bookURL(m.lib.ID, "item", mistbornBook), m.samTok, ""),
			types: wireBookTypes},
		{name: "error", route: "GET /api/v1/libraries/{id}/item?path=<no book> (the error envelope)",
			body: func(t *testing.T) string {
				return wireStatus(t, m.testEnv, "GET", bookURL(m.lib.ID, "item", "No Such/Book"), m.samTok, "", http.StatusNotFound)
			},
			keys: []string{"writeError"}},
		{name: "error-code", route: "POST /api/v1/auth/password (a wrong current password: the error envelope with a code)",
			body: func(t *testing.T) string {
				return wireStatus(t, m.testEnv, "POST", "/api/v1/auth/password", m.samTok,
					`{"password":"new-password","current_password":"not-it"}`, http.StatusUnauthorized)
			},
			keys: []string{"writeErrorCode"}},

		{name: "server-bridge", route: "GET /api/v1/server (signed in, single sign-on on, a Bridge online)",
			body: func(t *testing.T) string {
				// Online only within bridge.OnlineWindow of a heartbeat: beat now,
				// not when the env was built before every case ran.
				v.beat(t, bridge.KindVerify)
				return wireOK(t, v.testEnv, "GET", "/api/v1/server", v.userTok, "")
			},
			keys:  wireServerKeys,
			types: wireAt[bridgeInfo]("$.bridge")},
		{name: "auth-redeem-invite", route: "POST /api/v1/auth/redeem (a listener's invite with an expiry and uses left, home and away addresses)",
			body: func(t *testing.T) string {
				return wireOK(t, v.testEnv, "POST", "/api/v1/auth/redeem", "", `{"code":"`+v.invite(t)+`"}`)
			},
			types: wireAt[PairingPayload]("$")},
		{name: "auth-exchange-invite", route: "POST /api/v1/auth/exchange (a listener's invite, home and away addresses)",
			body: func(t *testing.T) string {
				status, pairing, _ := v.redeemCode(t, v.invite(t))
				if status != http.StatusOK {
					t.Fatalf("redeem = %d", status)
				}
				status, b := v.exchangeToken(t, pairing, "Wire tablet")
				if status != http.StatusOK {
					t.Fatalf("exchange = %d %s", status, b)
				}
				return b
			},
			empty: []string{"$.user.managed_by", "$.user.member_kind"},
			keys:  []string{"sessionEnvelope"},
			types: wireSessionTypes},

		{name: "item-recordings", route: "GET /api/v1/libraries/{id}/item?path=<a matched book with other recordings, edited chapters and a cover colour>",
			body:  wireCall(x.testEnv, "GET", x.url("item", "Saga/2"), x.adminTok, ""),
			types: wireBookTypes},
		{name: "chapters-edited", route: "GET /api/v1/libraries/{id}/chapters?path=<a book with edited chapters>",
			body:  wireCall(x.testEnv, "GET", x.url("chapters", "Saga/2"), x.adminTok, ""),
			keys:  []string{"handleChapters"},
			types: wireChaptersTypes},

		{name: "fs-pending", route: "GET /api/v1/libraries/{id}/fs?path=Author X&offset=1&limit=2 (a folder override, a book not indexed yet, more after the page)",
			body:  wireCall(f.testEnv, "GET", bookURL(f.lib.ID, "fs", "Author X")+"&offset=1&limit=2", f.samTok, ""),
			types: wireAt[library.Listing]("$")},
		{name: "search-copies", route: "GET /api/v1/search?q=hobbit (a book in two libraries: one row, the other copy under other_locations)",
			body:  wireCall(c.env, "GET", c.url, c.tok, ""),
			keys:  []string{"handleSearch"},
			types: wireAt[catalog.Book]("$.books[]")},
		{name: "item-episode", route: "GET /api/v1/libraries/{id}/item?path=<a podcast episode> (X-AudioSilo-Client: a current player)",
			body: func(t *testing.T) string {
				resp, b := p.doHeaders(t, "GET", bookURL(p.lib.ID, "item", p.episode), p.userTok, "", podcastHeaders())
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("episode item = %d %s", resp.StatusCode, b)
				}
				return b
			},
			// The fixture episode's fetch failed: no request is open and no bytes
			// are counted. A running fetch's numbers are pinned by the stream's
			// podcast_fetching golden (TestPodcastStreamWireGolden).
			empty: []string{"$.episode.fetch.bytes_total", "$.episode.fetch.request_id"},
			types: wireBookTypes},
		{name: "listening-sync-diverged", route: "POST /api/v1/me/listening/sync (a span from behind another device's place, and one refused)",
			body:  s.watchSync,
			types: wireAt[catalog.SyncAnswer]("$")},
		{name: "me-progress-diverged", route: "GET /api/v1/me/progress (a place another device diverged from, a finished book)",
			body: func(t *testing.T) string {
				s.watchSync(t) // the divergence, when the case before did not run (-run)
				return wireOK(t, s.testEnv, "GET", "/api/v1/me/progress", s.phoneTok, "")
			},
			keys:  []string{"handleListProgress"},
			types: wireAt[catalog.Progress]("$.progress[]")},
		{name: "meta-unmatched", route: "GET /api/v1/libraries/{id}/meta?path=<a book with no community match and generated content>",
			body: wireCall(u.env, "GET", u.url, u.tok, ""),
			// A generated work has no authors of its own (a title alone): meta guards them.
			empty: []string{"$.work.authors"},
			types: wireAt[meta.Unmatched]("$")},
		{name: "meta-bundle", route: "GET /api/v1/libraries/{id}/meta?path=<a matched book two of a series>&include=previous",
			body:  wireCall(b.env, "GET", b.url+"&include=previous", b.tok, ""),
			types: wireAt[meta.Enrichment]("$")},
	}
}

// wireStatus sends a request and returns its body, failing unless it answers
// want.
func wireStatus(t *testing.T, e *testEnv, method, path, tok, body string, want int) string {
	t.Helper()
	resp, b := e.do(t, method, path, tok, body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d %s, want %d", method, path, resp.StatusCode, b, want)
	}
	return b
}

// wireSignInEnv: single sign-on on, a home (lan_url) and an away (public_url)
// address, a Bridge online, and a listener (lee) whose invite limits its uses
// and expires.
type wireSignInEnv struct {
	*bridgeEnv
	lee int64
}

func newWireSignInEnv(t *testing.T) *wireSignInEnv {
	t.Helper()
	idp := oidctest.New(t, "audiosilo", oidcSecret, oidcAlice)
	e := newTestEnvWith(t, func(c *config.Config) {
		c.PublicURL = oidcPublic
		c.LANURL = "http://192.168.1.20:8080"
		c.Auth.OIDC = config.OIDCConfig{Enabled: true, Name: "Keycloak", Issuer: idp.Issuer, ClientID: "audiosilo", ClientSecret: oidcSecret}
	})
	e.api.ffmpeg = "ffmpeg" // renditions on, as in the server golden
	lee, _ := newUserWithSession(t, e, "lee", false)
	return &wireSignInEnv{bridgeEnv: newBridgeEnvOn(t, e), lee: lee}
}

// invite mints lee a fresh invite (three uses, a day) and returns its code.
func (v *wireSignInEnv) invite(t *testing.T) string {
	t.Helper()
	inv, err := v.auth.CreateInvite(context.Background(), v.lee, "Wire", 3, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return inv.Code
}

// newWireExtrasEnv: recordingsEnv's Saga library (Saga/2 matched to work "two",
// two other recordings of it), Saga/2 given two files, its chapters edited in
// the console and its cover's colour read.
func newWireExtrasEnv(t *testing.T) *sagaEnv {
	t.Helper()
	const p = "Saga/2"
	files := []catalog.BookFile{{RelPath: p + "/01.mp3", Seq: 0, Duration: 1800, Size: 10},
		{RelPath: p + "/02.mp3", Seq: 1, Duration: 1800, Size: 20}}
	chapters := []metadata.Chapter{{Index: 0, Title: "01", FilePath: p + "/01.mp3", End: 1800},
		{Index: 1, Title: "02", FileIndex: 1, FilePath: p + "/02.mp3", End: 1800, BookOffset: 1800}}
	e := recordingsEnv(t, true)
	ctx := context.Background()
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: e.lib.ID, RelPath: p, IsFolder: true, Title: p,
		Series: "Saga", SeriesIndex: 2, ASIN: "B0TWO", Narrator: "Bray", Format: "mp3", Duration: 3600,
		AddedAt: "2024-01-01T00:00:00Z", Files: files, Chapters: chapters}); err != nil {
		t.Fatal(err)
	}
	edit := `{"basis":` + twoFileBasis + `,"chapters":[{"start":0,"title":"Prologue"},{"start":900,"title":"One"},{"start":1800,"title":"Two"}]}`
	wireOK(t, e.testEnv, "PUT", "/api/v1/admin/libraries/"+strconv.FormatInt(e.lib.ID, 10)+"/book/chapters?path="+escape(p),
		e.adminTok, edit)
	// The colour is recorded on the art identity it was read under (the
	// background pass's due list holds it).
	due, _, err := e.cat.CoverColorsDue(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(due, func(d catalog.CoverColorDue) bool { return d.Path == p })
	if i < 0 {
		t.Fatalf("%s is not due a colour: %+v", p, due)
	}
	if err := e.cat.RecordCoverColors(ctx, []catalog.CoverColorRecord{{LibraryID: e.lib.ID, Path: p, Art: due[i].Source.Art,
		Color: catalog.CoverColor{Bg: "#112233", Accent: "#ddaa22", OnAccent: "#000000"}}}); err != nil {
		t.Fatal(err)
	}
	return e
}

// wireFoldersEnv: Author X holds an indexed book, a folder set to be a
// collection, and two book folders not indexed yet; sam reads the library.
type wireFoldersEnv struct {
	*testEnv
	lib    *catalog.Library
	samTok string
}

func newWireFoldersEnv(t *testing.T) *wireFoldersEnv {
	t.Helper()
	ctx := context.Background()
	e := newTestEnv(t)
	root := t.TempDir()
	writeTree(t, root, "Author X/Indexed/01.mp3")
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Folders", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	writeTree(t, root, "Author X/Loose/a.mp3", "Author X/New Book/01.mp3", "Author X/Zed/01.mp3")
	if err := e.cat.SetFolderOverride(ctx, lib.ID, "Author X/Loose", catalog.OverrideCollection); err != nil {
		t.Fatal(err)
	}
	sam, samTok := newUserWithSession(t, e, "sam", false)
	if err := e.cat.GrantWholeLibrary(ctx, sam, lib.ID); err != nil {
		t.Fatal(err)
	}
	return &wireFoldersEnv{testEnv: e, lib: lib, samTok: samTok}
}

// wireTarget is an env, the URL a case reads and the token it reads it with.
type wireTarget struct {
	env *testEnv
	url string
	tok string
}

// newWireCopiesEnv: The Hobbit in two libraries (TestSearchDedupWire).
func newWireCopiesEnv(t *testing.T) wireTarget {
	t.Helper()
	ctx := context.Background()
	e := newTestEnv(t)
	var ids []int64
	// A's is one file; B's, the other location, a folder of two.
	for _, bk := range []catalog.Book{
		{RelPath: "hobbit.m4b", Format: "m4b"},
		{RelPath: "The Hobbit", Format: "mp3", IsFolder: true, Files: []catalog.BookFile{
			{RelPath: "The Hobbit/01.mp3", Duration: 50, Size: 500}, {RelPath: "The Hobbit/02.mp3", Seq: 1, Duration: 50, Size: 500}}},
	} {
		lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Library " + strconv.Itoa(len(ids)+1), Root: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, lib.ID)
		bk.LibraryID, bk.Title, bk.Author, bk.Size, bk.Duration, bk.AddedAt = lib.ID, "The Hobbit", "Tolkien", 1000, 100, "2024-01-01T00:00:00Z"
		if _, err := e.cat.UpsertBook(ctx, &bk); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.cat.ReorderLibraries(ctx, ids); err != nil {
		t.Fatal(err)
	}
	return wireTarget{env: e, url: "/api/v1/search?q=hobbit", tok: adminSession(t, e)}
}

// wireEpisodeEnv: the podcast fixture show (podcastEnv) and one of its episodes.
type wireEpisodeEnv struct {
	*bridgeEnv
	episode string
}

func newWireEpisodeEnv(t *testing.T) *wireEpisodeEnv {
	t.Helper()
	b, _, f := podcastEnv(t, false)
	return &wireEpisodeEnv{bridgeEnv: b, episode: f.Episodes[1].Path}
}

// wireDivergedEnv: sam's phone listened to Unsouled (then carried on from
// version), his watch is about to sync a span from that version, and he
// finished Mistborn.
type wireDivergedEnv struct {
	*syncEnv
	phoneTok, watchTok, version string
	synced                      string // the watch's sync's answer, once sent
}

// watchSync sends the watch's sync (a span from behind the phone's place, and
// one refused) once, and returns its answer.
func (s *wireDivergedEnv) watchSync(t *testing.T) string {
	t.Helper()
	if s.synced == "" {
		s.synced = wireOK(t, s.testEnv, "POST", syncURL, s.watchTok, syncBody("watch",
			listenJSON("w1", s.lib.ID, syncUnsouled, 14400, 16800, s.version),
			listenJSON("w2", 9999, syncUnsouled, 0, 60, "")))
	}
	return s.synced
}

func newWireDivergedEnv(t *testing.T) *wireDivergedEnv {
	t.Helper()
	s := newSyncEnv(t)
	ctx := context.Background()
	uid, phone := newUserWithSession(t, s.testEnv, "sam", false)
	if err := s.cat.GrantWholeLibrary(ctx, uid, s.lib.ID); err != nil {
		t.Fatal(err)
	}
	watch, err := s.auth.IssueToken(ctx, uid, auth.KindSession, "Sam's Watch", 0)
	if err != nil {
		t.Fatal(err)
	}
	code, a, raw := s.post(t, phone, syncBody("phone", listenJSON("p1", s.lib.ID, syncUnsouled, 17000, 17500, "0")))
	if code != http.StatusOK || len(a.Progress) != 1 {
		t.Fatalf("phone = %d %s", code, raw)
	}
	version := strconv.FormatInt(a.Progress[0].Version, 10)
	if _, a, raw = s.post(t, phone, syncBody("phone", listenJSON("p2", s.lib.ID, syncUnsouled, 17500, 18000, version))); len(a.Results) != 1 || a.Results[0].Status != "applied" {
		t.Fatalf("phone carries on: %s", raw)
	}
	wireOK(t, s.testEnv, "PUT", bookURL(s.lib.ID, "progress", mistbornBook), phone, `{"position":120,"duration":120,"finished":true}`)
	return &wireDivergedEnv{syncEnv: s, phoneTok: phone, watchTok: watch, version: version}
}

// newWireUnmatchedEnv: a book with no identifiers (so no community match) and
// generated characters and recaps.
func newWireUnmatchedEnv(t *testing.T) wireTarget {
	t.Helper()
	e := generatedMetaEnv(t, `{}`)
	lib := seedChapteredBook(t, e, "A/Local", "")
	setGenerated(t, e, lib.ID, "A/Local")
	return wireTarget{env: e, url: bookURL(lib.ID, "meta", "A/Local"), tok: adminSession(t, e)}
}

// newWireBundleEnv: book two of series S (and of its chronological ordering)
// matched upstream, with every optional part a /meta answer can carry: a
// subtitle, an abridged LibriVox recording, an aliased character, a position
// scale, its own generated description; book one before it (a community
// description) held twice in the library; book three to come.
func newWireBundleEnv(t *testing.T) wireTarget {
	t.Helper()
	works := map[string]string{
		"book-one": `{"id":"book-one","title":"One","authors":[{"id":"a","name":"A"}],"language":"en","series":[{"id":"s","name":"S","position":"1"}],` +
			`"community_description":{"text":"A community description.","license":"CC-BY-SA-4.0"},` +
			`"recap_summary":{"in_short":"One in short.","ending":"One ends."}}`,
		"book-two": `{"id":"book-two","title":"Two","subtitle":"The Second","authors":[{"id":"a","name":"A"}],"language":"en",` +
			`"first_published":"2020","description":"The publisher's blurb.",` +
			`"series":[{"id":"s","name":"S","position":"2"},{"id":"s-chrono","name":"S (Chronological)","position":"1","ordering_of":"s"},` +
			`{"id":"t-read","name":"T (Reading order)","position":"4","ordering_of":"t"}],` +
			`"recordings":[{"id":"r2","narrators":[{"id":"n","name":"N"}],"abridged":true,"runtime_min":300,"release_date":"2021-02-03",` +
			`"publisher":"P","cover_url":"https://c/2.jpg","chapter_count":5,"librivox_id":47,` +
			`"links":{"librivox":"https://librivox.org/two/","zip":"https://archive.org/compress/two/formats=64KBPS%20MP3&file=/two.zip"}}],` +
			`"characters":[{"id":"early","name":"Early","aliases":["E"],"role":"protagonist","reveal":{"chapter":1},"description":"First seen."}],` +
			`"recaps":[{"through":{"chapter":2},"scope":"book","text":"Through two."}],` +
			`"recap_summary":{"in_short":"Two in short.","ending":"Two ends."},"position_scale":{"length":10,"unit":"act","partial":true}}`,
	}
	const orderings = `"orderings":[{"id":"s","name":"S","ordering":"publication"},{"id":"s-chrono","name":"S (Chronological)","ordering":"chronological"}]`
	const tOrderings = `"orderings":[{"id":"t","name":"T","ordering":"publication"},{"id":"t-read","name":"T (Reading order)","ordering":"recommended"}]`
	one := `{"id":"book-one","title":"One","authors":[{"id":"a","name":"A"}],"cover_url":"https://c/1.jpg","release_date":"2020-01"}`
	two := `{"id":"book-two","title":"Two","authors":[{"id":"a","name":"A"}],"cover_url":null}`
	series := map[string]string{
		"s": `{"id":"s","name":"S","ordering":"publication","authors":[],"works":[{"position":"1","work":` + one + `},` +
			`{"position":"2","work":` + two + `},` +
			`{"position":"3","work":{"id":"book-three","title":"Three","authors":[{"id":"a","name":"A"}],"release_date":"2099-05-01"}}],` + orderings + `}`,
		"s-chrono": `{"id":"s-chrono","name":"S (Chronological)","ordering":"chronological","ordering_of":"s","authors":[],` +
			`"works":[{"position":"1","work":` + two + `},{"position":"2","work":` + one + `}],` + orderings + `}`,
		// T's reading order places book two, T's publication order does not:
		// the main view of T is a variant (ordering_of).
		"t": `{"id":"t","name":"T","ordering":"publication","authors":[],"works":[{"position":"1","work":` + one + `}],` + tOrderings + `}`,
		"t-read": `{"id":"t-read","name":"T (Reading order)","ordering":"recommended","ordering_of":"t","authors":[],` +
			`"works":[{"position":"1","work":` + one + `},{"position":"4","work":` + two + `}],` + tOrderings + `}`,
	}
	serve := func(bodies map[string]string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, ok := bodies[r.PathValue("id")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"work":{"id":"book-two","title":"Two","authors":[]},"recording_id":"r2"}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", serve(works))
	mux.HandleFunc("GET /api/v1/series/{id}", serve(series))
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Metadata.Enabled = true
		c.Metadata.BaseURL = mock.URL
	})
	lib := seedChapteredBook(t, e, "A/Two", "B0TWO")
	setDescription(t, e, lib.ID, "A/Two", "description.json:1:1", metadata.AttachmentFiles{})
	for _, p := range []string{"A/One", "A/One Again"} {
		if _, err := e.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: lib.ID, RelPath: p, Title: "One",
			AddedAt: "2020-01-01"}); err != nil {
			t.Fatal(err)
		}
		anchorMatch(t, e, lib.ID, p, "book-one", "")
	}
	return wireTarget{env: e, url: bookURL(lib.ID, "meta", "A/Two"), tok: adminSession(t, e)}
}
