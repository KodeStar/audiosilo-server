package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/modules"
	"github.com/kodestar/audiosilo-server/internal/modules/builtin"
)

// Bridge Phase 2b: the heartbeat's setup block and accepts, GET /server's
// bridge block for signed-in callers only, the community job kinds
// (sidecars.contribute, meta.correct) behind the contributions module, and the
// module routes' kid and demo refusals.

// setupBlock is a heartbeat's setup block: everything done but the GitHub
// sign-in, can_contribute as given.
func setupBlock(can bool) string {
	return fmt.Sprintf(`{"items":[{"id":"paired","done":true,"required":true},
		{"id":"libraries","done":false,"required":true,"detail":"1 of 2 libraries writable","library_ids":[3]},
		{"id":"github","done":%t,"required":false}],"can_contribute":%t,"github_login":"octocat"}`, can, can)
}

// beatKinds sends a heartbeat offering kinds, the env's library mapped and
// writable, with extra top-level fields (`,"setup":{...}`, or ""), and returns
// the reply.
func (b *bridgeEnv) beatKinds(t *testing.T, extra string, kinds ...string) string {
	t.Helper()
	k, _ := json.Marshal(kinds)
	body := fmt.Sprintf(`{"version":"0.3.0","kinds":%s,
		"libraries":[{"library_id":%d,"mount_path":"/media/books","mapped":true,"writable":true}]%s}`, k, b.lib.ID, extra)
	resp, out := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, out)
	}
	return out
}

