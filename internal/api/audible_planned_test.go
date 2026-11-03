package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/audible"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/modules"
)

// withExtra is title's JSON with extra fields (`"planned":{...}` and the like)
// added at its end.
func withExtra(titleJSON, extra string) string {
	return strings.TrimSuffix(titleJSON, "}") + "," + extra + "}"
}

// The titles PUT takes each unmatched title's planned destination and
// contested book; the console's list shows `planned` and `contested` (with
// the account's other titles on that book), null when there is none, and never
// the input's contested_path.
func TestAudiblePlannedOnTheWire(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	setAudible(t, b.testEnv, b.adminTok, true)
	if code, out := b.putAccount(t, testAccount, b.adminID, b.lib.ID); code != http.StatusOK {
		t.Fatalf("put account: %d %s", code, out)
	}
	plan := fmt.Sprintf(`"planned":{"library_id":%d,"path":"Brandon Sanderson/Mistborn/2 - The Well of Ascension/The Well of Ascension.m4b",
		"file":"The Well of Ascension.m4b","kind":"series_existing","siblings":1,"state":"joins","conflict":"","template":""}`, b.lib.ID)
	contested := withExtra(title("B000000003", "The Final Empire (Dramatised)", "available", 0, ""),
		fmt.Sprintf(`"contested_path":%q`, mistbornBook))
	contested = strings.Replace(contested, `"reason":""`, `"reason":"contested"`, 1)
	if code, out := b.putTitles(t, testAccount, "p1", true,
		title("B000000001", "The Final Empire", "available", b.lib.ID, mistbornBook),
		withExtra(title("B000000002", "The Well of Ascension", "available", 0, ""), plan),
		contested); code != http.StatusOK {
		t.Fatalf("titles: %d %s", code, out)
	}
	resp, out := b.do(t, "GET", "/api/v1/admin/audible/titles?account="+testAccount, b.adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %s", resp.StatusCode, out)
	}
	var page struct {
		Titles []map[string]json.RawMessage `json:"titles"`
	}
	decodeInto(t, out, &page)
	byASIN := map[string]map[string]json.RawMessage{}
	for _, x := range page.Titles {
		var asin string
		_ = json.Unmarshal(x["asin"], &asin)
		byASIN[asin] = x
	}
	compact := func(raw json.RawMessage) string {
		var v any
		_ = json.Unmarshal(raw, &v)
		s, _ := json.Marshal(v)
		return string(s)
	}
	if got := compact(byASIN["B000000002"]["planned"]); got != compact(json.RawMessage(strings.TrimPrefix(plan, `"planned":`))) {
		t.Errorf("planned = %s", got)
	}
	wantContest := fmt.Sprintf(`{"library_id":%d,"path":%q,"with":[{"asin":"B000000001","title":"The Final Empire"}]}`, b.lib.ID, mistbornBook)
	if got := compact(byASIN["B000000003"]["contested"]); got != compact(json.RawMessage(wantContest)) {
		t.Errorf("contested = %s, want %s", got, wantContest)
	}
	for asin, x := range byASIN {
		if _, ok := x["contested_path"]; ok {
			t.Errorf("%s shows contested_path", asin)
		}
	}
	if p, c := string(byASIN["B000000001"]["planned"]), string(byASIN["B000000001"]["contested"]); p != "null" || c != "null" {
		t.Errorf("the matched title: planned %s, contested %s; want null, null", p, c)
	}
}

