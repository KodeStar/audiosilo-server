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

func TestSupportCardFirstRunHidden(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	if supportShows(t, e, adminTok) {
		t.Fatal("the card shows on a new server")
	}
}

// "I've donated" hides the card for every admin on the server, for good, and is
// audited; a later "Not now" doesn't bring it back in six months.
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
	if resp, body := e.do(t, "POST", "/api/v1/admin/support", otherTok, `{"action":"snooze"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("snooze = %d %s", resp.StatusCode, body)
	}
	if events := settingsAudit(t, e); len(events) != 1 {
		t.Fatalf("a no-op snooze was audited: %+v", events)
	}
}

func TestSupportCardSnooze(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	backdateServer(t, e, (catalog.SupportAfterDays+1)*24*time.Hour)
	if resp, body := e.do(t, "POST", "/api/v1/admin/support", adminTok, `{"action":"snooze"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("snooze = %d %s", resp.StatusCode, body)
	}
	if supportShows(t, e, adminTok) {
		t.Fatal("the card shows while snoozed")
	}
	if events := settingsAudit(t, e); len(events) != 1 || events[0].Details["choice"] != "snoozed" ||
		events[0].Details["returns_at"] == nil {
		t.Fatalf("audit = %+v", events)
	}

	// The snooze's end moved into the past: the card is back.
	if _, err := e.db.ExecContext(context.Background(),
		`UPDATE server_state SET value = json_set(value, '$.until', '2020-01-01T00:00:00Z') WHERE key = 'support_card'`); err != nil {
		t.Fatal(err)
	}
	if !supportShows(t, e, adminTok) {
		t.Fatal("the card didn't come back after the snooze")
	}
}

func TestSupportCardBadAction(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	for _, body := range []string{`{"action":"later"}`, `{}`, `{"action":"donated","extra":1}`, `nope`} {
		if resp, _ := e.do(t, "POST", "/api/v1/admin/support", adminTok, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400", body, resp.StatusCode)
		}
	}
}
