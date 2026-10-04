package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Every Phase 5b endpoint is admin-only: signed out is 401, a member 403, an admin
// served.
func TestPhase5bEndpointsRequireAdmin(t *testing.T) {
	e := newTestEnv(t)
	adminTok, memberTok := opsTokens(t, e)
	b, err := e.backups.Create(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	tg, err := e.cat.CreateNotifyTarget(context.Background(), catalog.NotifyTarget{
		Kind: "webhook", Name: "Hook", URL: "http://127.0.0.1:1/x", Enabled: true, Events: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(tg.ID, 10)
	for _, tc := range []struct {
		method, path, body string
		ok                 int
	}{
		{"GET", "/api/v1/admin/audit", "", 200},
		{"GET", "/api/v1/admin/events", "", 200},
		{"GET", "/api/v1/admin/backups", "", 200},
		{"GET", "/api/v1/admin/backups/" + b.Name, "", 200},
		{"POST", "/api/v1/admin/backups/" + b.Name + "/restore", "", 200},
		{"DELETE", "/api/v1/admin/restore", "", 204},
		{"GET", "/api/v1/admin/notifications", "", 200},
		{"PATCH", "/api/v1/admin/notifications/" + id, `{"name":"Hook 2"}`, 200},
		{"POST", "/api/v1/admin/notifications/" + id + "/test", "", 200},
		{"POST", "/api/v1/admin/notifications", `{"kind":"ntfy","name":"n","url":"https://ntfy.sh/abc","events":[]}`, 201},
		{"DELETE", "/api/v1/admin/notifications/" + id, "", 204},
		{"DELETE", "/api/v1/admin/backups/" + b.Name, "", 204},
		{"POST", "/api/v1/admin/backups", "", 202},
	} {
		if resp, _ := e.do(t, tc.method, tc.path, memberTok, tc.body); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s as a member = %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
		if resp, _ := e.do(t, tc.method, tc.path, "", tc.body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s signed out = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
		if resp, body := e.do(t, tc.method, tc.path, adminTok, tc.body); resp.StatusCode != tc.ok {
			t.Errorf("%s %s as admin = %d %s, want %d", tc.method, tc.path, resp.StatusCode, body, tc.ok)
		}
	}
}

type backupsEnv struct {
	Backups []struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		Size int64  `json:"size"`
	} `json:"backups"`
	Status struct {
		Dir     string `json:"dir"`
		Running bool   `json:"running"`
		Last    *struct {
			OK      bool   `json:"ok"`
			Trigger string `json:"trigger"`
		} `json:"last"`
		Next *string `json:"next"`
	} `json:"status"`
	Restore struct {
		Pending *struct {
			Name        string `json:"name"`
			RequestedBy string `json:"requested_by"`
		} `json:"pending"`
		Last any `json:"last"`
	} `json:"restore"`
}

func (e *testEnv) backupList(t *testing.T, tok string) backupsEnv {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/admin/backups", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}
	var env backupsEnv
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestBackupLifecycle(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)

	if env := e.backupList(t, adminTok); len(env.Backups) != 0 || env.Status.Dir == "" {
		t.Fatalf("fresh list = %+v", env)
	}
	if resp, body := e.do(t, "POST", "/api/v1/admin/backups", adminTok, ""); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var env backupsEnv
	for range 100 {
		if env = e.backupList(t, adminTok); len(env.Backups) == 1 && !env.Status.Running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(env.Backups) != 1 || env.Backups[0].Kind != "manual" || env.Status.Last == nil || !env.Status.Last.OK {
		t.Fatalf("after create = %+v", env)
	}
	name := env.Backups[0].Name

	// Downloads stream the file itself, as an attachment, never cached.
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/admin/backups/"+name, nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(string(data), "SQLite format 3\x00") ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="`+name+`"` ||
		resp.Header.Get("Cache-Control") != "no-store" || int64(len(data)) != env.Backups[0].Size {
		t.Fatalf("download = %d %v (%d bytes)", resp.StatusCode, resp.Header, len(data))
	}

	// A restore is marked for the next start, and cancelled.
	if resp, body := e.do(t, "POST", "/api/v1/admin/backups/"+name+"/restore", adminTok, ""); resp.StatusCode != 200 {
		t.Fatalf("restore = %d %s", resp.StatusCode, body)
	}
	if env = e.backupList(t, adminTok); env.Restore.Pending == nil || env.Restore.Pending.Name != name ||
		env.Restore.Pending.RequestedBy != "admin" {
		t.Fatalf("pending = %+v", env.Restore)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/admin/restore", adminTok, ""); resp.StatusCode != 204 {
		t.Fatalf("cancel = %d", resp.StatusCode)
	}
	if env = e.backupList(t, adminTok); env.Restore.Pending != nil {
		t.Fatal("restore still pending")
	}

	// Junk in the folder under a backup's name is refused for a restore.
	junk := "audiosilo-junk.db"
	if err := os.WriteFile(filepath.Join(env.Status.Dir, junk), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, body := e.do(t, "POST", "/api/v1/admin/backups/"+junk+"/restore", adminTok, ""); resp.StatusCode != 400 ||
		!strings.Contains(body, `"invalid_backup"`) {
		t.Fatalf("junk restore = %d %s", resp.StatusCode, body)
	}

	// Names never leave the folder.
	for _, bad := range []string{"..%2Faudiosilo.db", "audiosilo.db", "config.yaml", "audiosilo-..%2F..%2Fx.db"} {
		for _, m := range []string{"GET", "DELETE"} {
			if resp, _ := e.do(t, m, "/api/v1/admin/backups/"+bad, adminTok, ""); resp.StatusCode != 404 {
				t.Errorf("%s %s = %d, want 404", m, bad, resp.StatusCode)
			}
		}
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/admin/backups/"+name, adminTok, ""); resp.StatusCode != 204 {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/backups/"+name, adminTok, ""); resp.StatusCode != 404 {
		t.Fatalf("deleted backup still served: %d", resp.StatusCode)
	}

	// Each step is in the audit log.
	events, _, err := e.cat.ListAudit(context.Background(), catalog.AuditFilter{Area: "backup"})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, ev := range events {
		actions = append(actions, ev.Action)
	}
	want := "backup.delete,backup.restore_cancel,backup.restore,backup.download,backup.create"
	if strings.Join(actions, ",") != want {
		t.Fatalf("audit = %v, want %s", actions, want)
	}
}

func TestBackupSettings(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	_, body := e.do(t, "GET", "/api/v1/admin/settings", adminTok, "")
	if !strings.Contains(body, `"backups":{"dir":"","keep":7,"schedule":"daily:03:00"}`) {
		t.Fatalf("settings = %s", body)
	}
	resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"backups":{"schedule":"weekly:sun:04:30","keep":3}}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"schedule":"weekly:sun:04:30"`) {
		t.Fatalf("patch = %d %s", resp.StatusCode, body)
	}
	if st := e.backups.Status(); st.Next == nil || st.Next.Weekday() != time.Sunday {
		t.Fatalf("the service didn't take the new schedule: %+v", st)
	}
	for _, tc := range []struct{ body, code, field string }{
		{`{"backups":{"schedule":"hourly"}}`, "invalid_setting", "backups.schedule"},
		{`{"backups":{"keep":0}}`, "invalid_setting", "backups.keep"},
		{`{"backups":{"keep":366}}`, "invalid_setting", "backups.keep"},
		{`{"backups":{"dir":"/tmp/elsewhere"}}`, "setting_read_only", "backups.dir"},
	} {
		resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, tc.body)
		if resp.StatusCode != 400 || !strings.Contains(body, `"code":"`+tc.code+`"`) ||
			!strings.Contains(body, `"field":"`+tc.field+`"`) {
			t.Errorf("%s = %d %s", tc.body, resp.StatusCode, body)
		}
	}
	// The save is in the audit log with what changed.
	events, _, _ := e.cat.ListAudit(context.Background(), catalog.AuditFilter{Area: "settings"})
	if len(events) != 1 {
		t.Fatalf("settings audit = %+v", events)
	}
	changes, _ := json.Marshal(events[0].Details["changes"])
	if !strings.Contains(string(changes), `{"from":"daily:03:00","setting":"backups.schedule","to":"weekly:sun:04:30"}`) ||
		!strings.Contains(string(changes), `{"from":7,"setting":"backups.keep","to":3}`) {
		t.Fatalf("changes = %s", changes)
	}
	// Health > System reports the backups.
	_, body = e.do(t, "GET", "/api/v1/admin/system", adminTok, "")
	if !strings.Contains(body, `"backups":{"dir":`) {
		t.Fatalf("system = %s", body)
	}
}

func TestNotificationTargetsNeverEchoCredentials(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	var got []string
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("X-AudioSilo-Event"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer dest.Close()
	secretURL := dest.URL + "/hooks/very-secret-token"

	resp, body := e.do(t, "POST", "/api/v1/admin/notifications", adminTok,
		`{"kind":"webhook","name":"Home Assistant","url":"`+secretURL+`","secret":"sign-me","events":["book_added","new_device"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "very-secret-token") || strings.Contains(body, "sign-me") ||
		!strings.Contains(body, `"address":"`+dest.URL+`/hooks/very…"`) || !strings.Contains(body, `"has_secret":true`) {
		t.Fatalf("create answer = %s", body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &created)
	id := strconv.FormatInt(created.ID, 10)
	_, body = e.do(t, "GET", "/api/v1/admin/notifications", adminTok, "")
	if strings.Contains(body, "very-secret-token") || strings.Contains(body, "sign-me") ||
		!strings.Contains(body, `"events":["book_added","scan_failed"`) {
		t.Fatalf("list = %s", body)
	}

	// A change keeps the address and secret unless they are sent; the kind can't change.
	resp, body = e.do(t, "PATCH", "/api/v1/admin/notifications/"+id, adminTok, `{"events":["scan_failed"],"enabled":false}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"has_secret":true`) || !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("patch = %d %s", resp.StatusCode, body)
	}
	stored, _ := e.cat.GetNotifyTarget(context.Background(), created.ID)
	if stored.URL != secretURL || stored.Secret != "sign-me" {
		t.Fatalf("stored = %+v", stored)
	}
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/notifications/"+id, adminTok, `{"kind":"ntfy"}`); resp.StatusCode != 400 ||
		!strings.Contains(body, `"field":"kind"`) {
		t.Fatalf("kind change = %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/notifications/"+id, adminTok, `{"secret":""}`); resp.StatusCode != 200 ||
		!strings.Contains(body, `"has_secret":false`) {
		t.Fatalf("clear secret = %d %s", resp.StatusCode, body)
	}
	for _, bad := range []struct{ body, field string }{
		{`{"kind":"discord","name":"d","url":"https://evil.example/api/webhooks/1/x","events":[]}`, "url"},
		{`{"kind":"webhook","name":"","url":"https://a.example","events":[]}`, "name"},
		{`{"kind":"webhook","name":"x","url":"https://a.example","events":["everything"]}`, "events"},
		{`{"kind":"sms","name":"x","url":"https://a.example","events":[]}`, "kind"},
	} {
		if resp, body := e.do(t, "POST", "/api/v1/admin/notifications", adminTok, bad.body); resp.StatusCode != 400 ||
			!strings.Contains(body, `"code":"invalid_target"`) || !strings.Contains(body, `"field":"`+bad.field+`"`) {
			t.Errorf("%s = %d %s", bad.body, resp.StatusCode, body)
		}
	}

	// Send test reaches it even while it is off, and records the outcome.
	resp, body = e.do(t, "POST", "/api/v1/admin/notifications/"+id+"/test", adminTok, "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"last_ok":true`) ||
		len(got) != 1 || got[0] != "test" {
		t.Fatalf("test = %d %s (got %v)", resp.StatusCode, body, got)
	}
	if resp, _ := e.do(t, "PATCH", "/api/v1/admin/notifications/999", adminTok, `{"name":"x"}`); resp.StatusCode != 404 {
		t.Fatalf("unknown id = %d", resp.StatusCode)
	}

	// The audit log says what changed, never the address or secret.
	events, _, _ := e.cat.ListAudit(context.Background(), catalog.AuditFilter{Area: "notify"})
	raw, _ := json.Marshal(events)
	if len(events) != 3 || strings.Contains(string(raw), "very-secret-token") || strings.Contains(string(raw), "sign-me") {
		t.Fatalf("audit = %s", raw)
	}
}

func TestNotificationTargetLimit(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	for i := range catalog.MaxNotifyTargets {
		if resp, body := e.do(t, "POST", "/api/v1/admin/notifications", adminTok,
			`{"kind":"ntfy","name":"n`+strconv.Itoa(i)+`","url":"https://ntfy.sh/t`+strconv.Itoa(i)+`","events":[]}`); resp.StatusCode != 201 {
			t.Fatalf("create %d = %d %s", i, resp.StatusCode, body)
		}
	}
	if resp, body := e.do(t, "POST", "/api/v1/admin/notifications", adminTok,
		`{"kind":"ntfy","name":"one more","url":"https://ntfy.sh/more","events":[]}`); resp.StatusCode != 409 ||
		!strings.Contains(body, "too_many_targets") {
		t.Fatalf("over the limit = %d %s", resp.StatusCode, body)
	}
}

func TestSignInsReachTheFeed(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	resp, _ := e.doHeaders(t, "POST", "/api/v1/auth/login", "", `{"username":"member","password":"member-password","device_name":"Sam's Mac"}`,
		map[string]string{auth.ClientHeader: "AudioSilo/1.4.0 (web)"})
	if resp.StatusCode != 200 {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	// An invite pairs a device: a new device and the invite being used.
	ctx := context.Background()
	sam, _ := e.auth.CreateUser(ctx, "sam", "", auth.RoleUser)
	invite, _ := e.auth.CreateAuthCode(ctx, sam.ID, "invite", 5, 0)
	_, pairing, _ := e.redeemCode(t, invite)
	if status, body := e.exchangeToken(t, pairing, "Sam's iPhone"); status != 200 {
		t.Fatalf("exchange = %d %s", status, body)
	}
	// A failed sign-in is not a new device.
	e.do(t, "POST", "/api/v1/auth/login", "", `{"username":"member","password":"wrong"}`)

	_, body := e.do(t, "GET", "/api/v1/admin/events?limit=10", adminTok, "")
	var feed struct {
		Events []catalog.ServerEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &feed); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, ev := range feed.Events {
		kinds = append(kinds, ev.Kind)
	}
	if strings.Join(kinds, ",") != "invite_redeemed,new_device,new_device" {
		t.Fatalf("feed = %v", kinds)
	}
	if d := feed.Events[2].Data; d["user"] != "member" || d["device"] != "Sam's Mac" || d["app"] != "AudioSilo 1.4.0" {
		t.Fatalf("sign-in data = %v", d)
	}
	if d := feed.Events[0].Data; d["user"] != "sam" || d["device"] != "Sam's iPhone" {
		t.Fatalf("invite data = %v", d)
	}
	if strings.Contains(body, "127.0.0.1") {
		t.Fatal("the feed carries an address")
	}
}

func TestAuditLogRecordsAdminActions(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	resp, body := e.do(t, "POST", "/api/v1/admin/users", adminTok, `{"username":"jo","password":"a-long-password","role":"user"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var jo struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &jo)
	joID := strconv.FormatInt(jo.ID, 10)
	e.do(t, "PATCH", "/api/v1/admin/users/"+joID, adminTok, `{"password":"another-long-password","disabled":true}`)
	e.do(t, "POST", "/api/v1/admin/users/"+joID+"/authcode", adminTok, `{"max_uses":3,"ttl_days":7}`)
	e.do(t, "DELETE", "/api/v1/admin/users/"+joID, adminTok, "")
	// A refused change records nothing.
	e.do(t, "DELETE", "/api/v1/admin/users/"+strconv.FormatInt(e.adminID, 10), adminTok, "")

	resp, body = e.do(t, "GET", "/api/v1/admin/audit", adminTok, "")
	if resp.StatusCode != 200 {
		t.Fatalf("audit = %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "a-long-password") || strings.Contains(body, "another-long-password") {
		t.Fatal("a password reached the audit log")
	}
	var page struct {
		Events     []catalog.AuditEvent `json:"events"`
		NextBefore int64                `json:"next_before"`
	}
	_ = json.Unmarshal([]byte(body), &page)
	var got []string
	for _, ev := range page.Events {
		got = append(got, ev.Action+" "+ev.Target)
		if ev.ActorName != "admin" || ev.ActorID == nil || *ev.ActorID != e.adminID || ev.Via != "session" {
			t.Fatalf("actor = %+v", ev)
		}
	}
	want := "user.delete jo,invite.create jo,user.update jo,user.create jo"
	if strings.Join(got, ",") != want || page.NextBefore != 0 {
		t.Fatalf("audit = %v (next %d), want %s", got, page.NextBefore, want)
	}
	if d := page.Events[2].Details; d["password"] != "set" || d["disabled"] != true {
		t.Fatalf("update details = %v", d)
	}
	if d := page.Events[1].Details; d["max_uses"] != float64(3) || d["expires_at"] == nil {
		t.Fatalf("invite details = %v", d)
	}

	// Paging, area and search filters; a bad filter is a 400.
	_, body = e.do(t, "GET", "/api/v1/admin/audit?limit=2", adminTok, "")
	_ = json.Unmarshal([]byte(body), &page)
	if len(page.Events) != 2 || page.NextBefore == 0 {
		t.Fatalf("page 1 = %s", body)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/audit?limit=2&before="+strconv.FormatInt(page.NextBefore, 10), adminTok, "")
	_ = json.Unmarshal([]byte(body), &page)
	if len(page.Events) != 2 || page.Events[1].Action != "user.create" || page.NextBefore != 0 {
		t.Fatalf("page 2 = %s", body)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/audit?area=invite", adminTok, "")
	if !strings.Contains(body, "invite.create") || strings.Contains(body, "user.") {
		t.Fatalf("area filter = %s", body)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/audit?q=nobody", adminTok, "")
	if !strings.Contains(body, `"events":[]`) {
		t.Fatalf("search = %s", body)
	}
	for _, q := range []string{"area=User%25", "before=x", "actor_id=x"} {
		if resp, _ := e.do(t, "GET", "/api/v1/admin/audit?"+q, adminTok, ""); resp.StatusCode != 400 {
			t.Errorf("%s = %d, want 400", q, resp.StatusCode)
		}
	}
}

// An API key's actions are recorded as made with a key.
func TestAuditViaAPIKey(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	_, key := e.mintAPIKey(t, adminTok, "script")
	if resp, body := e.do(t, "POST", "/api/v1/admin/users", key, `{"username":"bot-made","role":"user"}`); resp.StatusCode != 201 {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	events, _, _ := e.cat.ListAudit(context.Background(), catalog.AuditFilter{})
	if len(events) != 1 || events[0].Via != catalog.ViaAPIKey {
		t.Fatalf("audit = %+v", events)
	}
}

func TestBackupDownloadOutlivesRequestTimeout(t *testing.T) {
	if !isStreamingPath("/api/v1/admin/backups/audiosilo-x.db") {
		t.Fatal("backup downloads are bound by the request timeout")
	}
	for _, p := range []string{"/api/v1/admin/backups", "/api/v1/admin/backups/", "/api/v1/admin/backups/audiosilo-x.db/restore"} {
		if isStreamingPath(p) {
			t.Fatalf("%s escaped the request timeout", p)
		}
	}
}

// A saved secret stays with the server it was given for: moving the address to
// another server needs it again (or cleared), a path change on the same one doesn't.
func TestNotificationSecretStaysWithItsServer(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	resp, body := e.do(t, "POST", "/api/v1/admin/notifications", adminTok,
		`{"kind":"ntfy","name":"Phone","url":"https://ntfy.example/alerts","secret":"tk_access","events":["scan_failed"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &created)
	id := strconv.FormatInt(created.ID, 10)

	// Denied: another server, secret not sent again.
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/notifications/"+id, adminTok, `{"url":"https://evil.example/alerts"}`); resp.StatusCode != 400 ||
		!strings.Contains(body, `"field":"secret"`) {
		t.Fatalf("move without secret = %d %s", resp.StatusCode, body)
	}
	if stored, _ := e.cat.GetNotifyTarget(context.Background(), created.ID); stored.URL != "https://ntfy.example/alerts" {
		t.Fatalf("a refused change was saved: %+v", stored)
	}
	// Allowed: the same server under another topic, or another server with the secret cleared.
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/notifications/"+id, adminTok, `{"url":"https://NTFY.example/other"}`); resp.StatusCode != 200 ||
		!strings.Contains(body, `"has_secret":true`) {
		t.Fatalf("same server = %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/notifications/"+id, adminTok, `{"url":"https://other.example/alerts","secret":""}`); resp.StatusCode != 200 ||
		!strings.Contains(body, `"has_secret":false`) {
		t.Fatalf("move with secret cleared = %d %s", resp.StatusCode, body)
	}
}

// A save that changes nothing isn't an admin change.
func TestNoOpSettingsSaveIsNotAudited(t *testing.T) {
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"backups":{"keep":7}}`); resp.StatusCode != 200 {
		t.Fatalf("patch = %d %s", resp.StatusCode, body)
	}
	if events, _, _ := e.cat.ListAudit(context.Background(), catalog.AuditFilter{Area: "settings"}); len(events) != 0 {
		t.Fatalf("audited %+v", events)
	}
}