// Security-relevant input: a planned destination or contested book the
// server can't trust is refused (400), whole; a sound one is stored.
func TestAudiblePlannedRefused(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	if code, out := b.putAccount(t, testAccount, b.adminID, b.lib.ID); code != http.StatusOK {
		t.Fatalf("put account: %d %s", code, out)
	}
	base := title("B000000002", "The Well of Ascension", "available", 0, "")
	plan := func(path, kind, extra string) string {
		return fmt.Sprintf(`"planned":{"library_id":%d,"path":%q,"file":"B.m4b","kind":%q,"siblings":0,"state":"new","conflict":"","template":""%s}`,
			b.lib.ID, path, kind, extra)
	}
	for _, tc := range []struct {
		name, extra string
		ok          bool
	}{
		{"sound", plan("A/B/B.m4b", "standalone", ""), true},
		{"sound contest", `"contested_path":"A/B"`, true},
		{"an unknown field", plan("A/B/B.m4b", "standalone", `,"owner":"x"`), false},
		{"an unknown kind", plan("A/B/B.m4b", "elsewhere", ""), false},
		{"a path out of the library", plan("../B/B.m4b", "standalone", ""), false},
		{"an absolute path", plan("/etc/B.m4b", "standalone", ""), false},
		{"a contest out of the library", `"contested_path":"A/../../B"`, false},
		{"a contest that isn't text", `"contested_path":7`, false},
	} {
		code, out := b.putTitles(t, testAccount, "p1", false, withExtra(base, tc.extra))
		if tc.ok != (code == http.StatusOK) || (!tc.ok && (code != http.StatusBadRequest || codeOf(out) != "invalid_request")) {
			t.Errorf("%s: %d %s", tc.name, code, out)
		}
	}
}

// A poll carries its account's template, so the Bridge plans by it: Check now
// and the scheduler alike; omitted for the automatic placement.
func TestAudiblePollCarriesTemplate(t *testing.T) {
	t.Parallel()
	b := newBridgeEnv(t)
	ctx := context.Background()
	setAudible(t, b.testEnv, b.adminTok, true)
	b.beat(t, bridge.KindVerify, bridge.KindAudiblePoll)
	for _, id := range []string{testAccount, "acct0002"} {
		if code, out := b.putAccount(t, id, b.adminID, b.lib.ID); code != http.StatusOK {
			t.Fatalf("put %s: %d %s", id, code, out)
		}
	}
	const tmpl = "{Author}/{Title}"
	if resp, out := b.do(t, "PATCH", "/api/v1/admin/audible/accounts/"+testAccount, b.adminTok,
		fmt.Sprintf(`{"template":%q}`, tmpl)); resp.StatusCode != http.StatusOK {
		t.Fatalf("template: %d %s", resp.StatusCode, out)
	}
	resp, out := b.do(t, "POST", "/api/v1/admin/audible/accounts/"+testAccount+"/poll", b.adminTok, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("Check now: %d %s", resp.StatusCode, out)
	}
	var job bridge.Job
	decodeInto(t, out, &job)
	if want := fmt.Sprintf(`{"account":%q,"template":%q}`, testAccount, tmpl); string(job.Params) != want {
		t.Fatalf("Check now's params = %s, want %s", job.Params, want)
	}
	d := modules.Deps{Log: slog.New(slog.DiscardHandler), Catalog: b.cat, Bridge: b.api.bridge}
	if err := audible.SchedulerTick(ctx, d, time.Now()); err != nil {
		t.Fatal(err)
	}
	var params string
	if err := b.db.QueryRowContext(ctx, `SELECT params FROM bridge_jobs WHERE kind = ? AND dedupe_key LIKE '%acct0002'`,
		bridge.KindAudiblePoll).Scan(&params); err != nil || params != `{"account":"acct0002"}` {
		t.Fatalf("the scheduler's poll = %s, %v (no template: omitted)", params, err)
	}
	if err := b.cat.MarkAudibleQueued(ctx, "acct0002"); err != nil {
		t.Fatal(err)
	}
	due, err := b.cat.DueAudiblePolls(ctx, time.Now().Add(7*time.Hour), 10)
	if err != nil || len(due) != 2 || due[0].Account != testAccount || due[0].Template != tmpl || due[1].Template != "" {
		t.Fatalf("due = %+v, %v", due, err)
	}
}
