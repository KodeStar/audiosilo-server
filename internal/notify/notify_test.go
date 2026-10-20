package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

func TestClean(t *testing.T) {
	ok := []struct{ kind, url, secret string }{
		{TargetWebhook, "https://hooks.example.com/audiosilo?key=1", "s3cret"},
		{TargetWebhook, "http://192.168.1.5:8123/api/webhook/abc", ""},
		{TargetNtfy, "https://ntfy.sh/hearthside-books", ""},
		{TargetNtfy, "https://push.example.com/ntfy/books_1/", "tk_abc"},
		{TargetDiscord, "https://discord.com/api/webhooks/123456/AbC-dEf_9", ""},
		{TargetDiscord, "https://discordapp.com/api/v10/webhooks/1/x", ""},
	}
	for _, c := range ok {
		kind, name, u, secret, events := c.kind, "  Home  ", c.url, c.secret, []string{KindBookAdded, KindBookAdded}
		if err := Clean(&kind, &name, &u, &secret, &events); err != nil {
			t.Errorf("Clean(%s %s) = %v", c.kind, c.url, err)
			continue
		}
		if name != "Home" || len(events) != 1 {
			t.Errorf("not normalized: %q %v", name, events)
		}
	}
	bad := []struct{ kind, name, url, secret, field string }{
		{"email", "x", "https://a.example", "", "kind"},
		{TargetWebhook, "", "https://a.example", "", "name"},
		{TargetWebhook, strings.Repeat("n", 65), "https://a.example", "", "name"},
		{TargetWebhook, "a\nb", "https://a.example", "", "name"},
		{TargetWebhook, "x", "ftp://a.example/x", "", "url"},
		{TargetWebhook, "x", "/relative", "", "url"},
		{TargetWebhook, "x", "https://a.example/#frag", "", "url"},
		{TargetWebhook, "x", "https://a.example/\x00", "", "url"},
		{TargetWebhook, "x", "https://a.example", strings.Repeat("s", 257), "secret"},
		{TargetWebhook, "x", "https://a.example", "a\nb", "secret"},
		{TargetNtfy, "x", "https://ntfy.sh/", "", "url"},
		{TargetNtfy, "x", "https://ntfy.sh/bad topic!", "", "url"},
		{TargetNtfy, "x", "https://ntfy.sh/t?x=1", "", "url"},
		{TargetDiscord, "x", "https://evil.example/api/webhooks/1/x", "", "url"},
		{TargetDiscord, "x", "http://discord.com/api/webhooks/1/x", "", "url"},
		{TargetDiscord, "x", "https://discord.com/channels/1/2", "", "url"},
		{TargetDiscord, "x", "https://discord.com/api/webhooks/1/x", "secret", "secret"},
	}
	for _, c := range bad {
		kind, name, u, secret, events := c.kind, c.name, c.url, c.secret, []string{}
		err := Clean(&kind, &name, &u, &secret, &events)
		fe, isField := err.(*FieldError)
		if !isField || fe.Field != c.field {
			t.Errorf("Clean(%q %q %q) = %v, want a %s error", c.kind, c.name, c.url, err, c.field)
		}
	}
	kind, name, u, secret, events := TargetWebhook, "x", "https://a.example", "", []string{"nope"}
	if err := Clean(&kind, &name, &u, &secret, &events); err == nil {
		t.Error("unknown event accepted")
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"https://discord.com/api/webhooks/123456/AbCdEfGhIj": "https://discord.com/api/webhooks/123456/AbCd…",
		"https://ntfy.sh/hearthside-books":                   "https://ntfy.sh/hear…",
		"https://user:pw@hooks.example.com/in?token=abc":     "https://hooks.example.com/…?…",
		"http://10.0.0.2:8123/x":                             "http://10.0.0.2:8123/…",
		"https://ntfy.sh/abcd":                               "https://ntfy.sh/…",
		"https://hooks.example.com":                          "https://hooks.example.com",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(Redact(in), "pw") || strings.Contains(Redact(in), "token=abc") {
			t.Errorf("Redact(%q) leaks a credential", in)
		}
	}
}

