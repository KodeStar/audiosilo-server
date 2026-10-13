package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// Household members (Phase 8b-1): the server test table of the plan, one test
// (or more) per row, named TestMembers_<Row>_*, each with its allowed and denied
// halves. memberEnv: a copy of the fixture library (lib: Cradle, its "Cradle 2"
// sibling, Mistborn) and the fixture library itself (other); adam reaches lib
// through a share the tests revoke, bob through his own. Members are made through
// the API, as the player makes them.

type memberEnv struct {
	*testEnv
	lib, other      int64
	adam, bob       int64
	adamTok, bobTok string
	adminTok        string
	adamShare       int64
}

func newMemberEnv(t *testing.T) *memberEnv {
	return newMemberEnvWith(t, nil)
}

func newMemberEnvWith(t *testing.T, configure func(*config.Config)) *memberEnv {
	t.Helper()
	e := newTestEnvWith(t, configure)
	// The tests make more members, sessions and keys than the per-address
	// account limit (10 / 15 min) and the general burst allow one address.
	e.api.accountLimiter = newLimiter(100000, time.Minute)
	e.api.ipLimiter = newRateLimiter(100000, 100000)
	ctx := context.Background()
	root := copyFixtures(t, cradlePart, cradlePart2, mistbornBook+"/01 - The Final Empire.m4b")
	copyFixture(t, root, cradlePart, cradleTwoF)
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Copy", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	other := newFixtureLibrary(t, e)
	m := &memberEnv{testEnv: e, lib: lib.ID, other: other.ID}
	m.adam, m.adamTok = newUserWithSession(t, e, "adam", false)
	m.bob, m.bobTok = newUserWithSession(t, e, "bob", false)
	share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Adam's", Paths: []catalog.PathRule{{LibraryID: lib.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	m.adamShare = share.ID
	if err := e.cat.GrantShare(ctx, m.adam, share.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantWholeLibrary(ctx, m.bob, lib.ID); err != nil {
		t.Fatal(err)
	}
	if m.adminTok, err = e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "admin phone", 0); err != nil {
		t.Fatal(err)
	}
	return m
}

// createdMember is POST /me/household/members' answer.
type createdMember struct {
	Member  householdMember `json:"member"`
	Session *struct {
		Token    string `json:"token"`
		User     meUser `json:"user"`
		ServerID string `json:"server_id"`
	} `json:"session"`
}

func (m *memberEnv) add(t *testing.T, tok, body string) (int, string) {
	t.Helper()
	resp, b := m.do(t, "POST", "/api/v1/me/household/members", tok, body)
	return resp.StatusCode, b
}

// mustAdd adds a member as tok, wanting 201.
func (m *memberEnv) mustAdd(t *testing.T, tok, body string) createdMember {
	t.Helper()
	status, b := m.add(t, tok, body)
	if status != http.StatusCreated {
		t.Fatalf("add %s = %d %s", body, status, b)
	}
	var out createdMember
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// kid adds a kid on collections signed in on tok's device, sharing cols; it
// returns the member and its session token.
func (m *memberEnv) kid(t *testing.T, tok, name string, cols ...int64) (householdMember, string) {
	t.Helper()
	ids, _ := json.Marshal(append([]int64{}, cols...))
	out := m.mustAdd(t, tok, fmt.Sprintf(`{"name":%q,"kind":"kid","access":"collections","sign_in_here":true,"collection_ids":%s}`, name, ids))
	if out.Session == nil {
		t.Fatal("kid has no session")
	}
	return out.Member, out.Session.Token
}

// grownup adds a grown-up with access, signed in through the owner's session
// route; it returns the member and a session token.
func (m *memberEnv) grownup(t *testing.T, tok, name, access string) (householdMember, string) {
	t.Helper()
	out := m.mustAdd(t, tok, fmt.Sprintf(`{"name":%q,"kind":"grownup","access":%q}`, name, access))
	if out.Session != nil {
		t.Fatal("grown-up added with a session")
	}
	return out.Member, m.memberSession(t, tok, out.Member.ID)
}

func (m *memberEnv) memberSession(t *testing.T, tok string, id int64) string {
	t.Helper()
	resp, b := m.do(t, "POST", memberURL(id, "session"), tok, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("member session = %d %s", resp.StatusCode, b)
	}
	var env struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal([]byte(b), &env)
	return env.Token
}

func memberURL(id int64, rest string) string {
	u := "/api/v1/me/household/members/" + strconv.FormatInt(id, 10)
	if rest != "" {
		u += "/" + rest
	}
	return u
}

// collection makes tok's collection holding paths of lib and returns its id.
func (m *memberEnv) collection(t *testing.T, tok, name string, paths ...string) int64 {
	t.Helper()
	id := m.createCollection(t, tok, name).ID
	for _, p := range paths {
		m.addItem(t, tok, id, p)
	}
	return id
}

func (m *memberEnv) addItem(t *testing.T, tok string, col int64, path string) {
	t.Helper()
	resp, b := m.do(t, "POST", fmt.Sprintf("/api/v1/me/collections/%d/items", col), tok,
		fmt.Sprintf(`{"library_id":%d,"path":%q}`, m.lib, path))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add %s = %d %s", path, resp.StatusCode, b)
	}
}

// status is the answer's status code for method path as tok.
func (m *memberEnv) status(t *testing.T, method, path, tok, body string) int {
	t.Helper()
	resp, _ := m.do(t, method, path, tok, body)
	return resp.StatusCode
}

// item is GET /libraries/{lib}/item?path= as tok.
func (m *memberEnv) item(t *testing.T, tok string, lib int64, path string) int {
	t.Helper()
	return m.status(t, "GET", bookURL(lib, "item", path), tok, "")
}

func (m *memberEnv) me(t *testing.T, tok string) meUser {
	t.Helper()
	resp, b := m.do(t, "GET", "/api/v1/me", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /me = %d %s", resp.StatusCode, b)
	}
	var u meUser
	_ = json.Unmarshal([]byte(b), &u)
	return u
}

func (m *memberEnv) list(t *testing.T, tok string) householdMembers {
	t.Helper()
	resp, b := m.do(t, "GET", "/api/v1/me/household/members", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET members = %d %s", resp.StatusCode, b)
	}
	var out householdMembers
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func codeOf(body string) string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(body), &e)
	return e.Code
}

// Create (allowed): an ordinary user adds a kid signed in here and a grown-up
// with no session; each works on GET /me and shows managed_by and member_kind.
func TestMembers_Create_Allowed(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	evie := m.mustAdd(t, m.adamTok, `{"name":"Evie","kind":"kid","access":"collections","sign_in_here":true,"collection_ids":[]}`)
	if evie.Session == nil || evie.Session.Token == "" ||
		evie.Session.User.ManagedBy == nil || evie.Session.User.ManagedBy.Username != "adam" {
		t.Fatalf("kid session = %+v", evie.Session)
	}
	if evie.Member.Name != "Evie" || evie.Member.Username != "evie" || evie.Member.Kind != "kid" || evie.Member.Access != "collections" ||
		evie.Member.Collections == nil || len(*evie.Member.Collections) != 0 || evie.Member.Devices != 1 || evie.Member.LastListenedAt != nil {
		t.Fatalf("kid = %+v", evie.Member)
	}
	u := m.me(t, evie.Session.Token)
	if u.ManagedBy == nil || u.ManagedBy.ID != m.adam || u.MemberKind == nil || *u.MemberKind != "kid" || u.HasPassword || u.Role != "user" {
		t.Fatalf("kid /me = %+v", u)
	}
	// The kid's session was made on adam's phone and is named like it.
	devices, _ := myDevicesOf(t, m.testEnv, evie.Session.Token)
	if len(devices) != 1 || devices[0].Name != "adam's phone" {
		t.Fatalf("kid devices = %+v", devices)
	}

	ellis := m.mustAdd(t, m.adamTok, `{"name":"Ellis","kind":"grownup","access":"all"}`)
	if ellis.Session != nil || ellis.Member.Kind != "grownup" || ellis.Member.Collections != nil || ellis.Member.Devices != 0 {
		t.Fatalf("grown-up = %+v", ellis)
	}
	tok := m.memberSession(t, m.adamTok, ellis.Member.ID)
	if u := m.me(t, tok); u.ManagedBy == nil || *u.MemberKind != "grownup" {
		t.Fatalf("grown-up /me = %+v", u)
	}
	// The raw wire: null for a non-member, not omitted; no member_access on /me.
	_, body := m.do(t, "GET", "/api/v1/me", m.adamTok, "")
	if !strings.Contains(body, `"managed_by":null`) || !strings.Contains(body, `"member_kind":null`) ||
		strings.Contains(body, "member_access") || strings.Contains(body, "display_name") {
		t.Fatalf("owner /me = %s", body)
	}
	list := m.list(t, m.adamTok)
	if !list.Allowed || list.Reason != "" || list.Max != 6 || len(list.Members) != 2 || list.Members[0].ID != evie.Member.ID {
		t.Fatalf("list = %+v", list)
	}
}

