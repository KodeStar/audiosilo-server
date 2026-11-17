package api

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Wire goldens (audiosilo#96, server#347): the shape of each important player
// envelope, produced by the real handlers against a seeded store and committed
// under testdata/wire/<name>.json. A shape is the field set and the JSON type of
// each field, never a value, so ids, times and titles do not churn it:
//
//	"string" | "number" | "boolean" | "null"   a scalar
//	{ "field": <shape>, ... }                  an object
//	[ <shape> ]                                an array, its elements merged ([] when empty)
//	{ "$or": [ <shape>, ... ] }                elements of an array that differ in type,
//	                                           one shape per kind, sorted by kind
//
// The test fails when a handler's output shape differs from its golden: a field
// gone or retyped (a breaking change for shipped clients, CROSS-REPO.md §15) and
// a field added (so every addition shows in the PR's diff). After an intended
// change, rewrite them with
//
//	go test ./internal/api -run TestWireGolden -update
//
// The player reads the same files (audiosilo-frontend src/api/__wire__/, synced
// from here) and checks each against its hand-mirrored type in src/api/types.ts.

var updateWire = flag.Bool("update", false, "rewrite the wire goldens in internal/api/testdata/wire")

const wireDir = "testdata/wire"

// wireCase is one golden: the route it documents and the scenario producing it.
type wireCase struct {
	name  string // the file's name, without .json
	route string // the method, the route and the query used, recorded in the golden
	body  func(t *testing.T) string
}

func TestWireGolden(t *testing.T) {
	t.Parallel()
	cases := wireCases(t)
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.name] {
			t.Fatalf("two cases named %q", c.name)
		}
		seen[c.name] = true
		t.Run(c.name, func(t *testing.T) { checkWireGolden(t, c) })
	}
	// A golden with no case is a stale file: the player would keep checking a
	// shape nothing produces any more.
	entries, err := os.ReadDir(wireDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		name := strings.TrimSuffix(ent.Name(), ".json")
		if !seen[name] {
			if *updateWire {
				if err := os.Remove(filepath.Join(wireDir, ent.Name())); err != nil {
					t.Fatal(err)
				}
				continue
			}
			t.Errorf("%s/%s has no case: remove it, or run with -update", wireDir, ent.Name())
		}
	}
}

// wireGolden is a golden file's content.
type wireGolden struct {
	Route string `json:"route"`
	Shape any    `json:"shape"`
}

func checkWireGolden(t *testing.T, c wireCase) {
	t.Helper()
	body := c.body(t)
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("%s: not JSON: %v: %s", c.route, err, body)
	}
	got := wireGolden{Route: c.route, Shape: shapeOf(v)}
	file := filepath.Join(wireDir, c.name+".json")
	if *updateWire {
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false) // the routes' <placeholders> stay readable
		enc.SetIndent("", "  ")
		if err := enc.Encode(got); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%s: no golden (%v): run go test ./internal/api -run TestWireGolden -update", c.route, err)
	}
	var want wireGolden
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if want.Route != got.Route {
		t.Errorf("%s: route %q, the case says %q (-update)", file, want.Route, got.Route)
	}
	if diff := shapeDiff(want.Shape, got.Shape); len(diff) > 0 {
		t.Errorf("%s changed shape (%s):\n  %s\n"+
			"A removed or retyped field breaks shipped clients (CROSS-REPO.md §15). If the change is intended,\n"+
			"run go test ./internal/api -run TestWireGolden -update, then sync the player's copy\n"+
			"(audiosilo-frontend: npm run wire:sync) and mirror it in src/api/types.ts.",
			c.route, file, strings.Join(diff, "\n  "))
	}
}

// shapeOf is v's shape (the grammar at the top of this file).
func shapeOf(v any) any {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		if len(x) == 0 {
			return []any{}
		}
		el := shapeOf(x[0])
		for _, e := range x[1:] {
			el = mergeShape(el, shapeOf(e))
		}
		return []any{el}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = shapeOf(e)
		}
		return out
	}
	panic(fmt.Sprintf("shapeOf: %T", v))
}

// shapeKind is a shape's kind: its scalar name, "object", "array" or "$or".
func shapeKind(s any) string {
	switch x := s.(type) {
	case string:
		return x
	case []any:
		return "array"
	case map[string]any:
		if _, ok := x["$or"]; ok {
			return "$or"
		}
		return "object"
	}
	return fmt.Sprintf("%T", s)
}

