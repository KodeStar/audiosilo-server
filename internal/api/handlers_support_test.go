package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// backdateServer makes the server look age old: its accounts were made then.
func backdateServer(t *testing.T, e *testEnv, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age).UTC().Format(time.RFC3339Nano)
	if _, err := e.db.ExecContext(context.Background(), `UPDATE users SET created_at = ?`, at); err != nil {
		t.Fatal(err)
	}
}

func settingsAudit(t *testing.T, e *testEnv) []catalog.AuditEvent {
	t.Helper()
	events, _, err := e.cat.ListAudit(context.Background(), catalog.AuditFilter{Area: "settings"})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func supportShows(t *testing.T, e *testEnv, token string) bool {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/admin/support", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/support = %d %s", resp.StatusCode, body)
	}
	var env struct {
		Show *bool `json:"show"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Show == nil {
		t.Fatalf("body = %s (%v)", body, err)
	}
	return *env.Show
}

// The support card is the console's: a member and a signed-out caller can neither
// read nor answer it, and their tries change nothing.
func TestSupportCardAdminOnly(t *testing.T) {
	e := newTestEnv(t)
	adminTok, memberTok := opsTokens(t, e)
	backdateServer(t, e, (catalog.SupportAfterDays+1)*24*time.Hour)

	for _, tc := range []struct{ method, body string }{
		{"GET", ""},
		{"POST", `{"action":"donated"}`},
	} {
		if resp, _ := e.do(t, tc.method, "/api/v1/admin/support", memberTok, tc.body); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s as a member = %d, want 403", tc.method, resp.StatusCode)
		}
		if resp, _ := e.do(t, tc.method, "/api/v1/admin/support", "", tc.body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s signed out = %d, want 401", tc.method, resp.StatusCode)
		}
	}
	if !supportShows(t, e, adminTok) {
		t.Fatal("a refused answer hid the card")
	}
}

// "I've donated" hides the card for every admin on the server and is audited
// once; a later "Not now" changes nothing, so it isn't audited.
func TestSupportCardDonated(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	other, err := e.auth.CreateUser(context.Background(), "second", "second-password", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	otherTok, _ := e.auth.IssueToken(context.Background(), other.ID, auth.KindSession, "t", 0)
	backdateServer(t, e, (catalog.SupportAfterDays+1)*24*time.Hour)
	if !supportShows(t, e, adminTok) || !supportShows(t, e, otherTok) {
		t.Fatal("the card doesn't show on a month-old server")
	}

	resp, body := e.do(t, "POST", "/api/v1/admin/support", adminTok, `{"action":"donated"}`)
	if resp.StatusCode != http.StatusOK || body != "{\"show\":false}\n" {
		t.Fatalf("donated = %d %s", resp.StatusCode, body)
	}
	if supportShows(t, e, adminTok) || supportShows(t, e, otherTok) {
		t.Fatal("the card still shows after a donation")
	}
	events := settingsAudit(t, e)
	if len(events) != 1 || events[0].Action != "settings.support" ||
		events[0].Details["choice"] != "donated" || events[0].Details["returns_at"] != nil {
		t.Fatalf("audit = %+v", events)
	}

	// Not now after a donation changes nothing (and so audits nothing).
	if resp, body := e.do(t, "POST", "/api/v1/admin/support", otherTok, `{"action":"snoozed"}`); resp.StatusCode != http.StatusOK || body != "{\"show\":false}\n" {
		t.Fatalf("snoozed after donating = %d %s", resp.StatusCode, body)
	}
	if events := settingsAudit(t, e); len(events) != 1 {
		t.Fatalf("a no-op snooze was audited: %+v", events)
	}
}

// "Not now" answers with the snooze's end, and audits it as returns_at.
func TestSupportCardSnoozed(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	backdateServer(t, e, (catalog.SupportAfterDays+1)*24*time.Hour)
	resp, body := e.do(t, "POST", "/api/v1/admin/support", adminTok, `{"action":"snoozed"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("snoozed = %d %s", resp.StatusCode, body)
	}
	var env struct {
		Show  bool   `json:"show"`
		Until string `json:"until"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	until, err := time.Parse(time.RFC3339, env.Until)
	if err != nil || env.Show || until.Before(time.Now().AddDate(0, catalog.SupportSnoozeMonths, -1)) {
		t.Fatalf("snoozed = %s", body)
	}
	if supportShows(t, e, adminTok) {
		t.Fatal("the card shows while snoozed")
	}
	if events := settingsAudit(t, e); len(events) != 1 || events[0].Details["choice"] != "snoozed" ||
		events[0].Details["returns_at"] != env.Until {
		t.Fatalf("audit = %+v", events)
	}
}

func TestSupportCardBadAction(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	for _, body := range []string{`{"action":"later"}`, `{"action":"snooze"}`, `{}`, `{"action":"donated","extra":1}`, `nope`} {
		if resp, _ := e.do(t, "POST", "/api/v1/admin/support", adminTok, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400", body, resp.StatusCode)
		}
	}
}