// Display name (allowed): a member's /me and session envelopes carry the name
// the owner typed (display_name), and a rename reaches /me.
func TestMembers_DisplayName_Allowed(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	evie := m.mustAdd(t, m.adamTok, `{"name":"Evie Rose","kind":"kid","access":"collections","sign_in_here":true,"collection_ids":[]}`)
	if evie.Session == nil || evie.Session.User.MemberName != "Evie Rose" {
		t.Fatalf("kid session envelope = %+v", evie.Session)
	}
	if u := m.me(t, evie.Session.Token); u.MemberName != "Evie Rose" {
		t.Fatalf("kid /me display_name = %q", u.MemberName)
	}
	ellis := m.mustAdd(t, m.adamTok, `{"name":"Ellis","kind":"grownup","access":"all"}`)
	resp, b := m.do(t, "POST", memberURL(ellis.Member.ID, "session"), m.adamTok, "")
	if resp.StatusCode != http.StatusCreated || !strings.Contains(b, `"display_name":"Ellis"`) {
		t.Fatalf("member session envelope = %d %s", resp.StatusCode, b)
	}
	if resp, b := m.do(t, "PATCH", memberURL(evie.Member.ID, ""), m.adamTok, `{"name":"Evie"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("rename = %d %s", resp.StatusCode, b)
	}
	if u := m.me(t, evie.Session.Token); u.MemberName != "Evie" {
		t.Fatalf("renamed kid /me display_name = %q", u.MemberName)
	}
}

// Display name (denied): an ordinary person's /me and session envelope carry no
// display_name, and neither does a former member's (made independent; the
// stored name stays on the admin's user object only).
func TestMembers_DisplayName_Denied(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	if _, b := m.do(t, "GET", "/api/v1/me", m.bobTok, ""); strings.Contains(b, "display_name") {
		t.Fatalf("ordinary /me = %s", b)
	}
	ellis := m.mustAdd(t, m.adamTok, `{"name":"Ellis","kind":"grownup","access":"all"}`)
	tok := m.memberSession(t, m.adamTok, ellis.Member.ID)
	if resp, b := m.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(ellis.Member.ID, 10), m.adminTok, `{"managed_by":null}`); resp.StatusCode != http.StatusOK ||
		!strings.Contains(b, `"display_name":"Ellis"`) {
		t.Fatalf("make independent = %d %s", resp.StatusCode, b)
	}
	if _, b := m.do(t, "GET", "/api/v1/me", tok, ""); strings.Contains(b, "display_name") {
		t.Fatalf("former member /me = %s", b)
	}
}

// Create (denied): feature off, owner switched off, demo, API key, member caller,
// the cap, an unknown field, another user's collection, anonymous, and a kid's
// password login.
func TestMembers_Create_Denied(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ctx := context.Background()
	kid := `{"name":"Evie","kind":"kid","access":"collections"}`

	_, demoTok := newUserWithSession(t, m.testEnv, "demo_1", true)
	if status, b := m.add(t, demoTok, kid); status != http.StatusForbidden || codeOf(b) != "demo_account" {
		t.Errorf("demo = %d %s", status, b)
	}
	key, _, err := m.auth.IssueAPIToken(ctx, m.adam, "script")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := m.add(t, key, kid); status != http.StatusForbidden {
		t.Errorf("API key = %d", status)
	}
	if status, _ := m.add(t, "", kid); status != http.StatusUnauthorized {
		t.Errorf("anonymous = %d", status)
	}
	if status, b := m.add(t, m.adamTok, `{"name":"Evie","kind":"kid","access":"all","role":"admin"}`); status != http.StatusBadRequest {
		t.Errorf("role field = %d %s", status, b)
	}
	for body, code := range map[string]string{
		`{"name":"","kind":"kid","access":"all"}`:           "invalid_name",
		`{"name":"Evie","kind":"teen","access":"all"}`:      "",
		`{"name":"Evie","kind":"kid","access":"most"}`:      "",
		`{"name":"Ev\u0007ie","kind":"kid","access":"all"}`: "invalid_name",
	} {
		if status, b := m.add(t, m.adamTok, body); status != http.StatusBadRequest || codeOf(b) != code {
			t.Errorf("%s = %d %s", body, status, b)
		}
	}
	bobs := m.collection(t, m.bobTok, "Bob's")
	if status, b := m.add(t, m.adamTok, fmt.Sprintf(`{"name":"Evie","kind":"kid","access":"collections","collection_ids":[%d]}`, bobs)); status != http.StatusBadRequest || codeOf(b) != "invalid_collections" {
		t.Errorf("bob's collection = %d %s", status, b)
	}
	if n := len(m.list(t, m.adamTok).Members); n != 0 {
		t.Fatalf("refused creates made %d members", n)
	}

	// A member caller.
	evie, evieTok := m.kid(t, m.adamTok, "Evie")
	if status, b := m.add(t, evieTok, kid); status != http.StatusForbidden || codeOf(b) != "member_cannot_manage" {
		t.Errorf("member caller = %d %s", status, b)
	}
	// The kid has no password to log in with.
	if resp, _ := m.do(t, "POST", "/api/v1/auth/login", "", `{"username":"evie","password":""}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("kid password login = %d", resp.StatusCode)
	}
	_ = evie

	// The admin's switch for adam, then the server's.
	if status, b := m.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(m.adam, 10), m.adminTok, `{"can_add_members":false}`); status.StatusCode != http.StatusOK {
		t.Fatalf("switch off = %d %s", status.StatusCode, b)
	}
	if status, b := m.add(t, m.adamTok, kid); status != http.StatusForbidden || codeOf(b) != "not_allowed" {
		t.Errorf("switched off = %d %s", status, b)
	}
	if l := m.list(t, m.adamTok); l.Allowed || l.Reason != "not_allowed" || len(l.Members) != 1 {
		t.Errorf("list switched off = %+v", l)
	}
	if status, _ := m.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(m.adam, 10), m.adminTok, `{"can_add_members":true}`); status.StatusCode != http.StatusOK {
		t.Fatal("switch on")
	}
	if resp, b := m.do(t, "PATCH", "/api/v1/admin/settings", m.adminTok, `{"household":{"members_enabled":false}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("feature off = %d %s", resp.StatusCode, b)
	}
	if status, b := m.add(t, m.adamTok, kid); status != http.StatusForbidden || codeOf(b) != "feature_off" {
		t.Errorf("feature off = %d %s", status, b)
	}
	if l := m.list(t, m.adamTok); l.Allowed || l.Reason != "feature_off" {
		t.Errorf("list feature off = %+v", l)
	}
	// The existing member's routes keep working with the feature off.
	if status := m.status(t, "PATCH", memberURL(evie.ID, ""), m.adamTok, `{"name":"Evelyn"}`); status != http.StatusOK {
		t.Errorf("rename with the feature off = %d", status)
	}

	// The cap.
	if resp, b := m.do(t, "PATCH", "/api/v1/admin/settings", m.adminTok, `{"household":{"members_enabled":true,"max_members":2}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("cap = %d %s", resp.StatusCode, b)
	}
	m.mustAdd(t, m.adamTok, `{"name":"Ellis","kind":"grownup","access":"all"}`)
	if status, b := m.add(t, m.adamTok, kid); status != http.StatusConflict || codeOf(b) != "member_limit" {
		t.Errorf("at the cap = %d %s", status, b)
	}
	if l := m.list(t, m.adamTok); !l.Allowed || l.Max != 2 || len(l.Members) != 2 {
		t.Errorf("list at the cap = %+v", l)
	}
	// The list for a demo, a member and an API key.
	if l := m.list(t, demoTok); l.Allowed || l.Reason != "demo" || len(l.Members) != 0 {
		t.Errorf("demo list = %+v", l)
	}
	if l := m.list(t, evieTok); l.Allowed || l.Reason != "member" || len(l.Members) != 0 {
		t.Errorf("member list = %+v", l)
	}
	if status := m.status(t, "GET", "/api/v1/me/household/members", key, ""); status != http.StatusForbidden {
		t.Errorf("API key list = %d", status)
	}
}

// Collections access (allowed): the member streams, browses and searches a book
// in the owner's collection shared with them, and GET /libraries lists it.
func TestMembers_CollectionsAccess_Allowed(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	col := m.collection(t, m.adamTok, "Evie's books", cradleBook)
	_, tok := m.kid(t, m.adamTok, "Evie", col)

	if s := m.status(t, "GET", bookURL(m.lib, "stream", cradlePart), tok, ""); s != http.StatusOK && s != http.StatusPartialContent {
		t.Fatalf("stream = %d", s)
	}
	if s := m.item(t, tok, m.lib, cradleBook); s != http.StatusOK {
		t.Fatalf("item = %d", s)
	}
	_, body := m.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/books", m.lib), tok, "")
	var page catalog.Page
	_ = json.Unmarshal([]byte(body), &page)
	if len(page.Books) != 1 || page.Books[0].RelPath != cradleBook {
		t.Fatalf("browse = %s", body)
	}
	_, body = m.do(t, "GET", "/api/v1/search?q=Wight", tok, "")
	var found struct{ Books []catalog.Book }
	_ = json.Unmarshal([]byte(body), &found)
	if len(found.Books) != 1 || found.Books[0].RelPath != cradleBook {
		t.Fatalf("search = %s", body)
	}
	_, body = m.do(t, "GET", "/api/v1/libraries", tok, "")
	var libs struct{ Libraries []catalog.Library }
	_ = json.Unmarshal([]byte(body), &libs)
	if len(libs.Libraries) != 1 || libs.Libraries[0].ID != m.lib {
		t.Fatalf("libraries = %s", body)
	}
	// The member list shows the shared collection, counted.
	l := m.list(t, m.adamTok)
	if c := l.Members[0].Collections; c == nil || len(*c) != 1 || (*c)[0].ID != col || (*c)[0].Items != 1 || (*c)[0].Name != "Evie's books" {
		t.Fatalf("member collections = %+v", l.Members[0])
	}
}

// Collections access (denied): a book not in a shared collection, one in a
// collection a non-owner shared, one the owner lost access to (the live
// intersection: the owner's grant narrowed to a folder that doesn't hold it),
// the segment-boundary sibling and a ".." path.
func TestMembers_CollectionsAccess_Denied(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	col := m.collection(t, m.adamTok, "Ellis's books", cradleBook, mistbornBook)
	ellis, tok := m.grownup(t, m.adamTok, "Ellis", "collections")
	if s := m.status(t, "PATCH", memberURL(ellis.ID, ""), m.adamTok, fmt.Sprintf(`{"collection_ids":[%d]}`, col)); s != http.StatusOK {
		t.Fatalf("share = %d", s)
	}
	bobs := m.collection(t, m.bobTok, "Bob's", cradleTwo)
	if s := m.status(t, "PUT", fmt.Sprintf("/api/v1/me/collections/%d/shares", bobs), m.bobTok, fmt.Sprintf(`{"user_ids":[%d]}`, ellis.ID)); s != http.StatusOK {
		t.Fatalf("bob shares with ellis = %d", s)
	}
	if m.item(t, tok, m.lib, cradleBook) != http.StatusOK || m.item(t, tok, m.lib, mistbornBook) != http.StatusOK {
		t.Fatal("shared books unreachable")
	}
	for _, p := range []string{cradleTwo, cradleTwoF, "Will Wight/Cradle/../Cradle 2", "Will Wight"} {
		if s := m.item(t, tok, m.lib, p); s != http.StatusForbidden {
			t.Errorf("%q = %d, want 403", p, s)
		}
	}
	if s := m.status(t, "GET", bookURL(m.lib, "stream", "Will Wight/Cradle/../Cradle 2/01 - Unsouled.m4b"), tok, ""); s != http.StatusForbidden {
		t.Errorf(".. stream = %d", s)
	}
	if s := m.item(t, tok, m.other, cradleBook); s != http.StatusForbidden {
		t.Errorf("another library = %d", s)
	}
	// The owner keeps only the "Will Wight" folder: Mistborn, outside it, drops out.
	ctx := context.Background()
	if err := m.cat.RevokeShare(ctx, m.adam, m.adamShare); err != nil {
		t.Fatal(err)
	}
	grantWightOnly(t, m.testEnv, m.lib, m.adam)
	if s := m.item(t, tok, m.lib, mistbornBook); s != http.StatusForbidden {
		t.Errorf("a book the owner lost = %d", s)
	}
	if s := m.item(t, tok, m.lib, cradleBook); s != http.StatusOK {
		t.Errorf("a book the owner keeps = %d", s)
	}
	if l := m.list(t, m.adamTok); (*l.Members[0].Collections)[0].Items != 1 {
		t.Errorf("count after the owner lost a book = %+v", l.Members[0].Collections)
	}
}

// Everything access: a kid or grown-up reaches every book the owner reaches,
// including a library granted afterwards; a library revoked from the owner is
// 403 at once.
func TestMembers_EverythingAccess(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ellis, ellisTok := m.grownup(t, m.adamTok, "Ellis", "all")
	_ = ellis
	evie := m.mustAdd(t, m.adamTok, `{"name":"Evie","kind":"kid","access":"all","sign_in_here":true}`)
	for _, tok := range []string{ellisTok, evie.Session.Token} {
		for _, p := range []string{cradleBook, cradleTwo, mistbornBook} {
			if s := m.item(t, tok, m.lib, p); s != http.StatusOK {
				t.Errorf("%q = %d", p, s)
			}
		}
		if s := m.item(t, tok, m.other, cradleBook); s != http.StatusForbidden {
			t.Errorf("before the grant = %d", s)
		}
	}
	ctx := context.Background()
	if err := m.cat.GrantWholeLibrary(ctx, m.adam, m.other); err != nil {
		t.Fatal(err)
	}
	if s := m.item(t, ellisTok, m.other, cradleBook); s != http.StatusOK {
		t.Errorf("a library granted afterwards = %d", s)
	}
	if err := m.cat.RevokeShare(ctx, m.adam, m.adamShare); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{ellisTok, evie.Session.Token} {
		if s := m.item(t, tok, m.lib, cradleBook); s != http.StatusForbidden {
			t.Errorf("after the revoke = %d", s)
		}
	}
}

// Everything access, admin owner: the member reaches every library (as content)
// and gets 403 on every /admin route group.
func TestMembers_EverythingAccess_AdminOwner(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	_, tok := m.grownup(t, m.adminTok, "Ellis", "all")
	for _, lib := range []int64{m.lib, m.other} {
		if s := m.item(t, tok, lib, cradleBook); s != http.StatusOK {
			t.Errorf("library %d = %d", lib, s)
		}
	}
	for _, p := range []string{"users", "stats", "settings", "libraries", "shares", "audit", "books", "devices", "system",
		"backups", "notifications", "issues", "jobs", "imports", "match-runs", "invites", "logs", "events", "support"} {
		if s := m.status(t, "GET", "/api/v1/admin/"+p, tok, ""); s != http.StatusForbidden {
			t.Errorf("/admin/%s = %d", p, s)
		}
	}
	if s := m.status(t, "POST", "/api/v1/admin/users", tok, `{"username":"x"}`); s != http.StatusForbidden {
		t.Errorf("admin write = %d", s)
	}
}

