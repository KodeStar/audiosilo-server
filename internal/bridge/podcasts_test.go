package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func podcastQueue(t *testing.T) *testQueue {
	t.Helper()
	q := newTestQueue(t)
	ctx := context.Background()
	if _, err := q.db.ExecContext(ctx, `UPDATE libraries SET kind='podcasts' WHERE id=?`, q.lib); err != nil {
		t.Fatal(err)
	}
	st := Status{Version: "0.9.0", Kinds: []string{KindPodcastSubscribe, KindPodcastPoll}, Libraries: []LibraryStatus{{LibraryID: q.lib, MountPath: "/podcasts", Mapped: true, Writable: true}}}
	if err := q.SaveHeartbeat(ctx, q.token, st); err != nil {
		t.Fatal(err)
	}
	return q
}
func TestPrivateSubscribeLifecycle(t *testing.T) {
	q := podcastQueue(t)
	ctx := context.Background()
	secret := "https://example.com/private?auth=SECRET123"
	j, created, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, secret, true, "")
	if err != nil || !created {
		t.Fatalf("enqueue %+v %t %v", j, created, err)
	}
	var raw, key string
	if err := q.db.QueryRowContext(ctx, `SELECT params,dedupe_key FROM bridge_jobs WHERE id=?`, j.ID).Scan(&raw, &key); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "SECRET123") || !strings.HasSuffix(key, "|"+fmt.Sprint(q.admin)) {
		t.Fatalf("stored %s %s", raw, key)
	}
	again, created, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, secret, true, "")
	if err != nil || created || again.ID != j.ID {
		t.Fatalf("dedupe %+v %t %v", again, created, err)
	}
	claimed, err := q.ClaimFor(ctx, q.token, nil, 0)
	if err != nil || claimed == nil || !strings.Contains(string(claimed.Params), "SECRET123") {
		t.Fatalf("claim %+v %v", claimed, err)
	}
	ended := 0
	q.OnEnded = func(context.Context, Job) { ended++ }
	state := StateFailed
	msg := "failed to get " + secret
	out, err := q.UpdateHeld(ctx, j.ID, Update{Attempt: claimed.Attempts, State: &state, Error: &msg, Result: json.RawMessage(`{"code":"http_401","message":"failed ` + secret + `"}`), Progress: json.RawMessage(`{"message":"` + secret + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "SECRET123") || ended != 1 || out.Notable(StateRunning, "") {
		t.Fatalf("ended %s count=%d", b, ended)
	}
	if len(q.privateURLs) != 0 {
		t.Fatal("secret not released")
	}
	j, _, err = q.EnqueueSubscribe(ctx, q.lib, q.admin, secret, true, "")
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(q.db, func() time.Time { return *q.now })
	if err := restarted.ExpirePrivateRequests(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Job(ctx, j.ID)
	if err != nil || got.State != StateFailed || !strings.Contains(string(got.Result), "request_expired") {
		t.Fatalf("restart %+v %v", got, err)
	}
}
func TestSubscribeBoundAndKindGuards(t *testing.T) {
	q := podcastQueue(t)
	ctx := context.Background()
	for i := range 5 {
		if _, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, fmt.Sprintf("https://example.com/%d", i), false, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/extra", false, ""); !errors.Is(err, ErrTooManyRequests) {
		t.Fatalf("bound %v", err)
	}
	if _, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/0", false, ""); err != nil {
		t.Fatalf("idempotent at cap %v", err)
	}
	if _, _, err := q.Enqueue(ctx, NewJob{Kind: KindDescribe, LibraryID: &q.lib, Path: ptr("Show"), RequestedBy: q.admin}); !errors.Is(err, ErrNotBookLibrary) {
		t.Fatalf("book guard %v", err)
	}
	for _, bad := range []string{".hidden", "Show/episode.mp3", "../Show", ""} {
		if _, _, err := q.Enqueue(ctx, NewJob{Kind: KindPodcastPoll, LibraryID: &q.lib, Path: &bad, Params: json.RawMessage(`{}`)}); err == nil {
			t.Fatalf("show path accepted %q", bad)
		}
	}
	if _, err := q.db.ExecContext(ctx, `UPDATE libraries SET kind='books' WHERE id=?`, q.lib); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/new", false, ""); !errors.Is(err, ErrNotPodcastLibrary) {
		t.Fatalf("podcast guard %v", err)
	}
}

