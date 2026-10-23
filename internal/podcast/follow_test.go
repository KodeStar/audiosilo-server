package podcast

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/podcast/opml"
)

// fakeShow is a known show and the URLs whose hash proves access to it.
type fakeShow struct {
	show          catalog.PodcastShow
	urls          []string
	secretMissing bool
}

type fakeFollowCatalog struct {
	mu       sync.Mutex
	whole    bool
	shows    []*fakeShow
	follows  []string // show paths followed, in order
	grantErr error
	// onGrant runs before each grant check (a concurrent revoke or toggle).
	onGrant func(n int)
	grants  int
}

func (c *fakeFollowCatalog) PodcastWholeGrant(_ context.Context, _ catalog.Caller, _ int64) (bool, error) {
	c.mu.Lock()
	c.grants++
	n, hook := c.grants, c.onGrant
	c.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.whole, c.grantErr
}

func (c *fakeFollowCatalog) PodcastShowByHash(_ context.Context, _ int64, hash string, _ int64) (*catalog.PodcastShow, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.shows {
		for _, u := range s.urls {
			norm, err := feed.NormalizeFeedURL(u)
			if err == nil && feed.FeedURLHash(norm) == hash {
				show := s.show
				return &show, nil
			}
		}
	}
	return nil, catalog.ErrNotFound
}

func (c *fakeFollowCatalog) PodcastState(_ context.Context, _ int64, show string) (catalog.PodcastFeedState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.shows {
		if s.show.Path == show {
			return catalog.PodcastFeedState{ShowID: s.show.ShowID, SecretMissing: s.secretMissing}, nil
		}
	}
	return catalog.PodcastFeedState{}, catalog.ErrNotFound
}

func (c *fakeFollowCatalog) FollowPodcast(_ context.Context, _, _ int64, p, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.follows = append(c.follows, p)
	for _, s := range c.shows {
		if s.show.Path == p {
			s.show.Following = true
		}
	}
	return nil
}

// fakeQueue is the subscribe queue's rules: one open job per feed (adopted
// at the same privacy, privacy_conflict otherwise), five open per person,
// and AdoptOnly refusing a new one.
type fakeQueue struct {
	mu       sync.Mutex
	offline  bool
	kinds    []string
	nextID   int64
	open     map[string]*bridge.Job // by feed URL
	privacy  map[string]bool
	requests []bridge.SubscribeRequest
	wakes    int
	err      error
	failed   map[string]*bridge.Job // by feed URL: the caller's recently failed request
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{kinds: []string{bridge.KindPodcastSubscribe}, nextID: 81, open: map[string]*bridge.Job{}, privacy: map[string]bool{}}
}

func (q *fakeQueue) Online(context.Context) (*bridge.Heartbeat, error) {
	if q.offline {
		return nil, nil
	}
	return &bridge.Heartbeat{Status: bridge.Status{Kinds: q.kinds}}, nil
}

func (q *fakeQueue) Subscribe(_ context.Context, r bridge.SubscribeRequest) (*bridge.Job, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.requests = append(q.requests, r)
	if q.err != nil {
		return nil, false, q.err
	}
	if j, ok := q.open[r.FeedURL]; ok {
		if q.privacy[r.FeedURL] != r.Private {
			return nil, false, bridge.ErrPrivacyConflict
		}
		return j, false, nil
	}
	if r.AdoptOnly {
		return nil, false, bridge.ErrNoNewRequest
	}
	if len(q.open) >= 5 {
		return nil, false, bridge.ErrTooManyRequests
	}
	params, _ := json.Marshal(bridge.SubscribeParams{FeedURL: r.FeedURL, Private: r.Private, ShowPath: r.ShowPath})
	j := &bridge.Job{ID: q.nextID, Kind: bridge.KindPodcastSubscribe, Params: params, State: bridge.StateQueued}
	q.nextID++
	q.open[r.FeedURL], q.privacy[r.FeedURL] = j, r.Private
	return j, true, nil
}

func (q *fakeQueue) WakePodcasts() { q.mu.Lock(); q.wakes++; q.mu.Unlock() }