// Access and kind changes: collections to everything is wider at once and back
// narrower; a kind change leaves access; grown-up to kid clears the password and
// API keys while devices stay signed in. Denied: the member can't change their
// own, another person gets 404.
func TestMembers_AccessAndKindChanges(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	col := m.collection(t, m.adamTok, "Evie's books", cradleBook)
	evie, evieTok := m.kid(t, m.adamTok, "Evie", col)
	patch := func(tok string, id int64, body string) (int, householdMember) {
		t.Helper()
		resp, b := m.do(t, "PATCH", memberURL(id, ""), tok, body)
		var out householdMember
		_ = json.Unmarshal([]byte(b), &out)
		return resp.StatusCode, out
	}
	if m.item(t, evieTok, m.lib, mistbornBook) != http.StatusForbidden {
		t.Fatal("collections member reaches an unshared book")
	}
	if s, out := patch(m.adamTok, evie.ID, `{"access":"all"}`); s != http.StatusOK || out.Access != "all" || out.Collections != nil {
		t.Fatalf("to all = %d %+v", s, out)
	}
	if m.item(t, evieTok, m.lib, mistbornBook) != http.StatusOK {
		t.Fatal("not wider at once")
	}
	if s, _ := patch(m.adamTok, evie.ID, `{"access":"collections"}`); s != http.StatusOK || m.item(t, evieTok, m.lib, mistbornBook) != http.StatusForbidden {
		t.Fatal("not narrower at once")
	}
	if s, out := patch(m.adamTok, evie.ID, `{"kind":"grownup"}`); s != http.StatusOK || out.Access != "collections" || out.Kind != "grownup" {
		t.Fatalf("kind change = %d %+v", s, out)
	}
	// As a grown-up: a password and an API key, then back to a kid.
	if s := m.status(t, "POST", "/api/v1/auth/password", evieTok, `{"password":"evie-password"}`); s != http.StatusNoContent {
		t.Fatalf("grown-up password = %d", s)
	}
	resp, b := m.do(t, "POST", "/api/v1/auth/tokens", evieTok, `{"label":"dash"}`)
	var key struct{ Token string }
	_ = json.Unmarshal([]byte(b), &key)
	if resp.StatusCode != http.StatusOK || key.Token == "" {
		t.Fatalf("grown-up key = %d %s", resp.StatusCode, b)
	}
	if s, out := patch(m.adamTok, evie.ID, `{"kind":"kid"}`); s != http.StatusOK || out.Kind != "kid" || out.Access != "collections" {
		t.Fatalf("to kid = %d %+v", s, out)
	}
	if u := m.me(t, evieTok); u.HasPassword || *u.MemberKind != "kid" {
		t.Fatalf("kid keeps a password: %+v", u)
	}
	if s := m.status(t, "GET", "/api/v1/me", key.Token, ""); s != http.StatusUnauthorized {
		t.Fatalf("kid's API key = %d", s)
	}
	if resp, _ := m.do(t, "POST", "/api/v1/auth/login", "", `{"username":"evie","password":"evie-password"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("kid logs in with the old password")
	}

	// Denied.
	if s, _ := patch(evieTok, evie.ID, `{"access":"all"}`); s != http.StatusForbidden {
		t.Errorf("own access change = %d", s)
	}
	if s, _ := patch(m.bobTok, evie.ID, `{"kind":"grownup"}`); s != http.StatusNotFound {
		t.Errorf("bob's change = %d", s)
	}
	for _, body := range []string{`{}`, `{"role":"admin"}`, `{"kind":"teen"}`, `{"name":""}`} {
		if s, _ := patch(m.adamTok, evie.ID, body); s != http.StatusBadRequest {
			t.Errorf("%s = %d", body, s)
		}
	}
	if u := m.me(t, evieTok); *u.MemberKind != "kid" {
		t.Fatal("a refused change changed the kid")
	}
}

// Live changes: a book added to the kid's collection is reachable with no
// restart; removed, unshared or deleted, it is 403 at once, through an existing
// cast token too.
func TestMembers_LiveChanges(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	col := m.collection(t, m.adamTok, "Evie's books", cradleBook)
	evie, tok := m.kid(t, m.adamTok, "Evie", col)
	if m.item(t, tok, m.lib, mistbornBook) != http.StatusForbidden {
		t.Fatal("unshared book reachable")
	}
	m.addItem(t, m.adamTok, col, mistbornBook)
	if s := m.item(t, tok, m.lib, mistbornBook); s != http.StatusOK {
		t.Fatalf("added book = %d", s)
	}
	cast := func() string {
		t.Helper()
		resp, b := m.do(t, "POST", "/api/v1/cast/token", tok, fmt.Sprintf(`{"library_id":%d,"path":%q}`, m.lib, cradleBook))
		var out struct{ Token string }
		_ = json.Unmarshal([]byte(b), &out)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("mint = %d %s", resp.StatusCode, b)
		}
		return out.Token
	}
	castStream := func(ct string) int {
		t.Helper()
		return m.status(t, "GET", mediaURL(m.lib, "stream", cradlePart, ct), "", "")
	}
	ct := cast()
	if s := castStream(ct); s != http.StatusOK && s != http.StatusPartialContent {
		t.Fatalf("cast stream = %d", s)
	}
	// Removed.
	resp, _ := m.do(t, "DELETE", fmt.Sprintf("/api/v1/me/collections/%d/items?library_id=%d&path=%s", col, m.lib, url.QueryEscape(cradleBook)), m.adamTok, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove = %d", resp.StatusCode)
	}
	if s := m.item(t, tok, m.lib, cradleBook); s != http.StatusForbidden {
		t.Errorf("removed book = %d", s)
	}
	if s := castStream(ct); s != http.StatusForbidden {
		t.Errorf("removed book through the cast token = %d", s)
	}
	// Unshared (an empty collection_ids), then deleted.
	if s := m.item(t, tok, m.lib, mistbornBook); s != http.StatusOK {
		t.Fatal("other book lost")
	}
	if s := m.status(t, "PATCH", memberURL(evie.ID, ""), m.adamTok, `{"collection_ids":[]}`); s != http.StatusOK {
		t.Fatal("unshare")
	}
	if s := m.item(t, tok, m.lib, mistbornBook); s != http.StatusForbidden {
		t.Errorf("unshared = %d", s)
	}
	if s := m.status(t, "PATCH", memberURL(evie.ID, ""), m.adamTok, fmt.Sprintf(`{"collection_ids":[%d]}`, col)); s != http.StatusOK ||
		m.item(t, tok, m.lib, mistbornBook) != http.StatusOK {
		t.Fatal("reshare")
	}
	if s := m.status(t, "DELETE", fmt.Sprintf("/api/v1/me/collections/%d", col), m.adamTok, ""); s != http.StatusNoContent {
		t.Fatal("delete collection")
	}
	if s := m.item(t, tok, m.lib, mistbornBook); s != http.StatusForbidden {
		t.Errorf("deleted collection = %d", s)
	}
}

// Ownership: adam's list, rename, pair, session, devices and delete on his
// members; bob's requests on them answer 404 on every route.
func TestMembers_Ownership(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	evie, evieTok := m.kid(t, m.adamTok, "Evie")
	devices, _ := myDevicesOf(t, m.testEnv, evieTok)

	routes := []struct{ method, path, body string }{
		{"PATCH", memberURL(evie.ID, ""), `{"name":"Evelyn"}`},
		{"POST", memberURL(evie.ID, "session"), ""},
		{"POST", memberURL(evie.ID, "pair"), ""},
		{"GET", memberURL(evie.ID, "devices"), ""},
		{"DELETE", memberURL(evie.ID, "devices/") + strconv.FormatInt(devices[0].ID, 10), ""},
		{"DELETE", memberURL(evie.ID, ""), ""},
	}
	for _, r := range routes {
		if s := m.status(t, r.method, r.path, m.bobTok, r.body); s != http.StatusNotFound {
			t.Errorf("bob %s %s = %d", r.method, r.path, s)
		}
		if s := m.status(t, r.method, strings.Replace(r.path, strconv.FormatInt(evie.ID, 10), "99999", 1), m.adamTok, r.body); s != http.StatusNotFound {
			t.Errorf("unknown %s %s = %d", r.method, r.path, s)
		}
	}
	if l := m.list(t, m.bobTok); len(l.Members) != 0 {
		t.Fatalf("bob lists adam's members: %+v", l)
	}
	if u := m.me(t, evieTok); u.ManagedBy == nil {
		t.Fatal("bob's requests changed evie")
	}

	// Adam's, in the same order.
	resp, b := m.do(t, "PATCH", memberURL(evie.ID, ""), m.adamTok, `{"name":"Evelyn"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(b, `"name":"Evelyn"`) || !strings.Contains(b, `"username":"evie"`) {
		t.Fatalf("rename = %d %s", resp.StatusCode, b)
	}
	second := m.memberSession(t, m.adamTok, evie.ID)
	resp, b = m.do(t, "POST", memberURL(evie.ID, "pair"), m.adamTok, "")
	var pair PairingPayload
	_ = json.Unmarshal([]byte(b), &pair)
	if resp.StatusCode != http.StatusOK || pair.PairingToken == "" {
		t.Fatalf("pair = %d %s", resp.StatusCode, b)
	}
	if status, body := m.exchangeToken(t, pair.PairingToken, "Evie's tablet"); status != http.StatusOK || !strings.Contains(body, `"member_kind":"kid"`) {
		t.Fatalf("exchange = %d %s", status, body)
	}
	if status, _ := m.exchangeToken(t, pair.PairingToken, "again"); status != http.StatusUnauthorized {
		t.Fatal("member pairing token reused")
	}
	resp, b = m.do(t, "GET", memberURL(evie.ID, "devices"), m.adamTok, "")
	var got []map[string]any
	if err := json.Unmarshal([]byte(b), &got); err != nil || resp.StatusCode != http.StatusOK || len(got) != 3 {
		t.Fatalf("devices = %d %s", resp.StatusCode, b)
	}
	for _, d := range got {
		if d["current"] != false || d["member_sign_ins"] != nil {
			t.Fatalf("member device = %v", d)
		}
	}
	if s := m.status(t, "DELETE", memberURL(evie.ID, "devices/")+strconv.FormatInt(devices[0].ID, 10), m.adamTok, ""); s != http.StatusNoContent {
		t.Fatalf("revoke = %d", s)
	}
	if s := m.status(t, "GET", "/api/v1/me", evieTok, ""); s != http.StatusUnauthorized {
		t.Fatal("revoked device still signed in")
	}
	if s := m.status(t, "DELETE", memberURL(evie.ID, "devices/")+strconv.FormatInt(devices[0].ID, 10), m.adamTok, ""); s != http.StatusNotFound {
		t.Fatal("revoking twice")
	}
	adamDevices, _ := myDevicesOf(t, m.testEnv, m.adamTok)
	if s := m.status(t, "DELETE", memberURL(evie.ID, "devices/")+strconv.FormatInt(adamDevices[0].ID, 10), m.adamTok, ""); s != http.StatusNotFound {
		t.Fatal("revoked the owner's own device through the member's route")
	}
	if s := m.status(t, "DELETE", memberURL(evie.ID, ""), m.adamTok, ""); s != http.StatusNoContent {
		t.Fatalf("delete = %d", s)
	}
	if s := m.status(t, "GET", "/api/v1/me", second, ""); s != http.StatusUnauthorized {
		t.Fatal("deleted member still signed in")
	}
	resp, b = m.do(t, "GET", memberURL(evie.ID, "devices"), m.adamTok, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted member's devices = %d %s", resp.StatusCode, b)
	}
	// A member with no devices answers [], not null.
	ellis := m.mustAdd(t, m.adamTok, `{"name":"Ellis","kind":"grownup","access":"all"}`)
	if _, b := m.do(t, "GET", memberURL(ellis.Member.ID, "devices"), m.adamTok, ""); strings.TrimSpace(b) != "[]" {
		t.Fatalf("no devices = %s", b)
	}
}

// Member limits (allowed): members play, save progress and bookmark; a grown-up
// sets a password, pairs, creates an API key and shares a collection.
func TestMembers_MemberLimits_Allowed(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	col := m.collection(t, m.adamTok, "Evie's books", cradleBook)
	_, kidTok := m.kid(t, m.adamTok, "Evie", col)
	_, gTok := m.grownup(t, m.adamTok, "Ellis", "all")
	for _, tok := range []string{kidTok, gTok} {
		if s := m.status(t, "PUT", bookURL(m.lib, "progress", cradleBook), tok, `{"position":10,"duration":120}`); s != http.StatusOK {
			t.Errorf("progress = %d", s)
		}
		if s := m.status(t, "POST", bookURL(m.lib, "bookmarks", cradleBook), tok, `{"position":5}`); s != http.StatusCreated {
			t.Errorf("bookmark = %d", s)
		}
		// A kid may still make its own collections.
		if s := m.status(t, "POST", "/api/v1/me/collections", tok, `{"name":"Mine"}`); s != http.StatusCreated {
			t.Errorf("own collection = %d", s)
		}
	}
	if s := m.status(t, "POST", "/api/v1/auth/password", gTok, `{"password":"ellis-password"}`); s != http.StatusNoContent {
		t.Errorf("grown-up password = %d", s)
	}
	if s := m.status(t, "POST", "/api/v1/auth/pair", gTok, ""); s != http.StatusOK {
		t.Errorf("grown-up pair = %d", s)
	}
	if s := m.status(t, "POST", "/api/v1/auth/recovery", gTok, ""); s != http.StatusCreated {
		t.Errorf("grown-up recovery = %d", s)
	}
	if s := m.status(t, "POST", "/api/v1/auth/tokens", gTok, `{"label":"x"}`); s != http.StatusOK {
		t.Errorf("grown-up key = %d", s)
	}
	if s := m.status(t, "PUT", "/api/v1/me/household", gTok, `{"share_listening":true}`); s != http.StatusOK {
		t.Errorf("grown-up house sharing = %d", s)
	}
	gcol := m.collection(t, gTok, "Ellis's", cradleBook)
	if s := m.status(t, "PUT", fmt.Sprintf("/api/v1/me/collections/%d/shares", gcol), gTok, fmt.Sprintf(`{"user_ids":[%d]}`, m.bob)); s != http.StatusOK {
		t.Errorf("grown-up share = %d", s)
	}
}

// Member limits (denied): a member adding members, a kid's password, API key,
// pair, recovery, house sharing and collection sharing (403 kid_account), and the
// admin making a member an admin (409).
func TestMembers_MemberLimits_Denied(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	_, kidTok := m.kid(t, m.adamTok, "Evie")
	ellis, gTok := m.grownup(t, m.adamTok, "Ellis", "all")
	for _, tok := range []string{kidTok, gTok} {
		if s, b := m.add(t, tok, `{"name":"X","kind":"kid","access":"all"}`); s != http.StatusForbidden || codeOf(b) != "member_cannot_manage" {
			t.Errorf("member adds = %d %s", s, b)
		}
	}
	kidCol := m.collection(t, kidTok, "Mine")
	for _, r := range []struct{ method, path, body string }{
		{"POST", "/api/v1/auth/password", `{"password":"kid-password"}`},
		{"POST", "/api/v1/auth/tokens", `{"label":"x"}`},
		{"POST", "/api/v1/auth/pair", ""},
		{"POST", "/api/v1/auth/recovery", ""},
		{"PUT", "/api/v1/me/household", `{"share_listening":true}`},
		{"PUT", fmt.Sprintf("/api/v1/me/collections/%d/shares", kidCol), fmt.Sprintf(`{"user_ids":[%d]}`, m.adam)},
	} {
		resp, b := m.do(t, r.method, r.path, kidTok, r.body)
		if resp.StatusCode != http.StatusForbidden || codeOf(b) != "kid_account" || !strings.Contains(b, "not available for a kid's account") {
			t.Errorf("kid %s %s = %d %s", r.method, r.path, resp.StatusCode, b)
		}
	}
	if u := m.me(t, kidTok); u.HasPassword || u.HasRecovery {
		t.Fatal("refused kid routes wrote")
	}
	if _, b := m.do(t, "GET", "/api/v1/me/household", kidTok, ""); strings.TrimSpace(b) != `{"share_listening":false}` {
		t.Errorf("kid's household = %s", b)
	}
	resp, b := m.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(ellis.ID, 10), m.adminTok, `{"role":"admin","password":"ellis-password"}`)
	if resp.StatusCode != http.StatusConflict || codeOf(b) != "member_not_admin" {
		t.Errorf("admin promotes a member = %d %s", resp.StatusCode, b)
	}
	if u := m.me(t, gTok); u.Role != "user" {
		t.Fatal("member became an admin")
	}
}

// Sign-ins: a member's session keeps working after adam's phone is signed out
// without also_members; with it, the member tokens made on that phone answer 401
// and their tokens elsewhere stay 200.
func TestMembers_SignIns(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ctx := context.Background()
	evie, evieHere := m.kid(t, m.adamTok, "Evie")
	ellis, ellisHere := m.grownup(t, m.adamTok, "Ellis", "all")
	evieElsewhere, err := m.auth.IssueToken(ctx, evie.ID, auth.KindSession, "Evie's tablet", 0)
	if err != nil {
		t.Fatal(err)
	}
	devices, _ := myDevicesOf(t, m.testEnv, m.adamTok)
	phone := devices[0]
	resp, b := m.do(t, "GET", "/api/v1/me/devices", m.adamTok, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(b, fmt.Sprintf(`"member_sign_ins":[{"id":%d,"name":"Evie"},{"id":%d,"name":"Ellis"}]`, evie.ID, ellis.ID)) {
		t.Fatalf("devices = %s", b)
	}
	// Bob's devices never name adam's members.
	if _, b := m.do(t, "GET", "/api/v1/me/devices", m.bobTok, ""); !strings.Contains(b, `"member_sign_ins":[]`) {
		t.Fatalf("bob's devices = %s", b)
	}

	// A second adam device, signed out without also_members (any other value is off).
	tablet, _ := m.auth.IssueToken(ctx, m.adam, auth.KindSession, "adam's tablet", 0)
	tdev, _ := myDevicesOf(t, m.testEnv, tablet)
	var tabletID int64
	for _, d := range tdev {
		if d.Name == "adam's tablet" {
			tabletID = d.ID
		}
	}
	evieTablet := m.memberSession(t, tablet, evie.ID)
	resp, b = m.do(t, "DELETE", fmt.Sprintf("/api/v1/me/devices/%d?also_members=yes", tabletID), m.adamTok, "")
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(b) != `{"current":false,"members_signed_out":0}` {
		t.Fatalf("revoke without = %d %s", resp.StatusCode, b)
	}
	if s := m.status(t, "GET", "/api/v1/me", evieTablet, ""); s != http.StatusOK {
		t.Fatal("member signed out without also_members")
	}

	// The phone, with also_members (bob's call on it is a 404 and does nothing).
	if s := m.status(t, "DELETE", fmt.Sprintf("/api/v1/me/devices/%d?also_members=1", phone.ID), m.bobTok, ""); s != http.StatusNotFound {
		t.Fatalf("bob = %d", s)
	}
	resp, b = m.do(t, "DELETE", fmt.Sprintf("/api/v1/me/devices/%d?also_members=1", phone.ID), m.adamTok, "")
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(b) != `{"current":true,"members_signed_out":2}` {
		t.Fatalf("revoke with = %d %s", resp.StatusCode, b)
	}
	for _, tok := range []string{evieHere, ellisHere, m.adamTok} {
		if s := m.status(t, "GET", "/api/v1/me", tok, ""); s != http.StatusUnauthorized {
			t.Error("a token made on the phone survived")
		}
	}
	for _, tok := range []string{evieElsewhere, evieTablet} {
		if s := m.status(t, "GET", "/api/v1/me", tok, ""); s != http.StatusOK {
			t.Error("a member token made elsewhere was signed out")
		}
	}
	// Each member signed out there is audited as on the member route: the owner
	// as actor, the member as target; the sign-out without members audits none.
	_, b = m.do(t, "GET", "/api/v1/admin/audit?area=member&limit=50", m.adminTok, "")
	var out struct{ Events []catalog.AuditEvent }
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatal(err)
	}
	var revoked []string
	for _, e := range out.Events {
		if e.Action == "member.device_revoke" {
			if e.ActorID == nil || *e.ActorID != m.adam || e.Details["device_id"] != float64(phone.ID) {
				t.Errorf("event = %+v", e)
			}
			revoked = append(revoked, e.Target)
		}
	}
	if slices.Sort(revoked); !slices.Equal(revoked, []string{"ellis", "evie"}) {
		t.Errorf("audited sign-outs = %v", revoked)
	}
}

// Visibility: adam sees evie in his share targets and can share with her; ellis
// appears like any account. Bob doesn't see evie and can't share with her by id;
// evie never appears in household listening and can't read it.
func TestMembers_Visibility(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	evie, evieTok := m.kid(t, m.adamTok, "Evie")
	ellis, ellisTok := m.grownup(t, m.adamTok, "Ellis", "all")
	targets := func(tok string) []string {
		t.Helper()
		_, b := m.do(t, "GET", "/api/v1/me/share-targets", tok, "")
		var out struct{ Users []catalog.CollectionUser }
		_ = json.Unmarshal([]byte(b), &out)
		names := []string{}
		for _, u := range out.Users {
			names = append(names, u.Username)
		}
		return names
	}
	if got := targets(m.adamTok); !slices.Contains(got, "evie") || !slices.Contains(got, "ellis") {
		t.Errorf("adam's targets = %v", got)
	}
	if got := targets(m.bobTok); slices.Contains(got, "evie") || !slices.Contains(got, "ellis") {
		t.Errorf("bob's targets = %v", got)
	}
	adamCol := m.collection(t, m.adamTok, "Family", cradleBook)
	if s := m.status(t, "PUT", fmt.Sprintf("/api/v1/me/collections/%d/shares", adamCol), m.adamTok, fmt.Sprintf(`{"user_ids":[%d,%d]}`, evie.ID, ellis.ID)); s != http.StatusOK {
		t.Errorf("adam shares with evie = %d", s)
	}
	bobCol := m.collection(t, m.bobTok, "Bob's", cradleBook)
	if s := m.status(t, "PUT", fmt.Sprintf("/api/v1/me/collections/%d/shares", bobCol), m.bobTok, fmt.Sprintf(`{"user_ids":[%d]}`, evie.ID)); s != http.StatusBadRequest {
		t.Errorf("bob shares with evie = %d", s)
	}
	if s := m.status(t, "PUT", fmt.Sprintf("/api/v1/me/collections/%d/shares", bobCol), m.bobTok, fmt.Sprintf(`{"user_ids":[%d]}`, ellis.ID)); s != http.StatusOK {
		t.Errorf("bob shares with ellis = %d", s)
	}

	// House listening: adam, ellis and bob share; evie shared as a grown-up and
	// was then switched to a kid, which clears her choice, so she never shows and
	// reads nothing.
	kind := func(k string) {
		t.Helper()
		if s := m.status(t, "PATCH", memberURL(evie.ID, ""), m.adamTok, `{"kind":"`+k+`"}`); s != http.StatusOK {
			t.Fatalf("switch evie to %s = %d", k, s)
		}
	}
	kind("grownup")
	for _, tok := range []string{m.adamTok, ellisTok, m.bobTok, evieTok} {
		if s := m.status(t, "PUT", "/api/v1/me/household", tok, `{"share_listening":true}`); s != http.StatusOK {
			t.Fatalf("share = %d", s)
		}
	}
	kind("kid")
	for _, tok := range []string{evieTok, ellisTok} {
		for _, pos := range []string{"10", "20"} {
			m.status(t, "PUT", bookURL(m.lib, "progress", cradleBook), tok, `{"position":`+pos+`,"duration":120}`)
		}
	}
	_, b := m.do(t, "GET", "/api/v1/household/listening", m.adamTok, "")
	var house houseListening
	_ = json.Unmarshal([]byte(b), &house)
	if got := usernames(house.Listening); !slices.Equal(got, []string{"ellis"}) {
		t.Errorf("adam's house = %v", got)
	}
	_, b = m.do(t, "GET", "/api/v1/household/listening", evieTok, "")
	if strings.TrimSpace(b) != `{"sharing":false,"listening":[]}` {
		t.Errorf("evie's house = %s", b)
	}
	if _, b := m.do(t, "GET", "/api/v1/me/household", evieTok, ""); strings.TrimSpace(b) != `{"share_listening":false}` {
		t.Errorf("evie's setting = %s", b)
	}
	// A grown-up again, she starts from off and opts in again.
	kind("grownup")
	if _, b := m.do(t, "GET", "/api/v1/me/household", evieTok, ""); strings.TrimSpace(b) != `{"share_listening":false}` {
		t.Errorf("evie's setting as a grown-up again = %s", b)
	}
}

// Cast: a cast token minted by evie's session plays her collection book;
// minting for a book outside her access is 403.
func TestMembers_Cast(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	col := m.collection(t, m.adamTok, "Evie's books", cradleBook)
	_, tok := m.kid(t, m.adamTok, "Evie", col)
	resp, b := m.do(t, "POST", "/api/v1/cast/token", tok, fmt.Sprintf(`{"library_id":%d,"path":%q}`, m.lib, cradleBook))
	var out struct{ Token string }
	_ = json.Unmarshal([]byte(b), &out)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint = %d %s", resp.StatusCode, b)
	}
	if s := m.status(t, "GET", mediaURL(m.lib, "stream", cradlePart, out.Token), "", ""); s != http.StatusOK && s != http.StatusPartialContent {
		t.Fatalf("cast stream = %d", s)
	}
	for _, p := range []string{mistbornBook, cradleTwo} {
		if s := m.status(t, "POST", "/api/v1/cast/token", tok, fmt.Sprintf(`{"library_id":%d,"path":%q}`, m.lib, p)); s != http.StatusForbidden {
			t.Errorf("mint %s = %d", p, s)
		}
	}
}

