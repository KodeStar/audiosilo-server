package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// Security-critical (server#285 Q21): ending a person's API keys and calendar
// link is admin-only. A person on their own account, a household owner on
// their member, a kid, a member and a non-admin's API key are all refused and
// end nothing; the admin's call ends every key and the link of that person on
// the very next request, and nothing else: their sessions, another person's
// keys and link, and their household member's key stay.
func TestEndKeysRoute(t *testing.T) {
	t.Parallel()
	m := newMemberEnv(t)
	ctx := context.Background()
	adamPath := "/api/v1/admin/users/" + strconv.FormatInt(m.adam, 10) + "/end-keys"
	ellis, ellisTok := m.grownup(t, m.adamTok, "Ellis", "all")
	_, evieTok := m.kid(t, m.adamTok, "Evie")
	_, adamKey := m.mintAPIKey(t, m.adamTok, "dash")
	_, adamKey2 := m.mintAPIKey(t, m.adamTok, "cron")
	_, ellisKey := m.mintAPIKey(t, ellisTok, "ellis dash")
	_, bobKey := m.mintAPIKey(t, m.bobTok, "bob dash")
	adamFeed, _, err := m.auth.IssueFeedToken(ctx, m.adam, m.credentialID(t, m.adamTok))
	if err != nil {
		t.Fatal(err)
	}
	bobFeed, _, err := m.auth.IssueFeedToken(ctx, m.bob, m.credentialID(t, m.bobTok))
	if err != nil {
		t.Fatal(err)
	}
	me := func(tok string) int { return m.status(t, "GET", "/api/v1/me", tok, "") }
	feedLive := func(secret string) bool {
		resp, _ := m.do(t, "GET", feedURL(secret), "", "")
		return resp.StatusCode != http.StatusUnauthorized
	}
	adamLive := func() bool {
		t.Helper()
		keys := me(adamKey) == http.StatusOK
		if keys != (me(adamKey2) == http.StatusOK) {
			t.Fatal("adam's two keys disagree")
		}
		if keys != feedLive(adamFeed) {
			t.Fatalf("adam's keys live %v, his calendar link %v", keys, !keys)
		}
		return keys
	}
	if !adamLive() {
		t.Fatal("setup: adam's keys don't work")
	}

	// Denied: anyone but an admin, as a session or a key, and a bad or unknown id.
	for _, d := range []struct {
		name, path, tok string
		want            int
	}{
		{"adam on his own account", adamPath, m.adamTok, http.StatusForbidden},
		{"adam's key on his own account", adamPath, adamKey, http.StatusForbidden},
		{"the household owner on his member", "/api/v1/admin/users/" + strconv.FormatInt(ellis.ID, 10) + "/end-keys", m.adamTok, http.StatusForbidden},
		{"the member on her owner", adamPath, ellisTok, http.StatusForbidden},
		{"a kid", adamPath, evieTok, http.StatusForbidden},
		{"another person", adamPath, m.bobTok, http.StatusForbidden},
		{"no credential", adamPath, "", http.StatusUnauthorized},
		{"a bad id", "/api/v1/admin/users/x/end-keys", m.adminTok, http.StatusBadRequest},
		{"an unknown id", "/api/v1/admin/users/9999/end-keys", m.adminTok, http.StatusNotFound},
	} {
		if got := m.status(t, "POST", d.path, d.tok, ""); got != d.want {
			t.Errorf("%s = %d, want %d", d.name, got, d.want)
		}
	}
	if !adamLive() {
		t.Fatal("a refused call ended adam's keys")
	}
	if me(ellisKey) != http.StatusOK {
		t.Fatal("a refused call ended the member's key")
	}

	// The console sees the link before.
	if !calendarLinkActive(t, m.testEnv, m.adam) {
		t.Fatal("detail: calendar_link.active = false, want true")
	}

	// Allowed: the admin.
	resp, body := m.do(t, "POST", adamPath, m.adminTok, "")
	var out struct {
		APIKeys      *int  `json:"api_keys"`
		CalendarLink *bool `json:"calendar_link"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil ||
		out.APIKeys == nil || *out.APIKeys != 2 || out.CalendarLink == nil || !*out.CalendarLink {
		t.Fatalf("end keys = %d %s, want 200 api_keys 2 calendar_link true", resp.StatusCode, body)
	}
	if adamLive() {
		t.Fatal("adam's keys and link still work")
	}
	if me(m.adamTok) != http.StatusOK {
		t.Fatal("adam's session ended with his keys")
	}
	if me(ellisKey) != http.StatusOK || me(ellisTok) != http.StatusOK {
		t.Fatal("the member's key or session ended with her owner's keys")
	}
	if me(bobKey) != http.StatusOK || !feedLive(bobFeed) {
		t.Fatal("another person's key or link ended")
	}
	if calendarLinkActive(t, m.testEnv, m.adam) {
		t.Fatal("detail: calendar_link.active = true after it ended")
	}
	var audited int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE action = 'user.end_keys' AND target = 'adam'`).
		Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("audit rows = %d %v, want 1", audited, err)
	}

	// Again: nothing left to end.
	resp, body = m.do(t, "POST", adamPath, m.adminTok, "")
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil || *out.APIKeys != 0 || *out.CalendarLink {
		t.Fatalf("second end keys = %d %s, want 200 api_keys 0 calendar_link false", resp.StatusCode, body)
	}
}

// calendarLinkActive reads GET /admin/users/{id}'s calendar_link.active.
func calendarLinkActive(t *testing.T, e *testEnv, id int64) bool {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/admin/users/"+strconv.FormatInt(id, 10), adminSession(t, e), "")
	var out struct {
		CalendarLink *struct {
			Active bool `json:"active"`
		} `json:"calendar_link"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil || out.CalendarLink == nil {
		t.Fatalf("user detail = %d %s, want a calendar_link", resp.StatusCode, body)
	}
	return out.CalendarLink.Active
}