// The heartbeat's reply lists accepts ["setup"]; a setup block lands on GET
// /admin/bridge (login included) and never on GET /server; one of the wrong
// shape is dropped, never refusing the heartbeat; its can_contribute wins over
// the estimates' for the contribute feature, the module's flag and the estimate.
func TestBridgeHeartbeatSetup(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	setModule(t, b.testEnv, b.adminTok, true)
	reply := b.beatKinds(t, "", bridge.KindVerify, bridge.KindSidecars)
	var r struct {
		Accepts []string `json:"accepts"`
	}
	decodeInto(t, reply, &r)
	if !slices.Equal(r.Accepts, []string{"setup"}) {
		t.Fatalf("accepts = %s", reply)
	}
	// No setup sent: none shown.
	if _, body := b.do(t, "GET", "/api/v1/admin/bridge", b.adminTok, ""); strings.Contains(body, `"setup"`) {
		t.Fatalf("setup without one: %s", body)
	}

	// The estimates say yes, the setup says no: no.
	yes := `,"estimates":{"sidecars":{"can_contribute":true}}`
	b.beatKinds(t, yes+`,"setup":`+setupBlock(false), bridge.KindVerify, bridge.KindSidecars)
	var admin struct {
		Features []string      `json:"features"`
		Setup    *bridge.Setup `json:"setup"`
	}
	_, body := b.do(t, "GET", "/api/v1/admin/bridge", b.adminTok, "")
	decodeInto(t, body, &admin)
	if admin.Setup == nil || admin.Setup.CanContribute || admin.Setup.GitHubLogin != "octocat" || len(admin.Setup.Items) != 3 ||
		!slices.Equal(admin.Setup.Items[1].LibraryIDs, []int64{3}) || slices.Contains(admin.Features, bridge.FeatureContribute) {
		t.Fatalf("admin bridge (setup no): %s", body)
	}
	if capability(t, b.testEnv) {
		t.Fatal("contributions flag true while the setup says the Bridge can't")
	}
	if _, est, body := b.estimate(t, b.adminTok, b.lib.ID, mistbornBook); est.CanContribute {
		t.Fatalf("estimate (setup no): %s", body)
	}
	for _, tok := range []string{"", b.userTok, b.adminTok} {
		if _, body := b.do(t, "GET", "/api/v1/server", tok, ""); strings.Contains(body, "octocat") || strings.Contains(body, "setup") {
			t.Fatalf("GET /server shows the setup: %s", body)
		}
	}

	// The setup says yes over estimates that say nothing.
	b.beatKinds(t, `,"setup":`+setupBlock(true), bridge.KindVerify, bridge.KindSidecars)
	if !capability(t, b.testEnv) {
		t.Fatal("contributions flag false while the setup says the Bridge can")
	}
	if _, est, body := b.estimate(t, b.adminTok, b.lib.ID, mistbornBook); !est.CanContribute || est.Figures {
		t.Fatalf("estimate (setup yes): %s", body)
	}

	// A block of the wrong shape is dropped, the heartbeat kept: the estimates
	// answer again.
	b.beatKinds(t, yes+`,"setup":{"items":"all done","can_contribute":false}`, bridge.KindVerify, bridge.KindSidecars)
	_, body = b.do(t, "GET", "/api/v1/admin/bridge", b.adminTok, "")
	if strings.Contains(body, `"setup"`) || !capability(t, b.testEnv) {
		t.Fatalf("a malformed setup: %s", body)
	}
	// An unknown top-level field is still refused (the reason for accepts).
	resp, _ := b.do(t, "PUT", "/api/v1/bridge/status", b.bridgeTok, `{"version":"0.3.0","telescope":{}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown heartbeat field = %d, want 400", resp.StatusCode)
	}
}

// Security: GET /server's bridge block (the Bridge's version and features) goes
// only to a signed-in caller of any role; anyone else sees the flag alone.
func TestServerInfoBridgeBlockNeedsSignIn(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	read := func(tok string) (bool, bool) {
		t.Helper()
		_, body := b.do(t, "GET", "/api/v1/server", tok, "")
		var info struct {
			Capabilities map[string]bool `json:"capabilities"`
			Bridge       *bridgeInfo     `json:"bridge"`
		}
		decodeInto(t, body, &info)
		return info.Capabilities["bridge"], info.Bridge != nil
	}
	for name, tok := range map[string]string{
		"signed out":    "",
		"a bad token":   "not-a-token",
		"bridge token":  b.bridgeTok,
		"revoked token": revokedSession(t, b.testEnv),
	} {
		if flag, block := read(tok); !flag || block {
			t.Errorf("denied (%s): flag %t, block %t; want the flag alone", name, flag, block)
		}
	}
	for name, tok := range map[string]string{"listener": b.userTok, "admin": b.adminTok, "api key": b.adminKey} {
		if flag, block := read(tok); !flag || !block {
			t.Errorf("allowed (%s): flag %t, block %t; want both", name, flag, block)
		}
	}
}

// revokedSession is a listener's session token that was signed out.
func revokedSession(t *testing.T, e *testEnv) string {
	t.Helper()
	_, tok := newUserWithSession(t, e, "gone", false)
	if resp, body := e.do(t, "POST", "/api/v1/auth/logout", tok, ""); resp.StatusCode >= 300 {
		t.Fatalf("logout: %d %s", resp.StatusCode, body)
	}
	return tok
}

func (b *bridgeEnv) queueKind(t *testing.T, kind, path, params string) (*http.Response, string) {
	t.Helper()
	return b.do(t, "POST", "/api/v1/admin/bridge/jobs", b.adminTok,
		fmt.Sprintf(`{"kind":%q,"library_id":%d,"path":%q,"params":%s}`, kind, b.lib.ID, path, params))
}

// wantCode checks a refusal's status and code.
func wantCode(t *testing.T, what string, resp *http.Response, body string, status int, code string) {
	t.Helper()
	if resp.StatusCode != status || !strings.Contains(body, `"code":"`+code+`"`) {
		t.Fatalf("%s: %d %s, want %d %s", what, resp.StatusCode, body, status, code)
	}
}

// sidecars.contribute through the console's queue: 404 module_off while the
// contributions module is off (also before the Bridge's answer), 409
// cannot_contribute while the Bridge can't, 409 kind_unavailable from a Bridge
// that doesn't run it; queued with its params normalised; 409 generate_open and
// contribute_open between it and a generate job for the book.
func TestBridgeContributeJob(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	all := []string{bridge.KindVerify, bridge.KindSidecars, bridge.KindContribute}
	b.beatKinds(t, `,"setup":`+setupBlock(true), all...)
	resp, body := b.queueKind(t, bridge.KindContribute, mistbornBook, `{}`)
	wantCode(t, "module off", resp, body, http.StatusNotFound, "module_off")

	setModule(t, b.testEnv, b.adminTok, true)
	b.beatKinds(t, `,"setup":`+setupBlock(false), all...)
	resp, body = b.queueKind(t, bridge.KindContribute, mistbornBook, `{}`)
	wantCode(t, "can't contribute", resp, body, http.StatusConflict, "cannot_contribute")
	b.beatKinds(t, `,"setup":`+setupBlock(true), bridge.KindVerify, bridge.KindSidecars)
	resp, body = b.queueKind(t, bridge.KindContribute, mistbornBook, `{}`)
	wantCode(t, "an older Bridge", resp, body, http.StatusConflict, "kind_unavailable")

	b.beatKinds(t, `,"setup":`+setupBlock(true), all...)
	resp, body = b.queueKind(t, bridge.KindContribute, mistbornBook, `{"dimensions":["recaps"],"work_id":"mistborn"}`)
	var job bridge.Job
	decodeInto(t, body, &job)
	if resp.StatusCode != http.StatusCreated || string(job.Params) != `{"work_id":"mistborn","dimensions":["recaps"]}` {
		t.Fatalf("allowed: %d %s", resp.StatusCode, body)
	}
	resp, body = b.queueKind(t, bridge.KindContribute, cradleBook, `{"dimensions":["description"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad params: %d %s, want 400", resp.StatusCode, body)
	}
	// Denied: a generate job while the contribute job is queued.
	resp, body = b.queueSidecars(t, b.lib.ID, mistbornBook, `{}`)
	wantCode(t, "generate with a contribute open", resp, body, http.StatusConflict, "contribute_open")
	// Denied: a contribute job while a generate job is open for the book.
	if resp, body := b.queueSidecars(t, b.lib.ID, cradleBook, `{}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("generate: %d %s", resp.StatusCode, body)
	}
	resp, body = b.queueKind(t, bridge.KindContribute, cradleBook, `{}`)
	wantCode(t, "contribute with a generate open", resp, body, http.StatusConflict, "generate_open")
}

// correction is a meta.correct params body for one change.
func correction(field, current, corrected string) string {
	return fmt.Sprintf(`{"work_id":"the-final-empire","asin":"B002UZMLXM",
		"changes":[{"field":%q,"current":%q,"corrected":%q}],"evidence":"The cover says so.","source":"evil/1.0"}`,
		field, current, corrected)
}

// Security: meta.correct files only an admin's own edits of the book. Each
// change must be a field edited on this server (not scanned, nor accepted from
// the community) whose corrected value is the book's value now (400 not_edited
// otherwise); the source is stamped by the server whatever was sent; the module
// must be on (404 module_off) and the params are strict (400).
func TestBridgeCorrectJob(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	b.beatKinds(t, `,"setup":`+setupBlock(true), bridge.KindVerify, bridge.KindCorrect)
	if err := b.cat.EditBook(ctx, b.lib.ID, mistbornBook, catalog.BookEdit{Set: map[string]string{
		catalog.FieldTitle: "Mistborn: The Final Empire", catalog.FieldSeriesIndex: "1",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := b.cat.EditBook(ctx, b.lib.ID, mistbornBook, catalog.BookEdit{Source: catalog.SourceCommunity,
		Set: map[string]string{catalog.FieldNarrator: "Michael Kramer"}}); err != nil {
		t.Fatal(err)
	}
	good := correction("title", "The Final Empire", "Mistborn: The Final Empire")
	resp, body := b.queueKind(t, bridge.KindCorrect, mistbornBook, good)
	wantCode(t, "module off", resp, body, http.StatusNotFound, "module_off")
	setModule(t, b.testEnv, b.adminTok, true)

	for name, params := range map[string]string{
		"scanned, never edited":    correction("author", "Brandon", "Brandon Sanderson"),
		"accepted from community":  correction("narrator", "M. Kramer", "Michael Kramer"),
		"not the book's value now": correction("title", "The Final Empire", "Mistborn"),
	} {
		resp, body := b.queueKind(t, bridge.KindCorrect, mistbornBook, params)
		wantCode(t, "denied: "+name, resp, body, http.StatusBadRequest, "not_edited")
		if !strings.Contains(body, `"field":"`) {
			t.Errorf("%s: no field named: %s", name, body)
		}
	}
	for name, params := range map[string]string{
		"unknown field":  strings.Replace(good, `"work_id"`, `"x":1,"work_id"`, 1),
		"no evidence":    strings.Replace(good, `"The cover says so."`, `""`, 1),
		"an extra field": correction("description", "a", "b"),
	} {
		if resp, body := b.queueKind(t, bridge.KindCorrect, mistbornBook, params); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, resp.StatusCode, body)
		}
	}

	// Allowed: the edited title, and a series position the same as read.
	params := strings.Replace(good, `"changes":[`,
		`"changes":[{"field":"series_index","current":"2","corrected":"1.0"},`, 1)
	resp, body = b.queueKind(t, bridge.KindCorrect, mistbornBook, params)
	var job bridge.Job
	decodeInto(t, body, &job)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("allowed: %d %s", resp.StatusCode, body)
	}
	var p bridge.CorrectParams
	if err := json.Unmarshal(job.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.Source != "audiosilo-server/"+Version || len(p.Changes) != 2 || p.Changes[0].Corrected != "1" ||
		p.Changes[1].Corrected != "Mistborn: The Final Empire" || p.WorkID != "the-final-empire" {
		t.Fatalf("stored params = %s", job.Params)
	}
	// The meta feature is the kind's family.
	_, body = b.do(t, "GET", "/api/v1/admin/bridge", b.adminTok, "")
	if !strings.Contains(body, `"features":["bridge","contribute","meta"]`) {
		t.Fatalf("features: %s", body)
	}
}