// Deletion: deleting adam deletes his members by default, a kept member stays
// with null member columns and only their own grants; deleting a member removes
// their state; the last-admin rule still holds.
func TestMembers_Deletion(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ctx := context.Background()
	evie, evieTok := m.kid(t, m.adamTok, "Evie")
	ellis, ellisTok := m.grownup(t, m.adamTok, "Ellis", "all")
	if err := m.cat.GrantWholeLibrary(ctx, ellis.ID, m.other); err != nil {
		t.Fatal(err)
	}
	if m.item(t, ellisTok, m.lib, cradleBook) != http.StatusOK {
		t.Fatal("derived access missing")
	}
	// A member's own state, then deleting the member removes it.
	m.status(t, "PUT", bookURL(m.lib, "progress", cradleBook), ellisTok, `{"position":10,"duration":120}`)
	kit, kitTok := m.grownup(t, m.adamTok, "Kit", "all")
	m.status(t, "PUT", bookURL(m.lib, "progress", cradleBook), kitTok, `{"position":10,"duration":120}`)
	if s := m.status(t, "DELETE", memberURL(kit.ID, ""), m.adamTok, ""); s != http.StatusNoContent {
		t.Fatal("delete member")
	}
	var rows int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM progress WHERE user_id = ?`, kit.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("deleted member's progress = %d %v", rows, err)
	}

	userURL := "/api/v1/admin/users/" + strconv.FormatInt(m.adam, 10)
	for _, bad := range []string{"?keep_members=x", "?keep_members=" + strconv.FormatInt(m.bob, 10)} {
		resp, b := m.do(t, "DELETE", userURL+bad, m.adminTok, "")
		if resp.StatusCode != http.StatusBadRequest || codeOf(b) != "invalid_keep_members" {
			t.Errorf("%s = %d %s", bad, resp.StatusCode, b)
		}
	}
	if s := m.status(t, "DELETE", userURL+"?keep_members="+strconv.FormatInt(ellis.ID, 10), m.adminTok, ""); s != http.StatusNoContent {
		t.Fatalf("delete adam = %d", s)
	}
	if s := m.status(t, "GET", "/api/v1/me", evieTok, ""); s != http.StatusUnauthorized {
		t.Error("unkept member survived")
	}
	u := m.me(t, ellisTok)
	if u.ManagedBy != nil || u.MemberKind != nil {
		t.Fatalf("kept member = %+v", u)
	}
	if m.item(t, ellisTok, m.lib, cradleBook) != http.StatusForbidden || m.item(t, ellisTok, m.other, cradleBook) != http.StatusOK {
		t.Fatal("kept member doesn't have only their own grants")
	}
	var progress int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM progress WHERE user_id = ?`, ellis.ID).Scan(&progress); err != nil || progress != 1 {
		t.Fatalf("kept member's progress = %d %v", progress, err)
	}
	// The last-admin rule still holds for an owner: the only admin, with a
	// member, can't be deleted, and the member stays.
	_, rootKidTok := m.kid(t, m.adminTok, "Rory")
	if _, err := m.auth.DeleteUser(ctx, m.adminID, nil); !errors.Is(err, auth.ErrLastAdmin) {
		t.Errorf("the last admin = %v", err)
	}
	if s := m.status(t, "GET", "/api/v1/me", rootKidTok, ""); s != http.StatusOK {
		t.Error("a refused delete took the admin's member")
	}
	_ = evie
}