// mergeShape is the shape of a value that is sometimes a and sometimes b (two
// elements of one array): objects merge field by field, arrays by element, and
// different kinds become one $or holding at most one shape of each kind.
func mergeShape(a, b any) any {
	alts := map[string]any{}
	add := func(s any) {
		list := []any{s}
		if shapeKind(s) == "$or" {
			list = s.(map[string]any)["$or"].([]any)
		}
		for _, s := range list {
			k := shapeKind(s)
			prev, ok := alts[k]
			switch {
			case !ok:
				alts[k] = s
			case k == "object":
				out := maps.Clone(prev.(map[string]any))
				for f, v := range s.(map[string]any) {
					if pv, ok := out[f]; ok {
						out[f] = mergeShape(pv, v)
					} else {
						out[f] = v
					}
				}
				alts[k] = out
			case k == "array":
				pa, sa := prev.([]any), s.([]any)
				switch {
				case len(pa) == 0:
					alts[k] = sa
				case len(sa) > 0:
					alts[k] = []any{mergeShape(pa[0], sa[0])}
				}
			}
		}
	}
	add(a)
	add(b)
	out := make([]any, 0, len(alts))
	for _, k := range slices.Sorted(maps.Keys(alts)) {
		out = append(out, alts[k])
	}
	if len(out) == 1 {
		return out[0]
	}
	return map[string]any{"$or": out}
}

// shapeDiff lists how got differs from want, one line per field path.
func shapeDiff(want, got any) []string {
	w, g := map[string]string{}, map[string]string{}
	flattenShape(want, "$", w)
	flattenShape(got, "$", g)
	var out []string
	for p, k := range w {
		switch gk, ok := g[p]; {
		case !ok:
			out = append(out, "removed "+p+" ("+k+")")
		case gk != k:
			out = append(out, "retyped "+p+": "+k+" -> "+gk)
		}
	}
	for p, k := range g {
		if _, ok := w[p]; !ok {
			out = append(out, "added   "+p+" ("+k+")")
		}
	}
	slices.Sort(out)
	return out
}

// flattenShape records each field path's kind ("null|object" for a $or). A $or's
// object and array alternatives keep their children's paths, so a field turning
// nullable is one retype, not its whole subtree removed and added again.
func flattenShape(s any, path string, into map[string]string) {
	alts := []any{s}
	kind := shapeKind(s)
	if kind == "$or" {
		alts = s.(map[string]any)["$or"].([]any)
		kinds := make([]string, len(alts))
		for i, alt := range alts {
			kinds[i] = shapeKind(alt)
		}
		kind = strings.Join(kinds, "|")
	}
	into[path] = kind
	for _, alt := range alts {
		switch x := alt.(type) {
		case map[string]any:
			for k, v := range x {
				flattenShape(v, path+"."+k, into)
			}
		case []any:
			if len(x) > 0 {
				flattenShape(x[0], path+"[]", into)
			}
		}
	}
}

// TestShapeOf pins the shape grammar the player's parity test reads.
func TestShapeOf(t *testing.T) {
	t.Parallel()
	var v any
	if err := json.Unmarshal([]byte(`{"a":1,"b":"x","c":null,"d":[],"e":[{"x":1},{"y":true}],"f":[1,null,"s",{"k":1},{"l":2}]}`), &v); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(shapeOf(v))
	want := `{"a":"number","b":"string","c":"null","d":[],"e":[{"x":"number","y":"boolean"}],` +
		`"f":[{"$or":["null","number",{"k":"number","l":"number"},"string"]}]}`
	if string(got) != want {
		t.Fatalf("shape\n got %s\nwant %s", got, want)
	}
	// Allowed: an identical shape has no diff. Denied: a removal, a retype and
	// an addition are each reported.
	base := map[string]any{"a": "number", "b": []any{map[string]any{"c": "string"}}}
	if d := shapeDiff(base, base); len(d) != 0 {
		t.Fatalf("same shape differs: %v", d)
	}
	changed := map[string]any{"a": "string", "b": []any{map[string]any{"d": "string"}}}
	d := shapeDiff(base, changed)
	want2 := []string{"added   $.b[].d (string)", "removed $.b[].c (string)", "retyped $.a: number -> string"}
	if !slices.Equal(d, want2) {
		t.Fatalf("diff = %q, want %q", d, want2)
	}
	// A field turning nullable is one retype, its children unchanged.
	nullable := map[string]any{"a": "number", "b": []any{map[string]any{"$or": []any{"null", map[string]any{"c": "string"}}}}}
	d = shapeDiff(base, nullable)
	want3 := []string{"retyped $.b[]: object -> null|object"}
	if !slices.Equal(d, want3) {
		t.Fatalf("diff = %q, want %q", d, want3)
	}
}