// A podcast job a claim handed out carries on with the Podcasts module off:
// only a queued one is held.
func TestClaimedPodcastJobCarriesOnWithTheModuleOff(t *testing.T) {
	q := podcastQueue(t)
	ctx := context.Background()
	j, _, err := q.Enqueue(ctx, NewJob{Kind: KindPodcastPoll, LibraryID: &q.lib, Path: ptr("Show"), Params: json.RawMessage(`{}`), RequestedBy: q.admin})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := q.ClaimFor(ctx, q.token, nil, 0); err != nil || claimed == nil || claimed.ID != j.ID {
		t.Fatalf("claim %v %v", claimed, err)
	}
	q.ModuleOn = func(string) bool { return false }
	got, err := q.UpdateHeld(ctx, j.ID, Update{Attempt: 1, State: ptr(StateRunning)})
	if err != nil || got.CancelRequested || got.Error != "" || got.State != StateRunning {
		t.Fatalf("module off: %+v, %v; want working on", got, err)
	}
	if got, err = q.UpdateHeld(ctx, j.ID, Update{Attempt: 1, State: ptr(StateDone)}); err != nil || got.State != StateDone {
		t.Fatalf("its end = %+v, %v; want done", got, err)
	}
}

func TestPodcastEndedAndPollPruning(t *testing.T) {
	q := podcastQueue(t)
	ctx := context.Background()
	n := 0
	q.OnEnded = func(context.Context, Job) { n++ }
	for i := range 5 {
		j, _, err := q.Enqueue(ctx, NewJob{Kind: KindPodcastPoll, LibraryID: &q.lib, Path: ptr("Show"), Params: json.RawMessage(`{}`), RequestedBy: q.admin})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := q.Cancel(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
			continue
		}
		claimed, err := q.ClaimFor(ctx, q.token, nil, 0)
		if err != nil || claimed == nil {
			t.Fatalf("claim %v %v", claimed, err)
		}
		if i == 1 {
			q.advance(Lease + time.Second)
			if _, err := q.Cancel(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := q.UpdateHeld(ctx, j.ID, Update{Attempt: claimed.Attempts, State: ptr(StateDone)}); err != nil {
			t.Fatal(err)
		}
	}
	if n != 5 {
		t.Fatalf("ended callbacks %d", n)
	}
	var count int
	if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bridge_jobs WHERE kind='podcast.poll'`).Scan(&count); err != nil || count > 2 {
		t.Fatalf("poll history %d %v", count, err)
	}
	jobs, _, err := q.List(ctx, Filter{})
	if err != nil || len(jobs) != 0 {
		t.Fatalf("default listing %+v %v", jobs, err)
	}
	jobs, _, err = q.List(ctx, Filter{Kind: KindPodcastPoll})
	if err != nil || len(jobs) == 0 {
		t.Fatalf("poll listing %+v %v", jobs, err)
	}
}

func TestPrivateLeaseAfterRestartExpiresBeforeClaim(t *testing.T) {
	q := podcastQueue(t)
	ctx := context.Background()
	j, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/rss?secret=SECRET123", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if claim, err := q.ClaimFor(ctx, q.token, nil, 0); err != nil || claim == nil {
		t.Fatalf("initial claim %+v %v", claim, err)
	}
	restarted := New(q.db, func() time.Time { return *q.now })
	q.advance(Lease + time.Second)
	claim, err := restarted.ClaimFor(ctx, q.token, nil, 0)
	if err != nil || claim != nil {
		t.Fatalf("restarted claim %+v %v", claim, err)
	}
	got, err := restarted.Job(ctx, j.ID)
	if err != nil || got.State != StateFailed || !strings.Contains(string(got.Result), "request_expired") {
		t.Fatalf("expired %+v %v", got, err)
	}
}

// A URL in a podcast job's text is redacted whole: a quote or angle bracket in
// it doesn't end the match early and leave part of a secret behind.
func TestRedactPodcastTextWholeURL(t *testing.T) {
	for _, s := range []string{
		`failed https://host/feed?token=abc"SECRET`,
		`failed https://user:pa"SECRET@host/feed`,
		`failed https://host/members/abc<SECRET>/feed.xml`,
	} {
		if got := redactPodcastText(s); strings.Contains(got, "SECRET") {
			t.Errorf("%s -> %s", s, got)
		}
	}
}

// An open request is adopted only with the same privacy: a public request
// never rides a private job (nor the reverse), and the private URL in memory is
// never attached to a public job. Same-privacy adoption still works at the
// open cap.
func TestSubscribePrivacyConflict(t *testing.T) {
	q := podcastQueue(t)
	ctx := context.Background()
	public := "https://example.com/feed.xml"
	pub, created, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, public, false, "")
	if err != nil || !created {
		t.Fatal(err)
	}
	if _, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, public, true, ""); !errors.Is(err, ErrPrivacyConflict) {
		t.Fatalf("private onto public = %v", err)
	}
	q.secretMu.Lock()
	if q.privateURLs[pub.ID] != "" {
		t.Fatal("a private URL was attached to a public job")
	}
	q.secretMu.Unlock()
	// A member feed that looks public, followed as private.
	secret := "https://example.com/members.xml"
	priv, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, secret, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, secret, false, ""); !errors.Is(err, ErrPrivacyConflict) {
		t.Fatalf("public onto private = %v", err)
	}
	// Fill the open cap, then the same requests still adopt.
	for i := range 3 {
		if _, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, fmt.Sprintf("https://example.com/%d.xml", i), false, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/sixth.xml", false, ""); !errors.Is(err, ErrTooManyRequests) {
		t.Fatalf("cap = %v", err)
	}
	if j, created, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, public, false, ""); err != nil || created || j.ID != pub.ID {
		t.Fatalf("public adoption at cap = %v", err)
	}
	if j, created, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, secret, true, ""); err != nil || created || j.ID != priv.ID {
		t.Fatalf("private adoption at cap = %v", err)
	}
}