// Admin: the user list and detail carry the member fields; PATCH assigns, makes
// independent and switches; the refusals are coded.
func TestMembers_AdminFields(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	col := m.collection(t, m.adamTok, "Evie's books", cradleBook)
	evie, _ := m.kid(t, m.adamTok, "Evie", col)
	_, b := m.do(t, "GET", "/api/v1/admin/users", m.adminTok, "")
	var list struct{ Users []map[string]any }
	_ = json.Unmarshal([]byte(b), &list)
	byName := map[string]map[string]any{}
	for _, u := range list.Users {
		byName[u["username"].(string)] = u
	}
	if u := byName["evie"]; u["member_kind"] != "kid" || u["member_access"] != "collections" || u["display_name"] != "Evie" ||
		u["managed_by"].(map[string]any)["username"] != "adam" {
		t.Fatalf("evie = %v", u)
	}
	if u := byName["adam"]; u["members_count"] != float64(1) || u["can_add_members"] != true || u["managed_by"] != nil || u["member_access"] != nil {
		t.Fatalf("adam = %v", u)
	}
	_, b = m.do(t, "GET", "/api/v1/admin/users/"+strconv.FormatInt(evie.ID, 10), m.adminTok, "")
	var detail struct {
		AccessibleLibraries []catalog.Library          `json:"accessible_libraries"`
		Members             []adminMember              `json:"members"`
		MemberCollections   []catalog.MemberCollection `json:"member_collections"`
	}
	_ = json.Unmarshal([]byte(b), &detail)
	if len(detail.AccessibleLibraries) != 1 || len(detail.Members) != 0 || len(detail.MemberCollections) != 1 || detail.MemberCollections[0].Items != 1 {
		t.Fatalf("evie's detail = %s", b)
	}
	_, b = m.do(t, "GET", "/api/v1/admin/users/"+strconv.FormatInt(m.adam, 10), m.adminTok, "")
	_ = json.Unmarshal([]byte(b), &detail)
	if len(detail.Members) != 1 || detail.Members[0].Name != "Evie" || detail.Members[0].Kind != "kid" || len(detail.MemberCollections) != 0 {
		t.Fatalf("adam's detail = %s", b)
	}

	patch := func(id int64, body string) (int, string) {
		resp, b := m.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(id, 10), m.adminTok, body)
		return resp.StatusCode, b
	}
	// Assign bob to adam as a grown-up on everything; then make him independent.
	for body, code := range map[string]string{
		fmt.Sprintf(`{"managed_by":%d}`, m.adam):                                            "member_fields_required",
		`{"member_kind":"kid","member_access":"all"}`:                                       "member_fields_required",
		`{"managed_by":null,"member_kind":"kid"}`:                                           "member_fields_required",
		fmt.Sprintf(`{"managed_by":%d,"member_kind":"kid","member_access":"all"}`, evie.ID): "owner_is_member",
		fmt.Sprintf(`{"managed_by":%d,"member_kind":"kid","member_access":"all"}`, m.bob):   "self_managed",
	} {
		if s, b := patch(m.bob, body); codeOf(b) != code || (s != http.StatusBadRequest && s != http.StatusConflict) {
			t.Errorf("%s = %d %s", body, s, b)
		}
	}
	if s, b := patch(m.adam, fmt.Sprintf(`{"managed_by":%d,"member_kind":"kid","member_access":"all"}`, m.bob)); s != http.StatusConflict || codeOf(b) != "has_members" {
		t.Errorf("an owner assigned = %d %s", s, b)
	}
	if s, b := patch(m.adminID, fmt.Sprintf(`{"managed_by":%d,"member_kind":"kid","member_access":"all"}`, m.adam)); s != http.StatusConflict || codeOf(b) != "member_not_admin" {
		t.Errorf("an admin assigned = %d %s", s, b)
	}
	if s, b := patch(m.bob, `{"managed_by":99999,"member_kind":"kid","member_access":"all"}`); s != http.StatusNotFound {
		t.Errorf("unknown owner = %d %s", s, b)
	}
	if s, b := patch(m.bob, `{"managed_by":"x"}`); s != http.StatusBadRequest {
		t.Errorf("bad owner = %d %s", s, b)
	}
	s, b := patch(m.bob, fmt.Sprintf(`{"managed_by":%d,"member_kind":"grownup","member_access":"all"}`, m.adam))
	if s != http.StatusOK || !strings.Contains(b, `"member_access":"all"`) || !strings.Contains(b, `"managed_by":{"id":`) {
		t.Fatalf("assign = %d %s", s, b)
	}
	// Bob keeps his own grant beside the derived one.
	if m.item(t, m.bobTok, m.lib, cradleBook) != http.StatusOK {
		t.Fatal("assigned member lost access")
	}
	if s, b := patch(m.bob, `{"managed_by":null}`); s != http.StatusOK || !strings.Contains(b, `"managed_by":null`) {
		t.Fatalf("make independent = %d %s", s, b)
	}
	if s, b := patch(m.bob, `{"managed_by":null}`); s != http.StatusOK {
		t.Fatalf("make independent again (no-op) = %d %s", s, b)
	}
}

// Admin fields (a member's owner): a member's detail carries what their owner
// reaches (owner_libraries, owner_is_admin) and names the owner with their
// display_name in managed_by. Denied: an ordinary person's detail carries no
// owner facts, and the member's own /me never carries the owner's display_name.
func TestMembers_AdminOwnerFacts(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ellis, ellisTok := m.grownup(t, m.adamTok, "Ellis", "all")
	adminKid := m.mustAdd(t, m.adminTok, `{"name":"Ada","kind":"kid","access":"all"}`)
	detailOf := func(id int64) map[string]json.RawMessage {
		t.Helper()
		resp, b := m.do(t, "GET", "/api/v1/admin/users/"+strconv.FormatInt(id, 10), m.adminTok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("detail %d = %d %s", id, resp.StatusCode, b)
		}
		var out map[string]json.RawMessage
		_ = json.Unmarshal([]byte(b), &out)
		return out
	}
	d := detailOf(ellis.ID)
	var libs []catalog.Library
	_ = json.Unmarshal(d["owner_libraries"], &libs)
	if len(libs) != 1 || libs[0].ID != m.lib || string(d["owner_is_admin"]) != "false" ||
		!strings.Contains(string(d["user"]), `"managed_by":{"id":`+strconv.FormatInt(m.adam, 10)+`,"username":"adam","display_name":""}`) {
		t.Fatalf("ellis's detail = %s %s %s", d["owner_libraries"], d["owner_is_admin"], d["user"])
	}
	d = detailOf(adminKid.Member.ID)
	_ = json.Unmarshal(d["owner_libraries"], &libs)
	if string(d["owner_is_admin"]) != "true" || len(libs) != 2 {
		t.Fatalf("an admin's member = %s %s", d["owner_libraries"], d["owner_is_admin"])
	}
	if d := detailOf(m.adam); d["owner_libraries"] != nil || d["owner_is_admin"] != nil {
		t.Fatalf("an owner's detail carries owner facts: %s %s", d["owner_libraries"], d["owner_is_admin"])
	}
	if _, b := m.do(t, "GET", "/api/v1/me", ellisTok, ""); !strings.Contains(b, `"managed_by":{"id":`+strconv.FormatInt(m.adam, 10)+`,"username":"adam"}`) {
		t.Fatalf("ellis's /me = %s", b)
	}
}

// Admin fields (demo): a demo account is never assigned to anyone (409
// member_is_demo, nothing changed), while an ordinary account is.
func TestMembers_AdminAssignDemo(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	demo, demoTok := newUserWithSession(t, m.testEnv, auth.DemoUsernamePrefix+"x", true)
	body := fmt.Sprintf(`{"managed_by":%d,"member_kind":"grownup","member_access":"all"}`, m.adam)
	resp, b := m.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(demo, 10), m.adminTok, body)
	if resp.StatusCode != http.StatusConflict || codeOf(b) != "member_is_demo" {
		t.Fatalf("demo assigned = %d %s", resp.StatusCode, b)
	}
	if u := m.me(t, demoTok); u.ManagedBy != nil || u.MemberKind != nil {
		t.Fatalf("refused assign changed the demo account: %+v", u)
	}
	if l := m.list(t, m.adamTok); len(l.Members) != 0 {
		t.Fatalf("adam lists the demo account: %+v", l.Members)
	}
	// Allowed: bob, an ordinary account, in the same body.
	resp, b = m.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(m.bob, 10), m.adminTok, body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(b, `"member_kind":"grownup"`) {
		t.Fatalf("bob assigned = %d %s", resp.StatusCode, b)
	}
}