// Security: a module's listener routes refuse a kid's account (403
// kid_account) and a demo account (403 demo_account), on or off, and serve any
// other signed-in listener while on.
func TestModuleUserRoutesRefuseKidsAndDemo(t *testing.T) {
	t.Parallel()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	e := newTestEnv(t)
	withModules(t, e, modules.Module{ID: "contributions", Routes: func(r modules.Registrar) {
		r.User("POST /api/v1/modtest", ok)
	}})
	ctx := context.Background()
	adminTok := adminSession(t, e)
	owner, userTok := newUserWithSession(t, e, "sam", false)
	_, demoTok := newUserWithSession(t, e, auth.DemoUsernamePrefix+"x", true)
	kid, _, _, err := e.auth.CreateMember(ctx, owner, auth.NewMember{Name: "Pip", Kind: auth.MemberKid,
		Access: auth.MemberCollections}, auth.MemberPolicy{Enabled: true, Max: 5})
	if err != nil {
		t.Fatal(err)
	}
	kidTok, err := e.auth.IssueToken(ctx, kid.ID, auth.KindSession, "tablet", 0)
	if err != nil {
		t.Fatal(err)
	}

	refused := func(when string) {
		t.Helper()
		resp, body := e.do(t, "POST", "/api/v1/modtest", kidTok, "")
		wantCode(t, when+": kid", resp, body, http.StatusForbidden, "kid_account")
		resp, body = e.do(t, "POST", "/api/v1/modtest", demoTok, "")
		wantCode(t, when+": demo", resp, body, http.StatusForbidden, "demo_account")
	}
	refused("off")
	setModule(t, e, adminTok, true)
	refused("on")
	for name, tok := range map[string]string{"listener": userTok, "admin": adminTok} {
		if resp, body := e.do(t, "POST", "/api/v1/modtest", tok, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("allowed (%s): %d %s", name, resp.StatusCode, body)
		}
	}
}

// Every route a builtin module registers mounts through the real Registrar: a
// malformed pattern panics (checkModulePattern) and two that clash panic in the
// mux, so this fails at test time rather than at a server's start.
func TestBuiltinModuleRoutePatterns(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	mux := http.NewServeMux()
	for _, m := range builtin.Modules() {
		if m.Routes == nil {
			continue
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("module %s: %v", m.ID, p)
				}
			}()
			m.Routes(moduleRoutes{a: e.api, mux: mux, id: m.ID})
		}()
	}
}

// Every field a meta.correct job may correct is a book field the catalog
// overrides, so checkCorrections can find its source.
func TestCorrectionFieldsAreOverridable(t *testing.T) {
	t.Parallel()
	for _, f := range bridge.CorrectionFields {
		if !slices.Contains(catalog.OverrideFields, f) {
			t.Errorf("correction field %q is not in catalog.OverrideFields", f)
		}
	}
}