// An adopt-only subscribe returns the open request or ErrNoNewRequest, and
// queues nothing either way; with nothing open it never waits for the
// subscribe lock (server#219).
func TestSubscribeAdoptOnly(t *testing.T) {
	q := podcastQueue(t)
	ctx := context.Background()
	q.secretFlowMu.Lock()
	refused := make(chan error, 1)
	go func() {
		_, _, err := q.Subscribe(ctx, SubscribeRequest{LibraryID: q.lib, User: q.admin, FeedURL: "https://example.com/a.xml", AdoptOnly: true})
		refused <- err
	}()
	select {
	case err := <-refused:
		if !errors.Is(err, ErrNoNewRequest) {
			t.Fatalf("nothing open = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an adopt-only subscribe with nothing open waited for the subscribe lock")
	}
	q.secretFlowMu.Unlock()
	var n int
	if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bridge_jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("queued %d %v", n, err)
	}
	j, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/a.xml", false, "")
	if err != nil {
		t.Fatal(err)
	}
	got, created, err := q.Subscribe(ctx, SubscribeRequest{LibraryID: q.lib, User: q.admin, FeedURL: "https://example.com/a.xml", AdoptOnly: true})
	if err != nil || created || got.ID != j.ID {
		t.Fatalf("adopt = %v", err)
	}
}