func (q *fakeQueue) RecentSubscribeFailure(_ context.Context, _, _ int64, norm string, _ time.Time) (*bridge.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.failed[norm], nil
}

// fail ends every open request as failed (the Bridge could not read the feed).
func (q *fakeQueue) fail() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failed == nil {
		q.failed = map[string]*bridge.Job{}
	}
	for u, j := range q.open {
		j.State = bridge.StateFailed
		q.failed[u] = j
	}
	q.open = map[string]*bridge.Job{}
}

// fixtureFollower is import-context.json's world: library 7 held whole, an
// online writable Bridge, nothing open, and Fixture One (followed) known by
// its current and previous URLs.
func fixtureFollower(t *testing.T) (*Follower, *fakeFollowCatalog, *fakeQueue) {
	t.Helper()
	raw, err := os.ReadFile("testdata/phase2c3/opml/import-context.json")
	if err != nil {
		t.Fatal(err)
	}
	var ctx struct {
		Shows []struct {
			Path         string   `json:"path"`
			ShowID       string   `json:"show_id"`
			Private      bool     `json:"private"`
			Following    bool     `json:"following"`
			CurrentURL   string   `json:"current_url"`
			PreviousURLs []string `json:"previous_urls"`
		} `json:"shows"`
		NextRequestID int64 `json:"next_request_id"`
	}
	if err := json.Unmarshal(raw, &ctx); err != nil {
		t.Fatal(err)
	}
	cat := &fakeFollowCatalog{whole: true}
	for _, s := range ctx.Shows {
		cat.shows = append(cat.shows, &fakeShow{
			show: catalog.PodcastShow{Path: s.Path, ShowID: s.ShowID, Private: s.Private, Following: s.Following},
			urls: append([]string{s.CurrentURL}, s.PreviousURLs...),
		})
	}
	q := newFakeQueue()
	q.nextID = ctx.NextRequestID
	return &Follower{Catalog: cat, Queue: q}, cat, q
}