// wireCases builds the scenarios' envs and the cases reading them. The cases
// run in order, one at a time: several share an env, and a write one makes (an
// exchange, a sync) must not race a read whose shape it could change.
func wireCases(t *testing.T) []wireCase {
	t.Helper()
	m, h, u := newWireMainEnv(t), newWireHouseEnv(t), newUpcomingEnv(t)
	call := func(e *testEnv, method, path, tok, body string) func(*testing.T) string {
		return func(t *testing.T) string { return wireOK(t, e, method, path, tok, body) }
	}
	get := func(path, tok string) func(*testing.T) string { return call(m.testEnv, "GET", path, tok, "") }
	newCode := func(t *testing.T) string {
		code, err := m.auth.CreateAuthCode(context.Background(), m.adminID, "wire", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	lib := func(endpoint, path string) func(*testing.T) string {
		return get(bookURL(m.lib.ID, endpoint, path), m.samTok)
	}
	libRoute := "/api/v1/libraries/" + strconv.FormatInt(m.lib.ID, 10)

	return []wireCase{
		{name: "server", route: "GET /api/v1/server (signed in)", body: get("/api/v1/server", m.adminTok)},
		{name: "server-anonymous", route: "GET /api/v1/server", body: get("/api/v1/server", "")},
		{name: "auth-redeem", route: "POST /api/v1/auth/redeem", body: func(t *testing.T) string {
			return wireOK(t, m.testEnv, "POST", "/api/v1/auth/redeem", "", `{"code":"`+newCode(t)+`"}`)
		}},
		{name: "auth-exchange", route: "POST /api/v1/auth/exchange (a household member's pairing)", body: func(t *testing.T) string {
			_, pairing, err := m.auth.MemberPairing(context.Background(), m.adminID, m.member, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			status, b := m.exchangeToken(t, pairing, "Wire phone")
			if status != http.StatusOK {
				t.Fatalf("exchange = %d %s", status, b)
			}
			return b
		}},
		{name: "me", route: "GET /api/v1/me (a household member)", body: get("/api/v1/me", m.memberTok)},
		{name: "libraries", route: "GET /api/v1/libraries (an admin: with root)", body: get("/api/v1/libraries", m.adminTok)},
		{name: "fs", route: "GET /api/v1/libraries/{id}/fs?path=Will Wight", body: lib("fs", "Will Wight")},
		{name: "books", route: "GET /api/v1/libraries/{id}/books", body: get(libRoute+"/books", m.samTok)},
		{name: "authors", route: "GET /api/v1/libraries/{id}/authors", body: get(libRoute+"/authors", m.samTok)},
		{name: "series", route: "GET /api/v1/libraries/{id}/series", body: get(libRoute+"/series", m.samTok)},
		{name: "search", route: "GET /api/v1/search?q=cradle", body: get("/api/v1/search?q=cradle", m.samTok)},
		{name: "item", route: "GET /api/v1/libraries/{id}/item?path=<a two-file book>", body: lib("item", cradleBook)},
		{name: "chapters", route: "GET /api/v1/libraries/{id}/chapters?path=<a two-file book>", body: lib("chapters", cradleBook)},
		{name: "progress", route: "GET /api/v1/libraries/{id}/progress?path=<a book>", body: lib("progress", cradleBook)},
		{name: "me-progress", route: "GET /api/v1/me/progress", body: get("/api/v1/me/progress", m.samTok)},
		{name: "listening-sync", route: "POST /api/v1/me/listening/sync", body: call(m.testEnv, "POST", syncURL, m.samTok,
			syncBody("wire-device", listenJSON("wire-1", m.lib.ID, syncMistborn, 0, 60, "")))},
		{name: "bookmarks", route: "GET /api/v1/libraries/{id}/bookmarks?path=<a book>", body: lib("bookmarks", cradleBook)},
		{name: "meta", route: "GET /api/v1/libraries/{id}/meta?path=<a matched book>",
			body: get(bookURL(m.metaLib, "meta", metaBook), m.adminTok)},
		{name: "upcoming", route: "GET /api/v1/me/upcoming", body: call(u.testEnv, "GET", upcomingPath, adminSession(t, u.testEnv), "")},
		{name: "household-listening", route: "GET /api/v1/household/listening (sharing, one other person listening)",
			body: call(h.testEnv, "GET", "/api/v1/household/listening", h.patTok, "")},
	}
}

// wireOK sends a request and returns its body, failing on anything but a 2xx.
func wireOK(t *testing.T, e *testEnv, method, path, tok, body string) string {
	t.Helper()
	resp, b := e.do(t, method, path, tok, body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, b)
	}
	return b
}

// wireMainEnv: a copy of the fixture library with Cradle dressed in every
// optional field a seed can cheaply give it (an Audiobookshelf metadata.json, an
// ebook and its read-along map), scanned; a listener (sam) granted it with a
// place and a bookmark; a household member of the admin's; watch renditions on;
// and a second library holding a book the mock metaserve matches. A field a
// golden should guard has to appear here: an omitempty one left unset is absent
// from the shape, so its removal would pass.
type wireMainEnv struct {
	*testEnv
	lib       *catalog.Library
	metaLib   int64
	adminTok  string
	samTok    string
	member    int64
	memberTok string
}

// metaBook is the book the mock metaserve matches.
const metaBook = "Andy Weir/The Martian"

// cradleABS is Cradle's Audiobookshelf metadata.json: two series (series_list,
// the main one a range), and the descriptive fields /item carries.
const cradleABS = `{"title":"Unsouled","subtitle":"Cradle, Book 1","authors":["Will Wight"],"narrators":["Travis Baldree"],
	"series":[{"name":"Cradle","sequence":"1-2"},{"name":"Sacred Valley","sequence":"1"}],
	"publishedDate":"2016-06-01","description":"A boy with no power.","publisher":"Hidden Gnome","language":"English",
	"abridged":true,"genres":["Fantasy"],"tags":["progression"],"isbn":"9780989671767","asin":"B01H1CYG2M"}`

func newWireMainEnv(t *testing.T) *wireMainEnv {
	t.Helper()
	ctx := context.Background()
	e := newMetaEnvMock(t, true, &mockMetaserve{match: true})
	e.api.ffmpeg = "ffmpeg" // renditions on (nothing here encodes one)
	root := copyFixtures(t, cradlePart, "Will Wight/Cradle/02 - Soulsmith.m4b", syncMistborn)
	epub := testEpub(t, false)
	writeMap(t, root, cradleBook+"/"+metadata.ABSFileName, []byte(cradleABS))
	writeMap(t, root, cradleBook+"/Book.epub", epub)
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	writeMap(t, root, cradleBook+"/.audiosilo/readalong.json",
		readalongMapJSON(t, sha256Hex(epub), bookAudio(t, e.cat, lib.ID, cradleBook)))
	scanFixture(t, e, lib)

	sam, samTok := newUserWithSession(t, e, "sam", false)
	if err := e.cat.GrantWholeLibrary(ctx, sam, lib.ID); err != nil {
		t.Fatal(err)
	}
	wireOK(t, e, "PUT", bookURL(lib.ID, "progress", cradleBook), samTok, `{"position":42,"duration":120}`)
	wireOK(t, e, "POST", bookURL(lib.ID, "bookmarks", cradleBook), samTok, `{"position":12,"note":"here","label":"favourite"}`)
	member, _, _, err := e.auth.CreateMember(ctx, e.adminID, auth.NewMember{Name: "Pip", Kind: auth.MemberGrownup,
		Access: auth.MemberAll}, auth.MemberPolicy{Enabled: true, Max: 5})
	if err != nil {
		t.Fatal(err)
	}

	metaLib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Matched", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: metaLib.ID, RelPath: metaBook,
		Title: "The Martian", Author: "Andy Weir", ASIN: "B00B5HZGUG", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	return &wireMainEnv{testEnv: e, lib: lib, metaLib: metaLib.ID, adminTok: adminSession(t, e), samTok: samTok,
		member: member.ID, memberTok: e.session(t, member.ID)}
}

// newWireHouseEnv: sam and pat share their listening, sam has listened.
func newWireHouseEnv(t *testing.T) *houseEnv {
	t.Helper()
	h := newHouseEnv(t)
	h.setShare(t, h.samTok, true)
	h.setShare(t, h.patTok, true)
	h.listen(t, h.samTok, cradleBook)
	return h
}