// Admin fields (one change): PATCH /admin/users/{id} checks the whole body
// before writing, so a refused field leaves the password, the household and the
// role as they were; a password beside assigning a kid is 400 kid_no_password.
func TestMembers_AdminPatchAtomic(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ctx := context.Background()
	evie, _ := m.kid(t, m.adamTok, "Evie")
	patch := func(body string) (int, string) {
		resp, b := m.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(m.bob, 10), m.adminTok, body)
		return resp.StatusCode, b
	}
	unchanged := func(what string) {
		t.Helper()
		if err := m.auth.CheckPassword(ctx, m.bob, "bob-password"); err != nil {
			t.Errorf("%s: bob's password changed: %v", what, err)
		}
		u, err := m.auth.GetUser(ctx, m.bob)
		if err != nil || u.IsMember() || u.Role != auth.RoleUser || u.Disabled || !u.CanAddMembers {
			t.Errorf("%s: bob changed: %+v %v", what, u, err)
		}
	}
	for _, c := range []struct {
		body   string
		status int
		code   string
	}{
		// A refused assign after a password.
		{fmt.Sprintf(`{"password":"new-password-1","managed_by":%d,"member_kind":"grownup","member_access":"all"}`, evie.ID),
			http.StatusConflict, "owner_is_member"},
		// A password beside assigning a kid (it would be set, then cleared).
		{fmt.Sprintf(`{"password":"new-password-1","managed_by":%d,"member_kind":"kid","member_access":"collections"}`, m.adam),
			http.StatusBadRequest, "kid_no_password"},
		// A refused role after an allowed password and assign.
		{fmt.Sprintf(`{"password":"new-password-1","managed_by":%d,"member_kind":"grownup","member_access":"all","role":"admin"}`, m.adam),
			http.StatusConflict, "member_not_admin"},
		// The other fields wait for the refusal too.
		{fmt.Sprintf(`{"disabled":true,"can_add_members":false,"managed_by":%d,"member_kind":"grownup","member_access":"all"}`, evie.ID),
			http.StatusConflict, "owner_is_member"},
		{`{"role":"admin","disabled":true,"can_add_members":false,"managed_by":null,"password":"short"}`,
			http.StatusBadRequest, "password_too_short"},
	} {
		if s, b := patch(c.body); s != c.status || codeOf(b) != c.code {
			t.Errorf("%s = %d %s, want %d %s", c.body, s, b, c.status, c.code)
		}
		unchanged(c.body)
	}
	// Allowed: the same fields without the refused one apply together.
	s, b := patch(fmt.Sprintf(`{"password":"new-password-1","managed_by":%d,"member_kind":"grownup","member_access":"all"}`, m.adam))
	if s != http.StatusOK || !strings.Contains(b, `"member_kind":"grownup"`) {
		t.Fatalf("password and assign = %d %s", s, b)
	}
	if err := m.auth.CheckPassword(ctx, m.bob, "new-password-1"); err != nil {
		t.Fatalf("password not set: %v", err)
	}
	// Assigning a kid with no password in the body clears the one bob has.
	s, b = patch(fmt.Sprintf(`{"managed_by":%d,"member_kind":"kid","member_access":"collections"}`, m.adam))
	if s != http.StatusOK || !strings.Contains(b, `"has_password":false`) {
		t.Fatalf("assign a kid = %d %s", s, b)
	}
}

// Admin fields (a kid's password): a kid holds no password, so the admin's
// PATCH /admin/users/{id} can't set one on a kid (400 kid_no_password, the hash
// stays empty), nor beside assigning someone as a kid, though clearing one
// beside it is fine; a grown-up member and an ordinary account are given one,
// and so is a kid in the same change that leaves them no kid.
func TestMembers_AdminPasswordOnKid(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ctx := context.Background()
	evie, _ := m.kid(t, m.adamTok, "Evie")
	ellis, _ := m.grownup(t, m.adamTok, "Ellis", "all")
	patch := func(id int64, body string) (int, string) {
		resp, b := m.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(id, 10), m.adminTok, body)
		return resp.StatusCode, b
	}
	noPassword := func(id int64, what string) {
		t.Helper()
		u, err := m.auth.GetUser(ctx, id)
		if err != nil || u.HasPassword {
			t.Errorf("%s: has a password: %+v %v", what, u, err)
		}
		var hash string
		if err := m.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, id).Scan(&hash); err != nil || hash != "" {
			t.Errorf("%s: hash = %q %v", what, hash, err)
		}
	}
	// Denied: a password on the kid, alone, beside other fields, or beside
	// assigning them (again) as a kid.
	for _, body := range []string{
		`{"password":"kid-password-1"}`,
		`{"password":"kid-password-1","disabled":false,"can_add_members":false}`,
		fmt.Sprintf(`{"password":"kid-password-1","managed_by":%d,"member_kind":"kid","member_access":"all"}`, m.bob),
	} {
		if s, b := patch(evie.ID, body); s != http.StatusBadRequest || codeOf(b) != "kid_no_password" {
			t.Errorf("kid %s = %d %s, want 400 kid_no_password", body, s, b)
		}
		noPassword(evie.ID, body)
		if u, err := m.auth.GetUser(ctx, evie.ID); err != nil || !u.IsKid() || u.ManagedBy.ID != m.adam || !u.CanAddMembers {
			t.Errorf("%s: evie changed: %+v %v", body, u, err)
		}
	}
	// The self-service route never reaches it (403 kid_account), and the
	// service refuses it under that too.
	if err := m.auth.SetPassword(ctx, evie.ID, "kid-password-1"); !errors.Is(err, auth.ErrKidNoPassword) {
		t.Errorf("SetPassword on a kid = %v, want ErrKidNoPassword", err)
	}
	noPassword(evie.ID, "SetPassword")
	// Allowed: clearing a kid's (absent) password.
	if s, b := patch(evie.ID, `{"password":""}`); s != http.StatusOK || !strings.Contains(b, `"has_password":false`) {
		t.Errorf("clear a kid's password = %d %s", s, b)
	}
	// Allowed: a grown-up member and an ordinary account are given one.
	for _, id := range []int64{ellis.ID, m.bob} {
		if s, b := patch(id, `{"password":"new-password-1"}`); s != http.StatusOK || !strings.Contains(b, `"has_password":true`) {
			t.Errorf("password for %d = %d %s", id, s, b)
		}
		if err := m.auth.CheckPassword(ctx, id, "new-password-1"); err != nil {
			t.Errorf("password for %d not set: %v", id, err)
		}
	}
	// Clearing bob's beside assigning him as a kid is fine.
	if s, b := patch(m.bob, fmt.Sprintf(`{"password":"","managed_by":%d,"member_kind":"kid","member_access":"collections"}`, m.adam)); s != http.StatusOK || !strings.Contains(b, `"member_kind":"kid"`) {
		t.Errorf("clear and assign a kid = %d %s", s, b)
	}
	noPassword(m.bob, "clear and assign a kid")
	// A kid made a grown-up in the same change may be given one.
	if s, b := patch(evie.ID, fmt.Sprintf(`{"password":"new-password-2","managed_by":%d,"member_kind":"grownup","member_access":"all"}`, m.adam)); s != http.StatusOK || !strings.Contains(b, `"has_password":true`) {
		t.Errorf("password beside kid to grown-up = %d %s", s, b)
	}
}

// Not found (the member routes): someone else's or no member is "member not
// found", a device that isn't the member's "device not found".
func TestMembers_NotFoundAnswers(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	evie, evieTok := m.kid(t, m.adamTok, "Evie")
	devices, _ := myDevicesOf(t, m.testEnv, evieTok)
	want := func(method, path, tok, msg string) {
		t.Helper()
		resp, b := m.do(t, method, path, tok, "")
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(b, `"error":"`+msg+`"`) {
			t.Errorf("%s %s = %d %s, want 404 %q", method, path, resp.StatusCode, b, msg)
		}
	}
	device := memberURL(evie.ID, "devices/") + strconv.FormatInt(devices[0].ID, 10)
	want("DELETE", memberURL(evie.ID, "devices/99999"), m.adamTok, "device not found")
	want("DELETE", device, m.bobTok, "member not found")
	want("DELETE", memberURL(99999, "devices/")+strconv.FormatInt(devices[0].ID, 10), m.adamTok, "member not found")
	want("POST", memberURL(99999, "session"), m.adamTok, "member not found")
	want("GET", memberURL(evie.ID, "devices"), m.bobTok, "member not found")
	// Allowed: adam signs evie's device out.
	if s := m.status(t, "DELETE", device, m.adamTok, ""); s != http.StatusNoContent {
		t.Fatalf("revoke = %d", s)
	}
	want("DELETE", device, m.adamTok, "device not found")
}