func parseFixture(t *testing.T, name string) *opml.Document {
	t.Helper()
	raw, err := os.ReadFile("testdata/phase2c3/opml/" + name)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := opml.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func asJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

var caller = catalog.Caller{ID: 11, Podcasts: true}

func TestImportDuplicatesPrivateGolden(t *testing.T) {
	f, cat, q := fixtureFollower(t)
	got := f.Import(context.Background(), caller, 7, parseFixture(t, "duplicates-private.opml"))
	raw, err := os.ReadFile("testdata/phase2c3/opml/duplicates-private-expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if g := asJSON(t, got); !reflect.DeepEqual(g, want) {
		gotRaw, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("rows differ from the golden:\n%s", gotRaw)
	}
	// The private row went to the queue as private, with its secret, and the
	// response never echoes it.
	if len(q.requests) != 1 || !q.requests[0].Private || !strings.Contains(q.requests[0].FeedURL, "OPML_PRIVATE_CANARY_2C3") {
		t.Fatalf("queue requests %+v", q.requests)
	}
	if out, _ := json.Marshal(got); strings.Contains(string(out), "OPML_PRIVATE_CANARY_2C3") || strings.Contains(string(out), "hosts") {
		t.Fatal("the result echoes a raw URL")
	}
	// The two already-following rows re-followed the known show, no more.
	if len(cat.follows) != 2 || cat.follows[0] != "Fixture One" {
		t.Fatalf("follows %v", cat.follows)
	}
}

func TestImportQueueCapWave(t *testing.T) {
	f, cat, q := fixtureFollower(t)
	doc := parseFixture(t, "queue-cap.opml")
	first := f.Import(context.Background(), caller, 7, doc)
	statuses := func(r ImportResult) string {
		var s []string
		for _, row := range r.Rows {
			s = append(s, row.Status)
		}
		return strings.Join(s, ",")
	}
	if got := statuses(first); got != "queued,queued,queued,queued,queued,limited,limited" {
		t.Fatalf("first upload %s", got)
	}
	for _, row := range first.Rows[5:] {
		if row.Error == nil || row.Error.Code != "too_many_requests" || row.RequestID != nil || row.Feed == nil {
			t.Fatalf("limited row %+v", row)
		}
	}
	// Re-uploading while those five are open adopts them and still limits.
	again := f.Import(context.Background(), caller, 7, doc)
	if got := statuses(again); got != "already_queued,already_queued,already_queued,already_queued,already_queued,limited,limited" {
		t.Fatalf("retry %s", got)
	}
	if *again.Rows[0].RequestID != *first.Rows[0].RequestID {
		t.Fatal("an adopted row reports another job")
	}
	// The five complete (their shows indexed and followed); the next upload
	// admits the remaining two.
	q.mu.Lock()
	for u := range q.open {
		cat.shows = append(cat.shows, &fakeShow{show: catalog.PodcastShow{Path: u, ShowID: u, Following: true}, urls: []string{u}})
	}
	q.open = map[string]*bridge.Job{}
	q.mu.Unlock()
	third := f.Import(context.Background(), caller, 7, doc)
	if got := statuses(third); got != "already_following,already_following,already_following,already_following,already_following,queued,queued" {
		t.Fatalf("third upload %s", got)
	}
}

// The upload's own budget of five stands even when the person's open cap
// would allow more (a Bridge finishing jobs while the rows run).
func TestImportUploadBudget(t *testing.T) {
	f, _, q := fixtureFollower(t)
	q.mu.Lock()
	q.err = nil
	q.mu.Unlock()
	cat := f.Catalog.(*fakeFollowCatalog)
	cat.onGrant = func(int) {
		q.mu.Lock()
		q.open = map[string]*bridge.Job{} // every job finishes at once
		q.mu.Unlock()
	}
	res := f.Import(context.Background(), caller, 7, parseFixture(t, "queue-cap.opml"))
	queued := 0
	for _, r := range res.Rows {
		if r.Status == RowQueued {
			queued++
		}
	}
	if queued != ImportNewRequests {
		t.Fatalf("queued %d", queued)
	}
}

func TestImportRowsStopAtRevokeOrModuleOff(t *testing.T) {
	t.Run("module off", func(t *testing.T) {
		f, cat, q := fixtureFollower(t)
		on := true
		f.ModuleOn = func() bool { return on }
		cat.onGrant = func(n int) {
			if n == 2 {
				on = false
			}
		}
		res := f.Import(context.Background(), caller, 7, parseFixture(t, "queue-cap.opml"))
		if res.Rows[0].Status != RowQueued || res.Rows[1].Status != RowQueued {
			t.Fatalf("rows before the switch %+v", res.Rows[:2])
		}
		for _, r := range res.Rows[2:] {
			if r.Status != RowUnavailable || r.Error.Code != "module_off" {
				t.Fatalf("row after the switch %+v", r)
			}
		}
		if len(q.requests) != 2 {
			t.Fatalf("effects after the switch: %d requests", len(q.requests))
		}
	})
	t.Run("revoked", func(t *testing.T) {
		f, cat, q := fixtureFollower(t)
		cat.onGrant = func(n int) {
			if n == 2 {
				cat.mu.Lock()
				cat.whole = false
				cat.mu.Unlock()
			}
		}
		res := f.Import(context.Background(), caller, 7, parseFixture(t, "queue-cap.opml"))
		for _, r := range res.Rows[1:] {
			if r.Status != RowFailed || r.Error.Code != "no_library_access" {
				t.Fatalf("row after the revoke %+v", r)
			}
		}
		if len(q.requests) != 1 {
			t.Fatalf("effects after the revoke: %d requests", len(q.requests))
		}
	})
}

func TestImportRowErrors(t *testing.T) {
	doc := &opml.Document{Candidates: []opml.Candidate{{Index: 1, URL: "https://feeds.example.org/a.xml"}}}
	for _, tc := range []struct {
		name   string
		setup  func(*Follower, *fakeQueue)
		status string
		code   string
	}{
		{"no bridge", func(f *Follower, _ *fakeQueue) { f.Queue = nil }, RowUnavailable, "bridge_unavailable"},
		{"offline", func(_ *Follower, q *fakeQueue) { q.offline = true }, RowUnavailable, "bridge_unavailable"},
		{"no subscribe kind", func(_ *Follower, q *fakeQueue) { q.kinds = nil }, RowUnavailable, "bridge_unavailable"},
		{"not writable", func(_ *Follower, q *fakeQueue) { q.err = bridge.ErrLibraryNotWritable }, RowUnavailable, "library_not_writable"},
		{"open cap", func(_ *Follower, q *fakeQueue) { q.err = bridge.ErrTooManyRequests }, RowLimited, "too_many_requests"},
		{"privacy", func(_ *Follower, q *fakeQueue) { q.err = bridge.ErrPrivacyConflict }, RowFailed, "privacy_conflict"},
		{"unexpected", func(_ *Follower, q *fakeQueue) { q.err = errors.New("disk on fire") }, RowFailed, "subscribe_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, q := fixtureFollower(t)
			tc.setup(f, q)
			res := f.Import(context.Background(), caller, 7, doc)
			r := res.Rows[0]
			if r.Status != tc.status || r.Error == nil || r.Error.Code != tc.code || r.Path != nil || r.RequestID != nil {
				t.Fatalf("row %+v", r)
			}
			if strings.Contains(r.Error.Message, "fire") {
				t.Fatal("an unexpected error is echoed")
			}
		})
	}
}