// received is what a fake destination saw.
type received struct {
	path    string
	headers http.Header
	body    map[string]any
	raw     []byte
}

type fakeDest struct {
	srv  *httptest.Server
	mu   sync.Mutex
	got  []received
	code int
}

func newFakeDest(t *testing.T) *fakeDest {
	f := &fakeDest{code: http.StatusNoContent}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.got = append(f.got, received{path: r.URL.Path, headers: r.Header.Clone(), body: body, raw: raw})
		code := f.code
		f.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDest) wait(t *testing.T, n int) []received {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.got) >= n {
			out := append([]received(nil), f.got...)
			f.mu.Unlock()
			return out
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %d deliveries, want %d", len(f.got), n)
	return nil
}

func newService(t *testing.T) (*Service, *catalog.Catalog, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	s := New(cat, "1.2.3", func() Server { return Server{Name: "Hearthside", PublicURL: "https://books.example.com"} },
		slog.New(slog.DiscardHandler))
	s.delays = []time.Duration{time.Millisecond, time.Millisecond}
	s.Run(ctx)
	return s, cat, ctx
}

func addTarget(t *testing.T, cat *catalog.Catalog, kind, u, secret string, events ...string) *catalog.NotifyTarget {
	t.Helper()
	tg, err := cat.CreateNotifyTarget(context.Background(), catalog.NotifyTarget{
		Kind: kind, Name: kind, URL: u, Secret: secret, Enabled: true, Events: events,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func TestWebhookDelivery(t *testing.T) {
	s, cat, ctx := newService(t)
	dest := newFakeDest(t)
	addTarget(t, cat, TargetWebhook, dest.srv.URL+"/hook?key=k", "shh", KindNewDevice)
	unsubscribed := newFakeDest(t)
	addTarget(t, cat, TargetWebhook, unsubscribed.srv.URL, "", KindBookAdded)
	off := newFakeDest(t)
	tg := addTarget(t, cat, TargetWebhook, off.srv.URL, "", KindNewDevice)
	tg.Enabled = false
	if _, err := cat.SaveNotifyTarget(ctx, *tg); err != nil {
		t.Fatal(err)
	}

	s.SignedIn(ctx, "sam", "Sam's iPhone", "AudioSilo/1.4 (ios)", false)
	got := dest.wait(t, 1)[0]
	if got.path != "/hook" || got.headers.Get("X-AudioSilo-Event") != KindNewDevice ||
		got.headers.Get("User-Agent") != "AudioSilo/1.2.3" {
		t.Fatalf("request = %s %v", got.path, got.headers)
	}
	ts := got.headers.Get("X-AudioSilo-Timestamp")
	if want := "sha256=" + Sign("shh", ts, got.raw); got.headers.Get("X-AudioSilo-Signature") != want {
		t.Fatal("signature doesn't verify")
	}
	if strings.Contains(string(got.raw), "shh") || strings.Contains(string(got.raw), "key=k") {
		t.Fatal("the body carries a secret")
	}
	if got.body["event"] != KindNewDevice || got.body["title"] != "New sign-in: sam" ||
		got.body["link"] != "https://books.example.com/admin/people/devices" {
		t.Fatalf("body = %v", got.body)
	}
	if data := got.body["data"].(map[string]any); data["device"] != "Sam's iPhone" {
		t.Fatalf("data = %v", data)
	}
	time.Sleep(50 * time.Millisecond)
	if len(unsubscribed.got) != 0 || len(off.got) != 0 {
		t.Fatal("sent to an unsubscribed or disabled destination")
	}
	// The outcome is recorded on the destination.
	waitRecorded(t, cat, 1)
	// The event is in the feed either way.
	events, _, err := cat.ListServerEvents(ctx, 0, 10, "")
	if err != nil || len(events) != 1 || events[0].Kind != KindNewDevice {
		t.Fatalf("feed = %+v, %v", events, err)
	}
}

func waitRecorded(t *testing.T, cat *catalog.Catalog, id int64) *catalog.NotifyTarget {
	t.Helper()
	for range 200 {
		tg, err := cat.GetNotifyTarget(context.Background(), id)
		if err == nil && tg.LastAt != nil {
			return tg
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("delivery never recorded")
	return nil
}

func TestNtfyAndDiscordBodies(t *testing.T) {
	s, cat, ctx := newService(t)
	dest := newFakeDest(t)
	addTarget(t, cat, TargetNtfy, dest.srv.URL+"/push/hearthside", "tk_1", KindBookAdded)
	// Discord's hosts can't be faked, so its body is built directly.
	s.ScanFinished(ctx, library.RunReport{
		Library: catalog.Library{ID: 1, Name: "Fiction"}, Status: catalog.RunOK,
		Counts: catalog.ScanCounts{Added: 7}, AddedTitles: []string{"Dune", "@everyone Emma"},
	})
	got := dest.wait(t, 1)[0]
	if got.path != "/push" || got.headers.Get("Authorization") != "Bearer tk_1" {
		t.Fatalf("ntfy request = %s %v", got.path, got.headers)
	}
	if got.body["topic"] != "hearthside" || got.body["title"] != "7 new books in Fiction" ||
		got.body["message"] != "Dune\n@everyone Emma\nand 5 more" || got.body["click"] != "https://books.example.com/admin/library" {
		t.Fatalf("ntfy body = %v", got.body)
	}

	events, _, _ := cat.ListServerEvents(ctx, 0, 1, "")
	m := compose(events[0], s.server())
	b, _ := json.Marshal(discordBody(m, s.server()))
	var d map[string]any
	_ = json.Unmarshal(b, &d)
	if am := d["allowed_mentions"].(map[string]any); len(am["parse"].([]any)) != 0 {
		t.Fatal("Discord mentions not switched off")
	}
	embed := d["embeds"].([]any)[0].(map[string]any)
	if embed["title"] != "7 new books in Fiction" || embed["url"] != "https://books.example.com/admin/library" {
		t.Fatalf("embed = %v", embed)
	}
}

func TestNoRedirectsAndFailureReasons(t *testing.T) {
	s, cat, ctx := newService(t)
	elsewhere := newFakeDest(t)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	tg := addTarget(t, cat, TargetWebhook, redirect.URL, "", KindScanFailed)
	failing := newFakeDest(t)
	failing.code = http.StatusInternalServerError
	tf := addTarget(t, cat, TargetWebhook, failing.srv.URL+"/secret-path", "", KindScanFailed)

	s.ScanFinished(ctx, library.RunReport{Library: catalog.Library{ID: 1, Name: "Fiction"}, Status: catalog.RunFailed, Detail: "boom"})
	got := waitRecorded(t, cat, tg.ID)
	if *got.LastOK || got.LastError != "http_307" {
		t.Fatalf("redirect recorded as %+v", got)
	}
	if len(elsewhere.got) != 0 {
		t.Fatal("followed a redirect")
	}
	// A 5xx is retried (three attempts), then recorded with the status only.
	failing.wait(t, 3)
	got = waitRecorded(t, cat, tf.ID)
	if *got.LastOK || got.LastError != "http_500" || strings.Contains(got.LastError, "secret") {
		t.Fatalf("failure recorded as %+v", got)
	}

	// Nothing listening: "unreachable", never the address.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL + "/token-xyz"
	dead.Close()
	td := addTarget(t, cat, TargetWebhook, deadURL, "", KindScanFailed)
	if reason := s.Test(ctx, *td); reason != "unreachable" {
		t.Fatalf("reason = %q", reason)
	}
	// And a test that arrives records success.
	ok := newFakeDest(t)
	to := addTarget(t, cat, TargetWebhook, ok.srv.URL, "", KindScanFailed)
	if reason := s.Test(ctx, *to); reason != "" {
		t.Fatalf("test failed: %q", reason)
	}
	if got := ok.wait(t, 1)[0]; got.body["event"] != "test" || got.body["title"] != "Test from Hearthside" {
		t.Fatalf("test body = %v", got.body)
	}
}

func TestDedupAndOfflineOnce(t *testing.T) {
	s, cat, ctx := newService(t)
	s.UpdateFound(ctx, "v2.0.0", "Two", "https://github.com/KodeStar/audiosilo-server/releases/v2.0.0")
	s.UpdateFound(ctx, "v2.0.0", "Two", "https://github.com/KodeStar/audiosilo-server/releases/v2.0.0")
	s.UpdateFound(ctx, "v2.0.1", "Two.1", "https://github.com/KodeStar/audiosilo-server/releases/v2.0.1")
	if events, _, _ := cat.ListServerEvents(ctx, 0, 10, ""); len(events) != 2 {
		t.Fatalf("update events = %d, want one per version", len(events))
	}

	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Lectures", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	finish := func(status string) {
		id, err := cat.StartScanRun(ctx, lib.ID, "schedule", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.FinishScanRun(ctx, id, status, catalog.ScanCounts{}, nil); err != nil {
			t.Fatal(err)
		}
		s.ScanFinished(ctx, library.RunReport{Library: *lib, RunID: id, Status: status})
	}
	count := func() int {
		events, _, _ := cat.ListServerEvents(ctx, 0, 50, "")
		n := 0
		for _, e := range events {
			if e.Kind == KindLibraryUnavailable {
				n++
			}
		}
		return n
	}
	finish(catalog.RunOK)
	finish(catalog.RunUnavailable)
	finish(catalog.RunUnavailable)
	finish(catalog.RunCancelled)   // doesn't count as being back
	finish(catalog.RunInterrupted) // nor does a scan a restart cut short
	finish(catalog.RunUnavailable)
	if n := count(); n != 1 {
		t.Fatalf("offline announced %d times while it stayed offline", n)
	}
	finish(catalog.RunOK)
	finish(catalog.RunUnavailable)
	if n := count(); n != 2 {
		t.Fatalf("offline again after coming back announced %d times in all, want 2", n)
	}
}

func TestNilServiceIsSafe(t *testing.T) {
	var s *Service
	s.Run(context.Background())
	s.Emit(context.Background(), Event{Kind: KindBookAdded})
	s.SignedIn(context.Background(), "a", "b", "c", true)
	s.UpdateFound(context.Background(), "v1", "", "")
	s.BackupFailed(context.Background(), "scheduled", "failed")
	s.ScanFinished(context.Background(), library.RunReport{Status: catalog.RunFailed})
	s.BridgeJobEnded(context.Background(), BridgeJob{State: "done"})
}

// A Bridge job's end names the book; a park carries its code (a failure never
// does: its progress may still name the park it was in), and the error (the
// Bridge's own text) stays in the bell.
func TestBridgeJobEnded(t *testing.T) {
	t.Parallel()
	s, cat, ctx := newService(t)
	dest := newFakeDest(t)
	addTarget(t, cat, TargetWebhook, dest.srv.URL, "", KindBridgeJobFinished, KindBridgeJobFailed)
	s.BridgeJobEnded(ctx, BridgeJob{ID: 7, Kind: "sidecars.generate", State: "done", Library: "Fiction", LibraryID: 1,
		Path: "Brandon Sanderson/The Way of Kings", Title: "The Way of Kings"})
	s.BridgeJobEnded(ctx, BridgeJob{ID: 8, Kind: "sidecars.generate", State: "needs_attention", Library: "Fiction", LibraryID: 1,
		Path: "Author/Untitled", Code: "book_not_writable", Error: "mkdir /mnt/user/books/Author/Untitled/.audiosilo: permission denied"})
	s.BridgeJobEnded(ctx, BridgeJob{ID: 9, Kind: "bridge.verify", State: "failed", Library: "Fiction", LibraryID: 1})
	got := dest.wait(t, 3)
	titles := map[string]string{}
	for _, g := range got {
		titles[g.body["title"].(string)] = g.body["event"].(string)
		if strings.Contains(string(g.raw), "/mnt/user") {
			t.Fatalf("the Bridge's error left the server: %s", g.raw)
		}
	}
	for title, kind := range map[string]string{
		"Characters and recaps ready: The Way of Kings": KindBridgeJobFinished,
		"A Bridge job needs attention: Author/Untitled": KindBridgeJobFailed,
		"Bridge check failed: Fiction":                  KindBridgeJobFailed,
	} {
		if titles[title] != kind {
			t.Errorf("no %s titled %q in %v", kind, title, titles)
		}
	}
	events, _, _ := cat.ListServerEvents(ctx, 0, 10, KindBridgeJobFailed)
	var parked catalog.ServerEvent
	for _, ev := range events {
		if ev.Data["state"] == "needs_attention" {
			parked = ev
		}
	}
	if parked.Data["code"] != "book_not_writable" || !strings.Contains(parked.Data["detail"].(string), "permission denied") ||
		parked.Data["path"] != "Author/Untitled" {
		t.Fatalf("parked event = %+v", parked.Data)
	}
	if m := compose(parked, s.server()); m.Text != "The Bridge can't write into the book's folder." ||
		m.Link != "https://books.example.com/admin/server?topic=bridge" {
		t.Fatalf("parked message = %+v", m)
	}

	// A failure whose progress still names an earlier park: no code, so no
	// reason it doesn't have.
	s.BridgeJobEnded(ctx, BridgeJob{ID: 10, Kind: "sidecars.generate", State: "failed", Library: "Fiction", LibraryID: 1,
		Path: "Author/Stale", Code: "budget_exceeded", Error: "gave up"})
	dest.wait(t, 4)
	events, _, _ = cat.ListServerEvents(ctx, 0, 10, KindBridgeJobFailed)
	for _, ev := range events {
		if ev.Data["path"] != "Author/Stale" {
			continue
		}
		if _, has := ev.Data["code"]; has {
			t.Fatalf("failed event carries a park code: %+v", ev.Data)
		}
		if m := compose(ev, s.server()); m.Text != "See the job in Settings > Bridge." {
			t.Fatalf("failed message = %+v", m)
		}
		return
	}
	t.Fatal("no event for the failed job")
}

// Each announced park reads as a sentence of its own: a refused credential
// says where to replace it and a missing GitHub sign-in where to sign in,
// neither asking for a Retry (the Bridge resumes both); a spent budget fits the
// book's and the month's alike (the monthly one resumes by itself); an unknown
// code points at the page.
func TestBridgeParkReasons(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]string{
		"agent_credential_refused": "The agent's credential was refused. Replace it on the Bridge's Accounts tab and the job carries on. If the agent container has a credential of its own, replace it there; the job tries again by itself a little later.",
		"contrib_unavailable":      "The Bridge can't send to the community: it has no GitHub sign-in. Sign in to GitHub on the Bridge's Accounts tab and the job carries on.",
		"budget_exceeded":          "The job reached a spending limit set on the Bridge. Raise it there, or wait for the monthly limit to reset.",
		"something_new":            "See the job in Settings > Bridge.",
	} {
		if got := bridgeParkReason(code); got != want {
			t.Errorf("bridgeParkReason(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestSignedInDeviceIsOneLine(t *testing.T) {
	s, cat, ctx := newService(t)
	s.SignedIn(ctx, "sam", "Phone\n@everyone\x00"+strings.Repeat("x", 200), "", true)
	events, _, _ := cat.ListServerEvents(ctx, 0, 10, "")
	if len(events) != 2 || events[0].Kind != KindInviteRedeemed || events[1].Kind != KindNewDevice {
		t.Fatalf("events = %+v", events)
	}
	d := events[1].Data["device"].(string)
	if strings.ContainsAny(d, "\n\x00") || len([]rune(d)) > 100 {
		t.Fatalf("device = %q", d)
	}
}

// A destination that keeps failing waits for its retry off the workers, so the
// others' deliveries go out meanwhile.
func TestRetriesDontHoldTheWorkers(t *testing.T) {
	s, cat, ctx := newService(t)
	s.delays = []time.Duration{time.Hour} // the retry won't come during the test
	for range workers + 1 {
		failing := newFakeDest(t)
		failing.code = http.StatusServiceUnavailable
		addTarget(t, cat, TargetWebhook, failing.srv.URL, "", KindScanFailed)
	}
	good := newFakeDest(t)
	addTarget(t, cat, TargetWebhook, good.srv.URL, "", KindScanFailed)
	s.ScanFinished(ctx, library.RunReport{Library: catalog.Library{ID: 1, Name: "Fiction"}, Status: catalog.RunFailed})
	good.wait(t, 1)
}

// A failed scan's detail (the OS's own words, maybe a folder on the server) shows
// in the bell but is never sent to a destination.
func TestScanFailureDetailStaysHome(t *testing.T) {
	s, cat, ctx := newService(t)
	dest := newFakeDest(t)
	addTarget(t, cat, TargetWebhook, dest.srv.URL, "", KindScanFailed)
	s.ScanFinished(ctx, library.RunReport{Library: catalog.Library{ID: 1, Name: "Fiction"}, Status: catalog.RunFailed,
		Detail: "open /mnt/private/fiction: permission denied"})
	got := dest.wait(t, 1)[0]
	if strings.Contains(string(got.raw), "/mnt/private") {
		t.Fatalf("the detail left the server: %s", got.raw)
	}
	events, _, _ := cat.ListServerEvents(ctx, 0, 1, "")
	if events[0].Data["detail"] != "open /mnt/private/fiction: permission denied" {
		t.Fatalf("the feed lost the detail: %v", events[0].Data)
	}
}

// A retry goes where the destination is set to send now: to its new address after
// a change, and nowhere once it is switched off.
func TestRetryFollowsTheDestination(t *testing.T) {
	s, cat, ctx := newService(t)
	s.delays = []time.Duration{300 * time.Millisecond}
	old := newFakeDest(t)
	old.code = http.StatusServiceUnavailable
	moved := addTarget(t, cat, TargetWebhook, old.srv.URL, "", KindScanFailed)
	off := newFakeDest(t)
	off.code = http.StatusServiceUnavailable
	disabled := addTarget(t, cat, TargetWebhook, off.srv.URL, "", KindScanFailed)

	s.ScanFinished(ctx, library.RunReport{Library: catalog.Library{ID: 1, Name: "Fiction"}, Status: catalog.RunFailed})
	old.wait(t, 1)
	off.wait(t, 1)
	next := newFakeDest(t)
	moved.URL = next.srv.URL
	if _, err := cat.SaveNotifyTarget(ctx, *moved); err != nil {
		t.Fatal(err)
	}
	disabled.Enabled = false
	if _, err := cat.SaveNotifyTarget(ctx, *disabled); err != nil {
		t.Fatal(err)
	}
	next.wait(t, 1)
	time.Sleep(100 * time.Millisecond)
	old.mu.Lock()
	off.mu.Lock()
	defer old.mu.Unlock()
	defer off.mu.Unlock()
	if len(old.got) != 1 || len(off.got) != 1 {
		t.Fatalf("retried to the old address %d times, to a switched-off one %d times", len(old.got)-1, len(off.got)-1)
	}
}

func TestSameOrigin(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"https://a.example/x", "https://A.example/y", true},
		{"https://a.example/x", "https://a.example:443/x", true},
		{"http://a.example/x", "http://a.example:80/x", true},
		{"https://a.example/x", "http://a.example/x", false},
		{"https://a.example/x", "https://a.example:8443/x", false},
		{"https://a.example/x", "https://b.example/x", false},
	} {
		if got := SameOrigin(c.a, c.b); got != c.same {
			t.Errorf("SameOrigin(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// A description-only job (sidecars.describe) is worded as the description,
// finished or failed; a park still reads its code's sentence.
func TestBridgeDescribeJobWording(t *testing.T) {
	t.Parallel()
	s, _, _ := newService(t)
	event := func(kind, state string) catalog.ServerEvent {
		return catalog.ServerEvent{Kind: kind, Data: map[string]any{"job_kind": "sidecars.describe", "state": state,
			"title": "The Way of Kings", "code": "budget_exceeded"}}
	}
	if m := compose(event(KindBridgeJobFinished, "done"), s.server()); m.Title != "Description ready: The Way of Kings" ||
		m.Text != "Listeners see it in the player's About when the book has no other description." {
		t.Errorf("finished = %+v", m)
	}
	if m := compose(event(KindBridgeJobFailed, "failed"), s.server()); m.Title != "Description failed: The Way of Kings" {
		t.Errorf("failed = %+v", m)
	}
	if m := compose(event(KindBridgeJobFailed, "needs_attention"), s.server()); m.Title != "A Bridge job needs attention: The Way of Kings" ||
		!strings.Contains(m.Text, "spending limit") {
		t.Errorf("parked = %+v", m)
	}
}

// The community jobs (sidecars.contribute, meta.correct) are worded as what
// went to the community, finished or failed, and a contribute job that filed
// nothing never as shared.
func TestBridgeCommunityJobWording(t *testing.T) {
	t.Parallel()
	s, _, _ := newService(t)
	for kind, want := range map[string]struct{ done, text, failed string }{
		"sidecars.contribute": {"Shared with the community: Mistborn", "went to the community for review", "Sharing with the community failed: Mistborn"},
		"meta.correct":        {"Corrections sent: Mistborn", "corrections went to the community for review", "Corrections failed: Mistborn"},
	} {
		event := func(ev, state string) catalog.ServerEvent {
			return catalog.ServerEvent{Kind: ev, Data: map[string]any{"job_kind": kind, "state": state, "title": "Mistborn"}}
		}
		if m := compose(event(KindBridgeJobFinished, "done"), s.server()); m.Title != want.done || !strings.Contains(m.Text, want.text) {
			t.Errorf("%s finished = %+v", kind, m)
		}
		if m := compose(event(KindBridgeJobFailed, "failed"), s.server()); m.Title != want.failed {
			t.Errorf("%s failed = %+v", kind, m)
		}
	}
	// A contribute job that filed nothing (the community had it all) says so.
	ev := catalog.ServerEvent{Kind: KindBridgeJobFinished, Data: map[string]any{"job_kind": "sidecars.contribute",
		"state": "done", "title": "Mistborn", "shared_nothing": true}}
	if m := compose(ev, s.server()); m.Title != "Already in the community: Mistborn" ||
		!strings.Contains(m.Text, "nothing was sent") || strings.Contains(m.Text, "for review") {
		t.Errorf("shared nothing = %+v", m)
	}
}

// A chapter detection job (chapters.detect) is worded as the chapters: found,
// or failed; a park because no chapters were heard says what to do instead. A
// finished one promises listeners the chapters only when they are the book's
// now (chapters_source generated); else, or when that isn't known, it says
// they were saved but the book uses other chapters.
func TestBridgeDetectJobWording(t *testing.T) {
	t.Parallel()
	s, _, _ := newService(t)
	event := func(kind, state string) catalog.ServerEvent {
		return catalog.ServerEvent{Kind: kind, Data: map[string]any{"job_kind": "chapters.detect", "state": state,
			"title": "The Black Gate", "code": "chapters_not_detected", "chapters_source": "generated"}}
	}
	if m := compose(event(KindBridgeJobFinished, "done"), s.server()); m.Title != "Chapters found: The Black Gate" ||
		m.Text != "Listeners see them in the player now." {
		t.Errorf("finished = %+v", m)
	}
	for _, src := range []string{"files", "community", ""} {
		ev := event(KindBridgeJobFinished, "done")
		ev.Data["chapters_source"] = src
		if m := compose(ev, s.server()); m.Title != "Chapters found: The Black Gate" ||
			strings.Contains(m.Text, "Listeners see") || !strings.Contains(m.Text, "uses other chapters") {
			t.Errorf("finished on %q chapters = %+v", src, m)
		}
	}
	if m := compose(event(KindBridgeJobFailed, "failed"), s.server()); m.Title != "Chapters failed: The Black Gate" {
		t.Errorf("failed = %+v", m)
	}
	if m := compose(event(KindBridgeJobFailed, "needs_attention"), s.server()); m.Title != "A Bridge job needs attention: The Black Gate" ||
		!strings.Contains(m.Text, "couldn't find the chapters in the narration") {
		t.Errorf("parked = %+v", m)
	}
}