// The server reports the capability, and the settings envelope carries the
// household section with the cap's default and range as read-only facts.
func TestMembers_CapabilityAndSettings(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	if !serverCapabilities(t, m.testEnv)["household_members"] {
		t.Fatal("no household_members capability")
	}
	_, b := m.do(t, "GET", "/api/v1/admin/settings", m.adminTok, "")
	if !strings.Contains(b, `"household":{"max_members":6,"max_members_default":6,"max_members_max":50,"max_members_min":1,"members_enabled":true}`) {
		t.Fatalf("settings = %s", b)
	}
	if resp, b := m.do(t, "PATCH", "/api/v1/admin/settings", m.adminTok, `{"household":{"max_members":0}}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("max 0 = %d %s", resp.StatusCode, b)
	}
}

// Audit: each member route writes its event, the owner as actor and the member
// as target (audit_coverage_test only covers /admin routes).
func TestMembers_Audit(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	evie, evieTok := m.kid(t, m.adamTok, "Evie")
	devices, _ := myDevicesOf(t, m.testEnv, evieTok)
	for _, r := range []struct{ method, path, body string }{
		{"PATCH", memberURL(evie.ID, ""), `{"kind":"grownup","collection_ids":[]}`},
		{"POST", memberURL(evie.ID, "session"), ""},
		{"POST", memberURL(evie.ID, "pair"), ""},
		{"DELETE", memberURL(evie.ID, "devices/") + strconv.FormatInt(devices[0].ID, 10), ""},
		{"DELETE", memberURL(evie.ID, ""), ""},
	} {
		if s := m.status(t, r.method, r.path, m.adamTok, r.body); s >= 300 {
			t.Fatalf("%s %s = %d", r.method, r.path, s)
		}
	}
	_, b := m.do(t, "GET", "/api/v1/admin/audit?area=member&limit=50", m.adminTok, "")
	var out struct{ Events []catalog.AuditEvent }
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]catalog.AuditEvent{}
	for _, e := range out.Events {
		if e.ActorID == nil || *e.ActorID != m.adam || e.ActorName != "adam" || e.Target != "evie" || e.Details["member"] != "evie" {
			t.Errorf("event = %+v", e)
		}
		got[e.Action] = e
	}
	for _, action := range []string{"member.create", "member.update", "member.session", "member.pair", "member.device_revoke", "member.delete"} {
		if _, ok := got[action]; !ok {
			t.Errorf("no %s event", action)
		}
	}
	if d := got["member.create"].Details; d["kind"] != "kid" || d["access"] != "collections" || d["sign_in_here"] != true {
		t.Errorf("create details = %v", d)
	}
	if d := got["member.update"].Details; d["kind"] != "grownup" || d["collections"] != float64(0) {
		t.Errorf("update details = %v", d)
	}
	if d := got["member.device_revoke"].Details; d["device_id"] != float64(devices[0].ID) {
		t.Errorf("device details = %v", d)
	}
	// The admin's own member changes and deletions say what they did.
	ellis := m.mustAdd(t, m.adamTok, `{"name":"Ellis","kind":"grownup","access":"all"}`)
	m.status(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(m.adam, 10), m.adminTok, `{"can_add_members":false}`)
	m.status(t, "DELETE", "/api/v1/admin/users/"+strconv.FormatInt(m.adam, 10)+"?keep_members="+strconv.FormatInt(ellis.Member.ID, 10), m.adminTok, "")
	_, b = m.do(t, "GET", "/api/v1/admin/audit?area=user&limit=50", m.adminTok, "")
	_ = json.Unmarshal([]byte(b), &out)
	var sawUpdate, sawDelete bool
	for _, e := range out.Events {
		switch e.Action {
		case "user.update":
			sawUpdate = e.Details["can_add_members"] == false
		case "user.delete":
			kept, _ := e.Details["members_kept"].([]any)
			sawDelete = e.Details["members_deleted"] == float64(0) && len(kept) == 1 && kept[0] == "ellis"
		}
	}
	if !sawUpdate || !sawDelete {
		t.Fatalf("user events = %s", b)
	}
}

// Kids and collection shares: becoming a kid takes away the collections shared
// between the kid and anyone but their owner, both ways, so a sharer is never
// left with a kid in the list they are shown that the server refuses back.
// Allowed: the owner's shares, both ways, stay.
func TestMembers_KidSharesOnlyWithOwner(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ellis, ellisTok := m.grownup(t, m.adamTok, "Ellis", "all")
	shares := func(tok string, col int64, ids ...int64) (int, string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"user_ids": append([]int64{}, ids...)})
		resp, body := m.do(t, "PUT", fmt.Sprintf("/api/v1/me/collections/%d/shares", col), tok, string(b))
		return resp.StatusCode, body
	}
	names := func(tok string) []string {
		t.Helper()
		_, b := m.do(t, "GET", "/api/v1/me/collections", tok, "")
		var out struct{ Collections []wireCollection }
		_ = json.Unmarshal([]byte(b), &out)
		got := []string{}
		for _, c := range out.Collections {
			got = append(got, c.Name)
		}
		return got
	}
	family := m.collection(t, m.adamTok, "Family", cradleBook)
	bobs := m.collection(t, m.bobTok, "Bob's", cradleBook)
	own := m.collection(t, ellisTok, "Ellis's", cradleBook)
	for _, s := range []struct {
		tok string
		col int64
		ids []int64
	}{{m.adamTok, family, []int64{ellis.ID}}, {m.bobTok, bobs, []int64{ellis.ID}}, {ellisTok, own, []int64{m.adam, m.bob}}} {
		if status, b := shares(s.tok, s.col, s.ids...); status != http.StatusOK {
			t.Fatalf("share %d = %d %s", s.col, status, b)
		}
	}
	if s := m.status(t, "PATCH", memberURL(ellis.ID, ""), m.adamTok, `{"kind":"kid"}`); s != http.StatusOK {
		t.Fatalf("to kid = %d", s)
	}

	if got := names(ellisTok); !slices.Contains(got, "Family") || slices.Contains(got, "Bob's") {
		t.Errorf("the kid's collections = %v", got)
	}
	if got := names(m.adamTok); !slices.Contains(got, "Ellis's") {
		t.Errorf("adam lost the kid's collection: %v", got)
	}
	if got := names(m.bobTok); slices.Contains(got, "Ellis's") {
		t.Errorf("bob still sees the kid's collection: %v", got)
	}
	// Bob's list no longer names her, so the list he is shown saves again;
	// sharing with the kid stays refused.
	_, b := m.do(t, "GET", fmt.Sprintf("/api/v1/me/collections/%d", bobs), m.bobTok, "")
	var detail struct{ Collection wireCollection }
	_ = json.Unmarshal([]byte(b), &detail)
	if strings.TrimSpace(string(detail.Collection.SharedWith)) != "[]" {
		t.Errorf("bob's shared_with = %s", detail.Collection.SharedWith)
	}
	if s, b := shares(m.bobTok, bobs, m.adam); s != http.StatusOK {
		t.Errorf("bob saves his list = %d %s", s, b)
	}
	if s, _ := shares(m.bobTok, bobs, ellis.ID); s != http.StatusBadRequest {
		t.Errorf("bob shares with the kid = %d", s)
	}
}

// Moving a member to another household (the admin's "Assign to someone") signs
// them out of what their old owner set up for them: the sessions made on the
// old owner's devices and the open pairing codes, which would otherwise carry
// the new owner's access to the old owner's phone. Allowed: their own devices,
// and an assignment to the same owner, keep working.
func TestMembers_AdminMoveLeavesOldOwnersDevices(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	evie, onAdamsPhone := m.kid(t, m.adamTok, "Evie")
	pairing := func() string {
		t.Helper()
		resp, b := m.do(t, "POST", memberURL(evie.ID, "pair"), m.adamTok, "")
		var pair PairingPayload
		_ = json.Unmarshal([]byte(b), &pair)
		if resp.StatusCode != http.StatusOK || pair.PairingToken == "" {
			t.Fatalf("pair = %d %s", resp.StatusCode, b)
		}
		return pair.PairingToken
	}
	status, b := m.exchangeToken(t, pairing(), "Evie's tablet")
	var env struct{ Token string }
	_ = json.Unmarshal([]byte(b), &env)
	if status != http.StatusOK || env.Token == "" {
		t.Fatalf("exchange = %d %s", status, b)
	}
	tablet := env.Token
	assign := func(owner int64, kind, access string) {
		t.Helper()
		resp, b := m.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(evie.ID, 10), m.adminTok,
			fmt.Sprintf(`{"managed_by":%d,"member_kind":%q,"member_access":%q}`, owner, kind, access))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("assign = %d %s", resp.StatusCode, b)
		}
	}
	assign(m.adam, "kid", "all")
	if s := m.status(t, "GET", "/api/v1/me", onAdamsPhone, ""); s != http.StatusOK {
		t.Fatalf("same owner: adam's phone = %d", s)
	}

	// Moved as a grown-up, so nothing a kid loses anyway clears the code.
	open := pairing()
	assign(m.adminID, "grownup", "all")
	if s := m.status(t, "GET", "/api/v1/libraries", onAdamsPhone, ""); s != http.StatusUnauthorized {
		t.Errorf("moved: adam's phone = %d", s)
	}
	if s, _ := m.exchangeToken(t, open, "Adam's tablet"); s != http.StatusUnauthorized {
		t.Errorf("moved: adam's open pairing code = %d", s)
	}
	if s := m.status(t, "GET", "/api/v1/me", tablet, ""); s != http.StatusOK {
		t.Errorf("moved: her own tablet = %d", s)
	}
	if ins := m.list(t, m.adamTok).Members; len(ins) != 0 {
		t.Errorf("adam still lists %v", ins)
	}
}

// A kid holds no invite: becoming one retires the person's still-redeemable
// invites, so no code pairs a new device into the kid's account. Allowed: the
// owner's "Set up their own device" still pairs one.
func TestMembers_KidLosesInvites(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ctx := context.Background()
	ellis, _ := m.grownup(t, m.adamTok, "Ellis", "all")
	invite, err := m.auth.CreateInvite(ctx, ellis.ID, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s, _, _ := m.redeemCode(t, invite.Code); s != http.StatusOK {
		t.Fatalf("grown-up's invite = %d", s)
	}
	if s := m.status(t, "PATCH", memberURL(ellis.ID, ""), m.adamTok, `{"kind":"kid"}`); s != http.StatusOK {
		t.Fatalf("to kid = %d", s)
	}
	if s, _, _ := m.redeemCode(t, invite.Code); s != http.StatusUnauthorized {
		t.Errorf("kid's invite = %d", s)
	}
	resp, b := m.do(t, "POST", memberURL(ellis.ID, "pair"), m.adamTok, "")
	var pair PairingPayload
	_ = json.Unmarshal([]byte(b), &pair)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner's pairing = %d %s", resp.StatusCode, b)
	}
	if s, b := m.exchangeToken(t, pair.PairingToken, "Ellis's tablet"); s != http.StatusOK {
		t.Errorf("owner's pairing exchange = %d %s", s, b)
	}
}

// A member the admin disabled can't be signed in or paired (no token of theirs
// would work): 409 member_disabled, with no token made. Allowed: the owner still
// manages them, and once enabled again they sign in.
func TestMembers_DisabledMember(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ctx := context.Background()
	ellis, _ := m.grownup(t, m.adamTok, "Ellis", "all")
	userURL := "/api/v1/admin/users/" + strconv.FormatInt(ellis.ID, 10)
	if s := m.status(t, "PATCH", userURL, m.adminTok, `{"disabled":true}`); s != http.StatusOK {
		t.Fatalf("disable = %d", s)
	}
	var before int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE user_id = ?`, ellis.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"session", "pair"} {
		resp, b := m.do(t, "POST", memberURL(ellis.ID, route), m.adamTok, "")
		if resp.StatusCode != http.StatusConflict || codeOf(b) != "member_disabled" {
			t.Errorf("%s = %d %s", route, resp.StatusCode, b)
		}
	}
	var after int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE user_id = ?`, ellis.ID).Scan(&after); err != nil || after != before {
		t.Errorf("tokens %d -> %d, %v", before, after, err)
	}
	if s := m.status(t, "POST", memberURL(ellis.ID, "session"), m.bobTok, ""); s != http.StatusNotFound {
		t.Errorf("bob = %d", s)
	}
	if s := m.status(t, "PATCH", memberURL(ellis.ID, ""), m.adamTok, `{"name":"Elle"}`); s != http.StatusOK {
		t.Errorf("rename = %d", s)
	}
	if s := m.status(t, "PATCH", userURL, m.adminTok, `{"disabled":false}`); s != http.StatusOK {
		t.Fatalf("enable = %d", s)
	}
	if tok := m.memberSession(t, m.adamTok, ellis.ID); m.status(t, "GET", "/api/v1/me", tok, "") != http.StatusOK {
		t.Error("enabled member's session refused")
	}
}

// The typed name changes only with a rename: a kind or access change on a
// member with none (an assigned account) leaves display_name empty, so /me
// carries none. Allowed: a rename sets it.
func TestMembers_DisplayName_KindChangeKeepsIt(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	resp, b := m.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(m.bob, 10), m.adminTok,
		fmt.Sprintf(`{"managed_by":%d,"member_kind":"grownup","member_access":"all"}`, m.adam))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("assign bob = %d %s", resp.StatusCode, b)
	}
	if s := m.status(t, "PATCH", memberURL(m.bob, ""), m.adamTok, `{"access":"collections"}`); s != http.StatusOK {
		t.Fatalf("access change = %d", s)
	}
	_, me := m.do(t, "GET", "/api/v1/me", m.bobTok, "")
	if strings.Contains(me, `"display_name"`) {
		t.Errorf("an access change set a typed name: %s", me)
	}
	if s := m.status(t, "PATCH", memberURL(m.bob, ""), m.adamTok, `{"name":"Bobby"}`); s != http.StatusOK {
		t.Fatalf("rename = %d", s)
	}
	if u := m.me(t, m.bobTok); u.MemberName != "Bobby" {
		t.Errorf("renamed /me = %+v", u)
	}
}

// Routes (denied): every member route refuses the owner's own API key (403,
// even on the owner's own member), a demo account (403 demo_account) and a
// member caller (403 member_cannot_manage, on themselves and on a sibling), and
// no refused call adds, changes, signs in or signs out anyone.
func TestMembers_Routes_Denied(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ctx := context.Background()
	evie, evieTok := m.kid(t, m.adamTok, "Evie")
	ellis, ellisTok := m.grownup(t, m.adamTok, "Ellis", "all")
	devices, _ := myDevicesOf(t, m.testEnv, evieTok)
	key, err := m.auth.IssueToken(ctx, m.adam, auth.KindAPI, "script", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, demoTok := newUserWithSession(t, m.testEnv, auth.DemoUsernamePrefix+"x", true)
	state := func() string {
		t.Helper()
		var users, tokens int
		if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
			t.Fatal(err)
		}
		if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE revoked = 0`).Scan(&tokens); err != nil {
			t.Fatal(err)
		}
		_, list := m.do(t, "GET", "/api/v1/me/household/members", m.adamTok, "")
		return strconv.Itoa(users) + " " + strconv.Itoa(tokens) + " " + list
	}
	before := state()

	routes := func(id int64) []struct{ method, path, body string } {
		return []struct{ method, path, body string }{
			{"POST", "/api/v1/me/household/members", `{"name":"Kit","kind":"kid","access":"all","sign_in_here":true}`},
			{"POST", memberURL(id, "session"), ""},
			{"POST", memberURL(id, "pair"), ""},
			{"GET", memberURL(id, "devices"), ""},
			{"DELETE", memberURL(id, "devices/") + strconv.FormatInt(devices[0].ID, 10), ""},
			{"PATCH", memberURL(id, ""), `{"name":"Evelyn","kind":"grownup","access":"all"}`},
			{"DELETE", memberURL(id, ""), ""},
		}
	}
	for _, c := range []struct {
		who, tok, code, msg string
		target              int64
	}{
		{"adam's API key", key, "", "not available when authenticating with an API key", evie.ID},
		{"a demo account", demoTok, "demo_account", "not available for demo accounts", evie.ID},
		{"the kid on herself", evieTok, "member_cannot_manage", "", evie.ID},
		{"the kid on her sibling", evieTok, "member_cannot_manage", "", ellis.ID},
		{"the grown-up on the kid", ellisTok, "member_cannot_manage", "", evie.ID},
	} {
		for _, r := range routes(c.target) {
			resp, b := m.do(t, r.method, r.path, c.tok, r.body)
			if resp.StatusCode != http.StatusForbidden || codeOf(b) != c.code || !strings.Contains(b, c.msg) {
				t.Errorf("%s: %s %s = %d %s, want 403 %q", c.who, r.method, r.path, resp.StatusCode, b, c.code)
			}
		}
	}
	// The API key on the list (a demo or member caller gets 200 with allowed
	// false there: TestMembers_Create_Denied).
	if s := m.status(t, "GET", "/api/v1/me/household/members", key, ""); s != http.StatusForbidden {
		t.Errorf("API key list = %d", s)
	}
	if after := state(); after != before {
		t.Fatalf("refused calls changed something:\n before %s\n after  %s", before, after)
	}
	// Allowed: the owner's session does each of them (TestMembers_Ownership).
	if s := m.status(t, "POST", memberURL(evie.ID, "session"), m.adamTok, ""); s != http.StatusCreated {
		t.Fatalf("adam's session on evie = %d", s)
	}
}