func TestFollow(t *testing.T) {
	ctx := context.Background()
	t.Run("known show offline", func(t *testing.T) {
		f, cat, q := fixtureFollower(t)
		q.offline = true
		cat.shows[0].show.Following = false
		out, err := f.Follow(ctx, FollowURL{Caller: caller, LibraryID: 7, FeedURL: "https://feeds.example.org/old-one.xml"})
		if err != nil || out.State != StateFollowing || out.Show.Path != "Fixture One" || !out.Show.Following || q.wakes != 1 {
			t.Fatalf("%+v %v", out, err)
		}
	})
	t.Run("narrow grant", func(t *testing.T) {
		f, cat, q := fixtureFollower(t)
		cat.whole = false
		if _, err := f.Follow(ctx, FollowURL{Caller: caller, LibraryID: 7, FeedURL: "https://feeds.example.org/one.xml"}); !errors.Is(err, ErrNoWholeGrant) {
			t.Fatal(err)
		}
		if len(cat.follows) != 0 || len(q.requests) != 0 {
			t.Fatal("a denied follow had effects")
		}
	})
	t.Run("module off", func(t *testing.T) {
		f, cat, _ := fixtureFollower(t)
		f.ModuleOn = func() bool { return false }
		if _, err := f.Follow(ctx, FollowURL{Caller: caller, LibraryID: 7, FeedURL: "https://feeds.example.org/one.xml"}); !errors.Is(err, ErrModuleOff) {
			t.Fatal(err)
		}
		if cat.grants != 0 {
			t.Fatal("module off still checked the grant")
		}
	})
	t.Run("unstable URL", func(t *testing.T) {
		f, _, q := fixtureFollower(t)
		for _, raw := range []string{"file:///etc/hosts", "", "https://"} {
			if _, err := f.Follow(ctx, FollowURL{Caller: caller, LibraryID: 7, FeedURL: raw}); !errors.Is(err, feed.ErrInvalidFeedURL) {
				t.Fatalf("%q: %v", raw, err)
			}
		}
		if len(q.requests) != 0 {
			t.Fatal("an invalid URL was queued")
		}
	})
	t.Run("secret repair", func(t *testing.T) {
		f, cat, q := fixtureFollower(t)
		cat.shows[0].secretMissing = true
		cat.shows[0].show.Private = true
		out, err := f.Follow(ctx, FollowURL{Caller: caller, LibraryID: 7, FeedURL: "https://feeds.example.org/one.xml"})
		if err != nil || out.State != StateSubscribing || !out.Created {
			t.Fatalf("%+v %v", out, err)
		}
		// A matched private show makes the request private, whatever it said.
		if r := q.requests[0]; !r.Private || r.ShowPath != "Fixture One" {
			t.Fatalf("request %+v", r)
		}
	})
	t.Run("refused repair keeps the show's privacy", func(t *testing.T) {
		f, cat, q := fixtureFollower(t)
		q.offline = true
		cat.shows[0].secretMissing = true
		cat.shows[0].show.Private = true
		out, err := f.Follow(ctx, FollowURL{Caller: caller, LibraryID: 7, FeedURL: "https://feeds.example.org/one.xml"})
		if !errors.Is(err, ErrBridgeUnavailable) || out == nil || !out.Private || out.State != "" {
			t.Fatalf("%+v %v", out, err)
		}
		// The import's row reports the matched show as private too.
		doc := &opml.Document{Version: "2.0", Outlines: 1, Candidates: []opml.Candidate{{Index: 1, URL: "https://feeds.example.org/one.xml"}}}
		res := f.Import(ctx, caller, 7, doc)
		if len(res.Rows) != 1 || res.Rows[0].Status != RowUnavailable || !res.Rows[0].Private {
			t.Fatalf("row %+v", res.Rows)
		}
	})
	t.Run("member feed", func(t *testing.T) {
		f, _, q := fixtureFollower(t)
		if _, err := f.Follow(ctx, FollowURL{Caller: caller, LibraryID: 7, FeedURL: "https://example.com/members.xml", Private: true}); err != nil {
			t.Fatal(err)
		}
		if !q.requests[0].Private {
			t.Fatal("private:true was dropped")
		}
	})
}

