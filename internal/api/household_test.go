package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// houseEnv: the fixture library; kid sees only the "Will Wight" folder, sam and
// pat the whole library, and the demo and disabled accounts would share if they
// could.
type houseEnv struct {
	*testEnv
	libID           int64
	kid, sam, pat   int64
	kidTok, samTok  string
	patTok, demoTok string
	adminTok        string
	demo, dora      int64
	doraTok         string
}

func newHouseEnv(t *testing.T) *houseEnv {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	lib := newFixtureLibrary(t, e)
	h := &houseEnv{testEnv: e, libID: lib.ID}
	h.kid, h.kidTok = newUserWithSession(t, e, "kid", false)
	h.sam, h.samTok = newUserWithSession(t, e, "sam", false)
	h.pat, h.patTok = newUserWithSession(t, e, "pat", false)
	h.demo, h.demoTok = newUserWithSession(t, e, "demo_1", true)
	h.dora, h.doraTok = newUserWithSession(t, e, "dora", false)
	for _, id := range []int64{h.sam, h.pat, h.demo, h.dora} {
		if err := e.cat.GrantWholeLibrary(ctx, id, lib.ID); err != nil {
			t.Fatal(err)
		}
	}
	grantWightOnly(t, e, lib.ID, h.kid)
	tok, err := e.auth.IssueToken(ctx, e.adminID, "session", "admin phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.adminTok = tok
	return h
}

// setShare PUTs /me/household for tok, wanting 200 and the echoed choice.
func (h *houseEnv) setShare(t *testing.T, tok string, on bool) {
	t.Helper()
	resp, body := h.do(t, "PUT", "/api/v1/me/household", tok, `{"share_listening":`+strconv.FormatBool(on)+`}`)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(body) != `{"share_listening":`+strconv.FormatBool(on)+`}` {
		t.Fatalf("PUT /me/household = %d %s", resp.StatusCode, body)
	}
}

// listen makes tok's progress saves on path, which record a session with listening.
func (h *houseEnv) listen(t *testing.T, tok, path string) {
	t.Helper()
	for _, pos := range []string{"10", "20"} {
		if resp, b := h.do(t, "PUT", bookURL(h.libID, "progress", path), tok, `{"position":`+pos+`,"duration":120}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("progress save = %d %s", resp.StatusCode, b)
		}
	}
}

func (h *houseEnv) house(t *testing.T, tok string) (houseListening, string) {
	t.Helper()
	resp, body := h.do(t, "GET", "/api/v1/household/listening", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /household/listening = %d %s", resp.StatusCode, body)
	}
	var out houseListening
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out, body
}

func usernames(rows []catalog.HouseListeningRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Username)
	}
	slices.Sort(out)
	return out
}

func TestHouseholdSetting(t *testing.T) {
	t.Parallel()
	h := newHouseEnv(t)
	get := func(tok string) string {
		t.Helper()
		resp, body := h.do(t, "GET", "/api/v1/me/household", tok, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /me/household = %d %s", resp.StatusCode, body)
		}
		return strings.TrimSpace(body)
	}
	if got := get(h.samTok); got != `{"share_listening":false}` {
		t.Fatalf("default = %s", got)
	}
	h.setShare(t, h.samTok, true)
	if got := get(h.samTok); got != `{"share_listening":true}` {
		t.Fatalf("after PUT true = %s", got)
	}
	// Someone else's choice is their own.
	if got := get(h.patTok); got != `{"share_listening":false}` {
		t.Fatalf("pat's = %s", got)
	}
	h.setShare(t, h.samTok, false)
	if got := get(h.samTok); got != `{"share_listening":false}` {
		t.Fatalf("after PUT false = %s", got)
	}
	// An API key is the person's own credential: allowed.
	_, key := h.mintAPIKey(t, h.samTok, "script")
	h.setShare(t, key, true)

	// Denied: a bad body is 400 (unknown field, no field, wrong type, not JSON).
	for _, bad := range []string{`{"share_listening":true,"extra":1}`, `{}`, `{"share_listening":"yes"}`, `nope`} {
		if resp, body := h.do(t, "PUT", "/api/v1/me/household", h.samTok, bad); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PUT %s = %d %s; want 400", bad, resp.StatusCode, body)
		}
	}
	// Denied: a demo account may read its (off) choice but never share.
	if got := get(h.demoTok); got != `{"share_listening":false}` {
		t.Fatalf("demo GET = %s", got)
	}
	if resp, body := h.do(t, "PUT", "/api/v1/me/household", h.demoTok, `{"share_listening":true}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("demo PUT = %d %s; want 403", resp.StatusCode, body)
	}
	if got := get(h.demoTok); got != `{"share_listening":false}` {
		t.Fatalf("demo after a refused PUT = %s", got)
	}
	// Denied: no credential.
	if resp, _ := h.do(t, "GET", "/api/v1/me/household", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d", resp.StatusCode)
	}

	// Deleting the account purges its choice.
	if resp, body := h.do(t, "DELETE", "/api/v1/admin/users/"+strconv.FormatInt(h.sam, 10), h.adminTok, ""); resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("delete sam = %d %s", resp.StatusCode, body)
	}
	var n int
	if err := h.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM household_sharing WHERE user_id = ?`, h.sam).Scan(&n); err != nil || n != 0 {
		t.Fatalf("sam's household rows after the delete = %d, %v", n, err)
	}
}

func TestHouseListening(t *testing.T) {
	t.Parallel()
	h := newHouseEnv(t)
	ctx := context.Background()
	h.listen(t, h.samTok, cradleBook)   // in kid's scope
	h.listen(t, h.patTok, mistbornBook) // outside kid's scope
	h.listen(t, h.demoTok, cradleBook)
	h.listen(t, h.doraTok, cradleBook)
	h.listen(t, h.kidTok, cradleBook)
	h.setShare(t, h.samTok, true)
	h.setShare(t, h.patTok, true)
	h.setShare(t, h.doraTok, true)
	// The demo account would share if the API let it; dora is disabled after sharing.
	if err := h.cat.SetHouseholdSharing(ctx, h.demo, true); err != nil {
		t.Fatal(err)
	}
	if err := h.auth.SetDisabled(ctx, h.dora, true); err != nil {
		t.Fatal(err)
	}

	// Denied: kid doesn't share, so sees nobody (reciprocal).
	got, body := h.house(t, h.kidTok)
	if got.Sharing || strings.TrimSpace(body) != `{"sharing":false,"listening":[]}` {
		t.Fatalf("kid before sharing = %s", body)
	}

	// Allowed: once kid shares, sam (sharing, on a book in kid's scope) shows,
	// playing; pat's book is outside kid's scope, the demo and disabled accounts
	// never show, and kid never sees themself.
	h.setShare(t, h.kidTok, true)
	got, body = h.house(t, h.kidTok)
	if !got.Sharing || len(got.Listening) != 1 {
		t.Fatalf("kid's house = %s", body)
	}
	row := got.Listening[0]
	if row.UserID != h.sam || row.Username != "sam" || row.LibraryID != h.libID || row.Path != cradleBook ||
		row.State != catalog.SessionPlaying || row.Position != 20 || row.Title == "" || len(row.UpdatedAt) != len("2006-01-02T15:04:05.000Z") {
		t.Fatalf("sam's row = %s", body)
	}
	// The row's keys are exactly the contract's: never a device, an address, an
	// app or a token.
	var raw struct {
		Listening []map[string]json.RawMessage `json:"listening"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(raw.Listening[0])); strings.Join(keys, ",") != "author,chapter,chapter_index,duration,library_id,path,position,state,title,updated_at,user_id,username" {
		t.Fatalf("row keys = %v", keys)
	}
	for _, leak := range []string{"ip", "device", "client", "token", "last_ip", "device_name", "device_id"} {
		if strings.Contains(body, `"`+leak+`"`) || strings.Contains(body, "sam's phone") {
			t.Fatalf("the house leaks %q: %s", leak, body)
		}
	}

	// Allowed: the admin (whole library, sharing) sees kid, pat and sam (kid shares
	// now too); not the demo, not the disabled account.
	h.setShare(t, h.adminTok, true)
	got, body = h.house(t, h.adminTok)
	if names := usernames(got.Listening); strings.Join(names, ",") != "kid,pat,sam" {
		t.Fatalf("admin's house = %v (%s)", names, body)
	}

	// Denied: a housemate who stops sharing leaves; a caller who stops sharing
	// sees nobody again.
	h.setShare(t, h.samTok, false)
	if got, body = h.house(t, h.kidTok); len(got.Listening) != 0 {
		t.Fatalf("kid after sam stopped = %s", body)
	}
	h.setShare(t, h.adminTok, false)
	if got, body = h.house(t, h.adminTok); got.Sharing || len(got.Listening) != 0 {
		t.Fatalf("admin after stopping = %s", body)
	}

	// Denied: anonymous.
	if resp, _ := h.do(t, "GET", "/api/v1/household/listening", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", resp.StatusCode)
	}
}

func TestHouseholdCapability(t *testing.T) {
	t.Parallel()
	if !serverCapabilities(t, newTestEnv(t))["household"] {
		t.Fatal("household capability is off")
	}
}

// serverCapabilities reads GET /server's capability flags.
func serverCapabilities(t *testing.T, e *testEnv) map[string]bool {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/server", "", "")
	var info struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &info) != nil {
		t.Fatalf("/server = %d %s", resp.StatusCode, body)
	}
	return info.Capabilities
}