// The kid refusals hold in the writes too, not only in notKid's look at the
// request's token: a grown-up's request that passed notKid and lands after the
// owner's switch to kid (here a slow client, whose body arrives after the
// switch) is 403 kid_account and leaves no API key, house sharing, password or
// collection share with anyone but the owner behind. Allowed: the same requests
// before the switch work.
func TestMembers_KidRefusalRacesTheSwitch(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ctx := context.Background()
	ellis, ellisTok := m.grownup(t, m.adamTok, "Ellis", "all")
	if resp, b := m.do(t, "POST", "/api/v1/auth/tokens", ellisTok, `{"label":"before"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("grown-up's key = %d %s", resp.StatusCode, b)
	}
	if s := m.status(t, "PUT", "/api/v1/me/household", ellisTok, `{"share_listening":true}`); s != http.StatusOK {
		t.Fatalf("grown-up's sharing = %d", s)
	}
	col := m.collection(t, ellisTok, "Ellis's", cradleBook)
	sharesURL := fmt.Sprintf("/api/v1/me/collections/%d/shares", col)
	bobBody := fmt.Sprintf(`{"user_ids":[%d]}`, m.bob)
	if s := m.status(t, "PUT", sharesURL, ellisTok, bobBody); s != http.StatusOK {
		t.Fatalf("grown-up's share = %d", s)
	}
	// slow starts method path on a fresh session of ellis's (last_seen NULL, so
	// the request's token touch shows it got past the lookup) and returns a func
	// that sends the body and reads the answer.
	slow := func(method, path string) func(body string) (int, string) {
		t.Helper()
		tok, err := m.auth.IssueToken(ctx, ellis.ID, auth.KindSession, path, 0)
		if err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := m.db.QueryRowContext(ctx, `SELECT id FROM tokens WHERE user_id = ? AND device_name = ?`, ellis.ID, path).Scan(&id); err != nil {
			t.Fatal(err)
		}
		type answer struct {
			status int
			body   string
		}
		pr, pw := io.Pipe()
		done := make(chan answer, 1)
		go func() {
			req, _ := http.NewRequest(method, m.srv.URL+path, pr)
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				done <- answer{0, err.Error()}
				return
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			done <- answer{resp.StatusCode, string(b)}
		}()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			var seen bool
			_ = m.db.QueryRowContext(ctx, `SELECT last_seen IS NOT NULL FROM tokens WHERE id = ?`, id).Scan(&seen)
			if seen {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s never got past the lookup", method, path)
			}
		}
		return func(body string) (int, string) {
			_, _ = pw.Write([]byte(body))
			_ = pw.Close()
			a := <-done
			return a.status, a.body
		}
	}
	key := slow("POST", "/api/v1/auth/tokens")
	share := slow("PUT", "/api/v1/me/household")
	password := slow("POST", "/api/v1/auth/password")
	colShare := slow("PUT", sharesURL)
	if s := m.status(t, "PATCH", memberURL(ellis.ID, ""), m.adamTok, `{"kind":"kid"}`); s != http.StatusOK {
		t.Fatalf("to kid = %d", s)
	}
	if s, b := key(`{"label":"late"}`); s != http.StatusForbidden || codeOf(b) != "kid_account" {
		t.Errorf("API key across the switch = %d %s", s, b)
	}
	if s, b := share(`{"share_listening":true}`); s != http.StatusForbidden || codeOf(b) != "kid_account" {
		t.Errorf("house sharing across the switch = %d %s", s, b)
	}
	// The self-service password answers as notKid does (403 kid_account), not
	// the admin PATCH's 400 kid_no_password.
	if s, b := password(`{"password":"late-password-1"}`); s != http.StatusForbidden || codeOf(b) != "kid_account" {
		t.Errorf("password across the switch = %d %s", s, b)
	}
	if s, b := colShare(bobBody); s != http.StatusForbidden || codeOf(b) != "kid_account" {
		t.Errorf("collection share across the switch = %d %s", s, b)
	}
	if u, err := m.auth.GetUser(ctx, ellis.ID); err != nil || u.HasPassword {
		t.Errorf("the kid holds a password: %+v %v", u, err)
	}
	var shared int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM collection_shares WHERE collection_id = ?`, col).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if shared != 0 {
		t.Errorf("the kid's collection is shared with %d people", shared)
	}
	var keys, rows int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE user_id = ? AND kind = 'api' AND revoked = 0`, ellis.ID).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM household_sharing WHERE user_id = ?`, ellis.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if keys != 0 || rows != 0 {
		t.Errorf("the kid holds %d API keys and %d house sharing rows", keys, rows)
	}
}

// The account limiter (gateSelfService's, per address) counts only calls that
// can make an account or a credential: a member's refused calls from the
// household's address spend none of it, so the owner can still add someone.
// Denied: past the limit the owner's own calls are 429.
func TestMembers_RefusedCallsSpendNoBudget(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	_, evieTok := m.kid(t, m.adamTok, "Evie")
	m.api.accountLimiter = newLimiter(10, 15*time.Minute)
	for range 12 {
		if status, b := m.add(t, evieTok, `{"name":"Kit","kind":"kid","access":"all"}`); status != http.StatusForbidden || codeOf(b) != "member_cannot_manage" {
			t.Fatalf("member's add = %d %s", status, b)
		}
	}
	for i := range 10 {
		if status, b := m.add(t, m.adamTok, fmt.Sprintf(`{"name":"Kit %d","kind":"grownup","access":"all"}`, i)); status != http.StatusCreated && status != http.StatusConflict {
			t.Fatalf("owner's add %d = %d %s", i, status, b)
		}
	}
	if status, _ := m.add(t, m.adamTok, `{"name":"One more","kind":"grownup","access":"all"}`); status != http.StatusTooManyRequests {
		t.Errorf("past the limit = %d", status)
	}
}

// The member rule is read in two places: auth's token lookup and GetUser
// (memberColumns, memberScan; the transport's callerOf) and catalog's by-id forms
// (memberRuleOf; the importer, tests). For every kind of account, bad data
// included, both give the same scopes and libraries, so the two readings can't
// drift apart unnoticed.
func TestMembers_RuleReadsAgree(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ctx := context.Background()
	col := m.collection(t, m.adamTok, "Evie's books", cradleBook)
	evie, _ := m.kid(t, m.adamTok, "Evie", col)
	ellis, _ := m.grownup(t, m.adamTok, "Ellis", "all")
	ada, _ := m.grownup(t, m.adminTok, "Ada", "all")
	adc, _ := m.grownup(t, m.adminTok, "Adc", "collections")
	// Bad data: self-managed, a member's member, a partial row.
	self, _ := newUserWithSession(t, m.testEnv, "selfy", false)
	chain, _ := newUserWithSession(t, m.testEnv, "chain", false)
	partial, _ := newUserWithSession(t, m.testEnv, "partial", false)
	if err := m.cat.GrantWholeLibrary(ctx, self, m.other); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE users SET managed_by = id, member_kind = 'kid', member_access = 'all' WHERE id = ?`, []any{self}},
		{`UPDATE users SET managed_by = ?, member_kind = 'kid', member_access = 'all' WHERE id = ?`, []any{ellis.ID, chain}},
		{`UPDATE users SET managed_by = ?, member_kind = NULL, member_access = 'all' WHERE id = ?`, []any{m.adam, partial}},
	} {
		if _, err := m.db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	// Scopes compare with an empty path list as none.
	norm := func(in []catalog.Scope) []catalog.Scope {
		out := make([]catalog.Scope, len(in))
		for i, s := range in {
			if len(s.Paths) == 0 {
				s.Paths = nil
			}
			out[i] = s
		}
		return out
	}
	for name, id := range map[string]int64{
		"admin": m.adminID, "adam": m.adam, "bob": m.bob, "evie": evie.ID, "ellis": ellis.ID,
		"ada": ada.ID, "adc": adc.ID, "self": self, "chain": chain, "partial": partial,
	} {
		u, err := m.auth.GetUser(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		admin, who := u.Role == auth.RoleAdmin, callerOf(u)
		byID, err1 := m.cat.UserScopes(ctx, id, admin)
		caller, err2 := m.cat.CallerScopes(ctx, who)
		if err1 != nil || err2 != nil || !reflect.DeepEqual(norm(byID), norm(caller)) {
			t.Errorf("%s: UserScopes %+v %v, CallerScopes %+v %v", name, byID, err1, caller, err2)
		}
		for _, lib := range []int64{m.lib, m.other, 9999} {
			a, err1 := m.cat.UserScope(ctx, id, lib, admin)
			b, err2 := m.cat.CallerScope(ctx, who, lib)
			if err1 != nil || err2 != nil || !reflect.DeepEqual(norm([]catalog.Scope{a}), norm([]catalog.Scope{b})) {
				t.Errorf("%s in %d: UserScope %+v %v, CallerScope %+v %v", name, lib, a, err1, b, err2)
			}
		}
		la, err1 := m.cat.AccessibleLibraries(ctx, id, admin)
		lb, err2 := m.cat.CallerLibraries(ctx, who)
		if err1 != nil || err2 != nil || !reflect.DeepEqual(la, lb) {
			t.Errorf("%s: AccessibleLibraries %v %v, CallerLibraries %v %v", name, la, err1, lb, err2)
		}
	}
}

// DELETE /admin/users/{id} keeps every member keep_members names, whether the
// ids come comma-separated or as the parameter repeated (how a query builder
// encodes a list): reading only the first value would delete the others.
func TestMembers_DeleteKeepsRepeatedKeepMembers(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	evie, evieTok := m.kid(t, m.adamTok, "Evie")
	ellis, ellisTok := m.grownup(t, m.adamTok, "Ellis", "all")
	_, kitTok := m.grownup(t, m.adamTok, "Kit", "all")
	target := fmt.Sprintf("/api/v1/admin/users/%d?keep_members=%d&keep_members=%d", m.adam, evie.ID, ellis.ID)
	if s := m.status(t, "DELETE", target, m.adminTok, ""); s != http.StatusNoContent {
		t.Fatalf("delete adam = %d", s)
	}
	for name, tok := range map[string]string{"evie": evieTok, "ellis": ellisTok} {
		if u := m.me(t, tok); u.ManagedBy != nil {
			t.Errorf("kept %s = %+v", name, u)
		}
	}
	if s := m.status(t, "GET", "/api/v1/me", kitTok, ""); s != http.StatusUnauthorized {
		t.Error("the member not kept survived")
	}
}