func TestNormalizeStable(t *testing.T) {
	got, err := feed.NormalizeStable("HTTPS://Feeds.Example.org:443/one.xml#x")
	if err != nil || got != "https://feeds.example.org/one.xml" {
		t.Fatalf("%q %v", got, err)
	}
	if again, err := feed.NormalizeStable(got); err != nil || again != got {
		t.Fatal("normalizing twice changed the URL")
	}
	if _, err := feed.NormalizeStable("ftp://example.org/a"); !errors.Is(err, feed.ErrInvalidFeedURL) {
		t.Fatal(err)
	}
}

// Five dead feeds ahead of three good ones: once the five have failed, the
// next wave reports them as recently failed (their request ids, nothing
// queued) and follows the good ones; a single follow still retries one.
func TestImportSkipsRecentlyFailedFeeds(t *testing.T) {
	f, _, q := fixtureFollower(t)
	doc := &opml.Document{Version: "2.0"}
	for i, u := range []string{"dead-1", "dead-2", "dead-3", "dead-4", "dead-5", "good-1", "good-2", "good-3"} {
		doc.Candidates = append(doc.Candidates, opml.Candidate{Index: i + 1, URL: "https://feeds.example.org/" + u + ".xml"})
	}
	doc.Outlines = len(doc.Candidates)
	statuses := func(r ImportResult) string {
		var s []string
		for _, row := range r.Rows {
			s = append(s, row.Status)
		}
		return strings.Join(s, ",")
	}
	first := f.Import(context.Background(), caller, 7, doc)
	if got := statuses(first); got != "queued,queued,queued,queued,queued,limited,limited,limited" {
		t.Fatalf("first wave %s", got)
	}
	q.fail()
	second := f.Import(context.Background(), caller, 7, doc)
	if got := statuses(second); got != "error,error,error,error,error,queued,queued,queued" {
		t.Fatalf("second wave %s", got)
	}
	for i, row := range second.Rows[:5] {
		if row.Error == nil || row.Error.Code != "recently_failed" || row.Error.Message == "" || row.RequestID == nil || *row.RequestID != *first.Rows[i].RequestID || row.Feed == nil {
			t.Fatalf("failed row %+v", row)
		}
	}
	for _, r := range q.requests[len(first.Rows):] {
		if strings.Contains(r.FeedURL, "dead-") {
			t.Fatalf("a recently failed feed reached the queue: %+v", r)
		}
	}
	// A third wave changes nothing for the failed five and adopts the rest.
	if got := statuses(f.Import(context.Background(), caller, 7, doc)); got != "error,error,error,error,error,already_queued,already_queued,already_queued" {
		t.Fatalf("third wave %s", got)
	}
	// The single follow by address always retries.
	out, err := f.Follow(context.Background(), FollowURL{Caller: caller, LibraryID: 7, FeedURL: "https://feeds.example.org/dead-1.xml"})
	if err != nil || out.State != StateSubscribing || !out.Created {
		t.Fatalf("manual retry %+v %v", out, err)
	}
}