// RecentSubscribeFailure is the person's LAST request for the feed in the
// library, only while it failed within the window and only when the failure
// was the feed's (never an expired private request, a lost lease or an
// unwritable library).
func TestRecentSubscribeFailure(t *testing.T) {
	t.Parallel()
	q := podcastQueue(t)
	ctx := context.Background()
	feedURL := "https://example.com/dead.xml"
	none := func(what, feedURL string, lib int64) {
		t.Helper()
		if got, err := q.RecentSubscribeFailure(ctx, lib, q.admin, feedURL, time.Hour); err != nil || got != nil {
			t.Fatalf("%s = %+v %v", what, got, err)
		}
	}
	none("no request", feedURL, q.lib)
	fail := func(id int64, code string) {
		t.Helper()
		claimed, err := q.ClaimFor(ctx, q.token, nil, 0)
		if err != nil || claimed == nil || claimed.ID != id {
			t.Fatalf("claim %+v %v", claimed, err)
		}
		state, msg := StateFailed, "failed"
		if _, err := q.UpdateHeld(ctx, id, Update{Attempt: claimed.Attempts, State: &state, Error: &msg, Result: json.RawMessage(`{"code":"` + code + `","message":"failed"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	j, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, feedURL, false, "")
	if err != nil {
		t.Fatal(err)
	}
	none("open request", feedURL, q.lib)
	fail(j.ID, "not_a_feed")
	got, err := q.RecentSubscribeFailure(ctx, q.lib, q.admin, feedURL, time.Hour)
	if err != nil || got == nil || got.ID != j.ID {
		t.Fatalf("failed request = %+v %v", got, err)
	}
	none("another feed's failure", "https://example.com/other.xml", q.lib)
	none("another library's failure", feedURL, q.lib+1)
	q.advance(time.Hour + time.Second)
	none("a failure past the window", feedURL, q.lib)
	// A retry that is open again hides the older failure.
	again, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, feedURL, false, "")
	if err != nil || again.ID == j.ID {
		t.Fatalf("retry %+v %v", again, err)
	}
	fail(again.ID, "not_a_feed")
	if got, err := q.RecentSubscribeFailure(ctx, q.lib, q.admin, feedURL, time.Hour); err != nil || got == nil || got.ID != again.ID {
		t.Fatalf("latest failure = %+v %v", got, err)
	}
	// A failure on the Bridge's or the library's side is not the feed's.
	unwritable := "https://example.com/unwritable.xml"
	u, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, unwritable, false, "")
	if err != nil {
		t.Fatal(err)
	}
	fail(u.ID, "library_not_writable")
	none("an unwritable library", unwritable, q.lib)
	lost := "https://example.com/lost.xml"
	l, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, lost, false, "")
	if err != nil {
		t.Fatal(err)
	}
	for range MaxAttempts {
		if claimed, err := q.ClaimFor(ctx, q.token, nil, 0); err != nil || claimed == nil || claimed.ID != l.ID {
			t.Fatalf("claim %+v %v", claimed, err)
		}
		q.advance(Lease + time.Second)
	}
	none("a Bridge that stopped reporting", lost, q.lib)
	if got, err := q.Job(ctx, l.ID); err != nil || got.State != StateFailed {
		t.Fatalf("lost job %+v %v", got, err)
	}
	// An expired private request is not the feed's failure.
	secret := "https://example.com/private?auth=SECRET123"
	p, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, secret, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := New(q.db, func() time.Time { return *q.now }).ExpirePrivateRequests(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := q.Job(ctx, p.ID); err != nil || got.State != StateFailed {
		t.Fatalf("expired %+v %v", got, err)
	}
	none("expired private request", secret, q.lib)
}

// R-13: deleting a library cascades its jobs away; PruneSecrets then drops the
// private URLs held for them and keeps every other library's.
func TestPruneSecretsAfterLibraryDelete(t *testing.T) {
	t.Parallel()
	q := podcastQueue(t)
	ctx := context.Background()
	res, err := q.db.ExecContext(ctx, `INSERT INTO libraries(name, root, kind, created_at) VALUES('Other', '/other', 'podcasts', 't')`)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := res.LastInsertId()
	st := Status{Version: "0.9.0", Kinds: []string{KindPodcastSubscribe}, Libraries: []LibraryStatus{{LibraryID: q.lib, MountPath: "/podcasts", Mapped: true, Writable: true}, {LibraryID: other, MountPath: "/other", Mapped: true, Writable: true}}}
	if err := q.SaveHeartbeat(ctx, q.token, st); err != nil {
		t.Fatal(err)
	}
	gone, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/a?auth=SECRET_GONE", true, "")
	if err != nil {
		t.Fatal(err)
	}
	kept, _, err := q.EnqueueSubscribe(ctx, other, q.admin, "https://example.com/b?auth=SECRET_KEPT", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := q.PruneSecrets(ctx); err != nil || len(q.privateURLs) != 2 {
		t.Fatalf("nothing deleted yet: %d %v", len(q.privateURLs), err)
	}
	if _, err := q.db.ExecContext(ctx, `DELETE FROM libraries WHERE id=?`, q.lib); err != nil {
		t.Fatal(err)
	}
	// The delete has committed: a request that ended meanwhile (its client
	// went, its deadline passed) still prunes.
	ended, cancel := context.WithCancel(ctx)
	cancel()
	if err := q.PruneSecrets(ended); err != nil {
		t.Fatal(err)
	}
	if _, ok := q.privateURLs[gone.ID]; ok {
		t.Fatal("a deleted library's private URL is still held")
	}
	if q.privateURLs[kept.ID] == "" {
		t.Fatal("another library's private URL was dropped")
	}
	// A URL held for a request that already ended (Subscribe adopting it as it
	// ended) is dropped too.
	if _, err := q.Cancel(ctx, kept.ID); err != nil {
		t.Fatal(err)
	}
	q.privateURLs[kept.ID] = "https://example.com/b?auth=SECRET_KEPT"
	if err := q.PruneSecrets(ctx); err != nil || len(q.privateURLs) != 0 {
		t.Fatalf("an ended request's URL: %d %v", len(q.privateURLs), err)
	}

	// A request that ends between its commit and the URL being stored (its
	// reportEnded found nothing to drop) is let go by Subscribe itself, with no
	// PruneSecrets (server#200).
	q.beforeSecretHeld = func(j *Job) {
		if _, err := q.Cancel(ctx, j.ID); err != nil {
			t.Error(err)
		}
	}
	raced, _, err := q.EnqueueSubscribe(ctx, other, q.admin, "https://example.com/c?auth=SECRET_RACED", true, "")
	q.beforeSecretHeld = nil
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := q.privateURLs[raced.ID]; ok {
		t.Fatal("the URL of a request that ended while it was being queued is still held")
	}
	// Allowed: one that stays open keeps its URL for the claim.
	open, _, err := q.EnqueueSubscribe(ctx, other, q.admin, "https://example.com/d?auth=SECRET_OPEN", true, "")
	if err != nil || q.privateURLs[open.ID] == "" {
		t.Fatalf("an open request's URL: %q %v", q.privateURLs[open.ID], err)
	}
}

// FeedFailure counts the feed's own failures only: an offline Bridge, a library
// it can't write or a lost private URL say nothing about the feed.
func TestFeedFailure(t *testing.T) {
	for _, code := range []string{"timeout", "dns", "http_500", "gone", "not_a_feed", "tls"} {
		if !FeedFailure(code) {
			t.Errorf("%s is not counted as the feed's failure", code)
		}
	}
	for _, code := range []string{"", "bridge_lost", "request_expired", "library_not_writable", "write_failed", "feed_secret_missing", "identity_changed", "not_podcast_library"} {
		if FeedFailure(code) {
			t.Errorf("%q is counted as the feed's failure", code)
		}
	}
}

// NetworkFailure is the Bridge not reaching the feed at all, each one also a
// feed failure; the feed's answer and its address are not network-class.
func TestNetworkFailure(t *testing.T) {
	for _, code := range []string{"dns", "network", "timeout", "tls"} {
		if !NetworkFailure(code) || !FeedFailure(code) {
			t.Errorf("%s is not a network-class feed failure", code)
		}
	}
	for _, code := range []string{"", "http_500", "http_503", "gone", "not_a_feed", "blocked_address", "bridge_lost", "feed_secret_missing"} {
		if NetworkFailure(code) {
			t.Errorf("%q is counted as network-class", code)
		}
	}
}

// An adopt-only subscribe that could never be queued says why (server#219):
// its params and library are checked before it looks for an open request, so
// an OPML row with a bad URL is invalid, never limited.
func TestSubscribeAdoptOnlyChecksParamsFirst(t *testing.T) {
	q := podcastQueue(t)
	ctx := context.Background()
	long := "https://" + strings.Repeat("a", 250) + ".example/feed.xml" // a host past 253 characters
	if _, _, err := q.Subscribe(ctx, SubscribeRequest{LibraryID: q.lib, User: q.admin, FeedURL: long, AdoptOnly: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over-long host = %v, want ErrInvalid", err)
	}
	if _, _, err := q.Subscribe(ctx, SubscribeRequest{LibraryID: q.lib, User: q.admin, FeedURL: "https://example.com/a.xml", ShowPath: "../Show", AdoptOnly: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad show path = %v, want ErrInvalid", err)
	}
	// A valid row with nothing open is still only refused as limited.
	if _, _, err := q.Subscribe(ctx, SubscribeRequest{LibraryID: q.lib, User: q.admin, FeedURL: "https://example.com/a.xml", AdoptOnly: true}); !errors.Is(err, ErrNoNewRequest) {
		t.Fatalf("valid, nothing open = %v, want ErrNoNewRequest", err)
	}
	if _, err := q.db.ExecContext(ctx, `UPDATE libraries SET kind='books' WHERE id=?`, q.lib); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Subscribe(ctx, SubscribeRequest{LibraryID: q.lib, User: q.admin, FeedURL: "https://example.com/a.xml", AdoptOnly: true}); !errors.Is(err, ErrNotPodcastLibrary) {
		t.Fatalf("book library = %v, want ErrNotPodcastLibrary", err)
	}
}

// A follow request never waits on a Bridge that left (server#179): once no
// online Bridge runs podcast.subscribe, the queued requests fail as
// request_expired when the player reads them (or the scheduler sweeps), so an
// OPML import's next wave answers unavailable. A held one is left to its
// lease; an online Bridge keeps every request, and a silence first seen less
// than OnlineWindow ago (a server just started, a clock that jumped) fails none.
func TestOfflineBridgeExpiresQueuedSubscribes(t *testing.T) {
	q := podcastQueue(t)
	ctx := context.Background()
	held, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/held.xml", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if c, err := q.ClaimFor(ctx, q.token, nil, 0); err != nil || c == nil || c.ID != held.ID {
		t.Fatalf("claim %+v %v", c, err)
	}
	waiting, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/waiting.xml", false, "")
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/members.xml?auth=SECRET", true, "")
	if err != nil {
		t.Fatal(err)
	}
	stateOf := func(id int64) string {
		t.Helper()
		j, err := q.Job(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return j.State
	}
	// Denied: the Bridge is online.
	if _, err := q.SubscribeRequests(ctx, q.admin); err != nil || stateOf(waiting.ID) != StateQueued {
		t.Fatalf("online Bridge: %s %v", stateOf(waiting.ID), err)
	}
	// The held request's Bridge reports within its lease (a requeued one is
	// queued again, and fails with the rest).
	extendHeld := func() {
		t.Helper()
		if _, err := q.db.ExecContext(ctx, `UPDATE bridge_jobs SET lease_until=? WHERE id=?`, at(q.now.Add(Lease)), held.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Denied: a clock that jumped (or a host that slept) past the Bridge's
	// heartbeat fails nothing before its next beat.
	q.advance(time.Hour)
	extendHeld()
	if _, err := q.SubscribeRequests(ctx, q.admin); err != nil || stateOf(waiting.ID) != StateQueued {
		t.Fatalf("clock jumped: %s %v", stateOf(waiting.ID), err)
	}
	q.beatLibrary(t, true, true, KindPodcastSubscribe)
	if _, err := q.SubscribeRequests(ctx, q.admin); err != nil || stateOf(waiting.ID) != StateQueued {
		t.Fatalf("beat after the jump: %s %v", stateOf(waiting.ID), err)
	}
	// Denied: a server that just started gives the Bridge OnlineWindow to call in.
	q.advance(OnlineWindow + time.Second)
	extendHeld()
	restarted := New(q.db, func() time.Time { return *q.now })
	if _, err := restarted.SubscribeRequests(ctx, q.admin); err != nil || stateOf(waiting.ID) != StateQueued {
		t.Fatalf("just started: %s %v", stateOf(waiting.ID), err)
	}
	// Denied: the first sweep to see the heartbeat lapsed only notes it.
	if _, err := q.SubscribeRequests(ctx, q.admin); err != nil || stateOf(waiting.ID) != StateQueued {
		t.Fatalf("first seen offline: %s %v", stateOf(waiting.ID), err)
	}
	// Allowed: offline at every sighting for OnlineWindow.
	q.advance(OnlineWindow)
	extendHeld()
	q.OnEnded = func(_ context.Context, j Job) {
		if j.State != StateFailed {
			t.Errorf("ended %d as %s", j.ID, j.State)
		}
	}
	reqs, err := q.SubscribeRequests(ctx, q.admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{waiting.ID, secret.ID} {
		j, err := q.Job(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		o, _ := PodcastOutcomeOf(*j)
		if j.State != StateFailed || o.Code != "request_expired" || o.Message != offlineRequestMessage || FeedFailure(o.Code) {
			t.Fatalf("expired %d: %s %+v", id, j.State, o)
		}
	}
	if stateOf(held.ID) == StateFailed {
		t.Fatal("a held request was failed: its lease decides")
	}
	if q.privateURLs[secret.ID] != "" {
		t.Fatal("an expired private request kept its URL")
	}
	for _, r := range reqs {
		if r.ID == waiting.ID && r.State != StateFailed {
			t.Fatalf("the read answered the request as %s, not its end", r.State)
		}
	}
	// A Bridge that no longer runs the kind is no Bridge for it either.
	q.beatLibrary(t, true, true, KindPodcastSubscribe)
	again, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, "https://example.com/again.xml", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.EndedPodcasts(ctx); err != nil || stateOf(again.ID) != StateQueued {
		t.Fatalf("back online: %s %v", stateOf(again.ID), err)
	}
	q.beatLibrary(t, true, true, KindPodcastPoll)
	if _, err := q.EndedPodcasts(ctx); err != nil || stateOf(again.ID) != StateQueued {
		t.Fatalf("first seen without the kind: %s %v", stateOf(again.ID), err)
	}
	q.advance(OnlineWindow)
	q.beatLibrary(t, true, true, KindPodcastPoll)
	if _, err := q.EndedPodcasts(ctx); err != nil || stateOf(again.ID) != StateFailed {
		t.Fatalf("no subscribe kind: %s %v", stateOf(again.ID), err)
	}
}

// foldContext's floor of its own per fold stops maxFoldOverrun past the
// caller's deadline, so one settle that ends many jobs cannot hold its request
// open for minFoldBudget per job; with no caller deadline a fold has none.
func TestFoldContextBounds(t *testing.T) {
	deadlineOf := func(ctx context.Context) time.Duration {
		t.Helper()
		d, ok := ctx.Deadline()
		if !ok {
			t.Fatal("no deadline")
		}
		return time.Until(d)
	}
	near := time.Second / 2
	for _, tc := range []struct {
		name   string
		caller time.Duration // the caller's deadline, from now
		want   time.Duration // the fold's, from now
	}{
		{"caller has longer", time.Minute, time.Minute},
		{"caller spent: the floor", -time.Second, minFoldBudget},
		{"floor cut at the overrun", -(maxFoldOverrun - 2*time.Second), 2 * time.Second},
	} {
		caller, cancel := context.WithDeadline(context.Background(), time.Now().Add(tc.caller))
		fold, done := foldContext(caller)
		if got := deadlineOf(fold); got > tc.want+near || got < tc.want-near {
			t.Errorf("%s: fold has %v, want %v", tc.name, got, tc.want)
		}
		done()
		cancel()
	}
	caller, cancel := context.WithDeadline(context.Background(), time.Now().Add(-maxFoldOverrun-time.Second))
	defer cancel()
	fold, done := foldContext(caller)
	defer done()
	if fold.Err() == nil {
		t.Fatal("a fold past the overrun still had time")
	}
	fold, done = foldContext(context.Background())
	defer done()
	if _, ok := fold.Deadline(); ok {
		t.Fatal("a caller with no deadline gave its fold one")
	}
}

// Subscribe adopting an open private request while the Bridge ends it (the
// adopt race, server#179 and server#200): no private URL outlives its request,
// whichever commits first.
func TestSubscribeAdoptRacesTheEnd(t *testing.T) {
	q := podcastQueue(t)
	ctx := context.Background()
	secret := "https://example.com/rss?auth=SECRET"
	for range 20 {
		j, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, secret, true, "")
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := q.ClaimFor(ctx, q.token, nil, 0)
		if err != nil || claimed == nil || claimed.ID != j.ID {
			t.Fatalf("claim %+v %v", claimed, err)
		}
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				if _, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, secret, true, ""); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Go(func() {
			if _, err := q.UpdateHeld(ctx, j.ID, Update{Attempt: claimed.Attempts, State: ptr(StateDone)}); err != nil {
				t.Error(err)
			}
		})
		wg.Wait()
		open, err := queryIDs(ctx, q.db, `SELECT id FROM bridge_jobs WHERE kind=? AND state IN `+OpenStatesSQL, KindPodcastSubscribe)
		if err != nil {
			t.Fatal(err)
		}
		q.secretMu.Lock()
		for id := range q.privateURLs {
			if !slices.Contains(open, id) {
				t.Errorf("job %d ended with its private URL held", id)
			}
		}
		q.secretMu.Unlock()
		// The next round starts with nothing open.
		for _, id := range open {
			if _, err := q.Cancel(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Each OnEnded fold has a budget of its own (server#219): one fold that spends
// the caller's deadline leaves the next one minFoldBudget, never a dead
// context.
func TestFoldsGetTheirOwnBudget(t *testing.T) {
	q := podcastQueue(t)
	ctx := context.Background()
	var jobs []*Job
	for i := range 2 {
		j, _, err := q.EnqueueSubscribe(ctx, q.lib, q.admin, fmt.Sprintf("https://example.com/%d.xml", i), false, "")
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	for range jobs {
		if c, err := q.ClaimFor(ctx, q.token, nil, 0); err != nil || c == nil {
			t.Fatalf("claim %+v %v", c, err)
		}
	}
	// Both leases run out on their last attempt: one settle reports both.
	if _, err := q.db.ExecContext(ctx, `UPDATE bridge_jobs SET attempts=?`, MaxAttempts); err != nil {
		t.Fatal(err)
	}
	q.advance(Lease + time.Second)
	short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var folds []error
	q.OnEnded = func(ctx context.Context, _ Job) {
		if len(folds) == 0 {
			<-short.Done() // the first fold outlasts the caller's deadline
		}
		folds = append(folds, ctx.Err())
	}
	// The read after the settle meets the spent deadline; the folds are what
	// count here.
	if _, err := q.Job(short, jobs[0].ID); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if len(folds) != 2 || folds[1] != nil {
		t.Fatalf("folds %v: the second fold had no time left", folds)
	}
}
