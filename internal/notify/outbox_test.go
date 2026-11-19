package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/podcast/feed"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// serviceRun is one run of the server's notifications over cat: stop ends it
// (a stopped server), and the service's queue is left behind with it.
func serviceRun(t *testing.T, cat *catalog.Catalog, delays ...time.Duration) (s *Service, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s = newTestService(cat)
	if len(delays) > 0 {
		s.delays = delays
	}
	s.Run(ctx)
	return s, cancel
}

// stoppedService records events but never sends: the server stopped between
// the record and the delivery.
func stoppedService(cat *catalog.Catalog) *Service { return newTestService(cat) }

// waitOwedNone waits until the outbox owes nothing: every delivery queued has
// ended (delivered, failed or skipped), not just reached its destination.
func waitOwedNone(t *testing.T, cat *catalog.Catalog) {
	t.Helper()
	for range 500 {
		if owed, err := cat.PendingDeliveries(context.Background(), 0); err == nil && len(owed) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the outbox still owes deliveries")
}

// settle gives a delivery that should not happen time to arrive, then says
// how many dest has had.
func settle(f *fakeDest) int {
	time.Sleep(150 * time.Millisecond)
	return f.count()
}

// server#465: a notice recorded but not yet sent when the server stopped goes
// out at the next start, once to each destination, and the bell shows it once;
// a start after that sends nothing again.
func TestOwedNoticeIsSentAtTheNextStart(t *testing.T) {
	ctx := context.Background()
	cat := catalog.New(storetest.Open(t), time.Now)
	hook, push, other := newFakeDest(t), newFakeDest(t), newFakeDest(t)
	addTarget(t, cat, TargetWebhook, hook.srv.URL, "", KindScanFailed)
	addTarget(t, cat, TargetNtfy, push.srv.URL+"/t", "", KindScanFailed)
	addTarget(t, cat, TargetWebhook, other.srv.URL, "", KindBookAdded)

	stoppedService(cat).ScanFinished(ctx, library.RunReport{Library: catalog.Library{ID: 1, Name: "Fiction"}, Status: catalog.RunFailed})
	if hook.count() != 0 {
		t.Fatal("a stopped server sent it")
	}

	_, stop := serviceRun(t, cat)
	if got := hook.wait(t, 1)[0]; got.body["event"] != KindScanFailed {
		t.Fatalf("redelivered %v", got.body)
	}
	push.wait(t, 1)
	waitOwedNone(t, cat) // ended, not cut short by the stop
	stop()
	serviceRun(t, cat) // and another start
	if n, m, o := settle(hook), push.count(), other.count(); n != 1 || m != 1 || o != 0 {
		t.Fatalf("webhook %d, ntfy %d, unsubscribed %d deliveries; want 1, 1, 0", n, m, o)
	}
	if events, _, _ := cat.ListServerEvents(ctx, 0, 10, ""); len(events) != 1 {
		t.Fatalf("the bell shows %d events, want 1", len(events))
	}
}

// server#465 and #403: a feed's failing notice is recorded in the transaction
// that marks it announced, so a stop before it is sent still sends it at the
// next start, once.
func TestOwedPodcastNoticeIsSentAtTheNextStart(t *testing.T) {
	ctx := context.Background()
	cat := catalog.New(storetest.Open(t), time.Now)
	cat.SetPodcastNotice(stoppedService(cat).PodcastListener())
	hook := newFakeDest(t)
	addTarget(t, cat, TargetWebhook, hook.srv.URL, "", KindPodcastFeedFailing)
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Pods", Root: t.TempDir(), Kind: catalog.KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	show := "Example Show"
	f, err := feed.ReadFile("../podcast/testdata/feed-v1.json", show, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.IndexPodcastShow(ctx, lib.ID, show, "s1", "", "", f, nil, false); err != nil {
		t.Fatal(err)
	}
	for id := range int64(catalog.FeedFailingAfter) {
		raw, _ := json.Marshal(map[string]string{"code": "http_404"})
		j := bridge.Job{ID: id + 1, Kind: bridge.KindPodcastPoll, LibraryID: &lib.ID, Path: &show, State: bridge.StateFailed,
			Params: json.RawMessage(`{"show_id":"a1b2c3d4e5f60718","scheduled":true}`), Result: raw}
		if err := cat.FoldPodcastJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if events, _, _ := cat.ListServerEvents(ctx, 0, 10, KindPodcastFeedFailing); len(events) != 1 {
		t.Fatalf("failing events = %+v, want one", events)
	}

	s, stop := serviceRun(t, cat)
	cat.SetPodcastNotice(s.PodcastListener())
	if got := hook.wait(t, 1)[0]; got.body["event"] != KindPodcastFeedFailing {
		t.Fatalf("redelivered %v", got.body)
	}
	waitOwedNone(t, cat)
	stop()
	serviceRun(t, cat)
	if n := settle(hook); n != 1 {
		t.Fatalf("sent %d times, want once", n)
	}
}

// A destination that keeps failing is tried a bounded number of times in all,
// restarts included: attempts made before a stop count.
func TestFailingDestinationRetriesOnABound(t *testing.T) {
	ctx := context.Background()
	cat := catalog.New(storetest.Open(t), time.Now)
	failing := newFakeDest(t)
	failing.code = http.StatusServiceUnavailable
	tg := addTarget(t, cat, TargetWebhook, failing.srv.URL, "", KindScanFailed)

	// The first attempt fails, and its retry is due long after the stop.
	s, stop := serviceRun(t, cat, time.Hour, time.Hour)
	s.ScanFinished(ctx, library.RunReport{Library: catalog.Library{ID: 1, Name: "Fiction"}, Status: catalog.RunFailed})
	failing.wait(t, 1) // counted before it was sent
	stop()

	// The next start makes the two attempts left, and no more.
	serviceRun(t, cat, time.Millisecond, time.Millisecond)
	failing.wait(t, 3)
	if got := waitRecorded(t, cat, tg.ID); got.LastError != "http_503" {
		t.Fatalf("recorded %q", got.LastError)
	}
	serviceRun(t, cat, time.Millisecond, time.Millisecond)
	if n := settle(failing); n != 3 {
		t.Fatalf("tried %d times, want 3", n)
	}
	if owed, _ := cat.PendingDeliveries(ctx, 0); len(owed) != 0 {
		t.Fatalf("still owed: %+v", owed)
	}
}

// A destination switched off before an owed notice goes out is not sent it.
func TestOwedNoticeFollowsTheDestination(t *testing.T) {
	ctx := context.Background()
	cat := catalog.New(storetest.Open(t), time.Now)
	dest := newFakeDest(t)
	tg := addTarget(t, cat, TargetWebhook, dest.srv.URL, "", KindScanFailed)
	stoppedService(cat).ScanFinished(ctx, library.RunReport{Library: catalog.Library{ID: 1, Name: "Fiction"}, Status: catalog.RunFailed})
	tg.Enabled = false
	if _, err := cat.SaveNotifyTarget(ctx, *tg); err != nil {
		t.Fatal(err)
	}
	serviceRun(t, cat)
	waitOwedNone(t, cat)
	if n := dest.count(); n != 0 {
		t.Fatalf("sent %d to a switched-off destination", n)
	}
}

// An event raised before Run starts is both queued by its own Deliver and
// owed to the start's redelivery: it goes once, even when the redelivery reads
// the row while the first copy's send is still under way.
func TestNoticeQueuedTwiceAtStartGoesOnce(t *testing.T) {
	ctx := context.Background()
	cat := catalog.New(storetest.Open(t), time.Now)
	var (
		mu  sync.Mutex
		got int
	)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		got++
		mu.Unlock()
		time.Sleep(200 * time.Millisecond) // the redelivery reads the row meanwhile
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(slow.Close)
	addTarget(t, cat, TargetWebhook, slow.URL, "", KindScanFailed)

	s := newTestService(cat)
	s.ScanFinished(ctx, library.RunReport{Library: catalog.Library{ID: 1, Name: "Fiction"}, Status: catalog.RunFailed})
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	s.Run(runCtx)
	waitOwedNone(t, cat)
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if got != 1 {
		t.Fatalf("sent %d times, want once", got)
	}
}

// A delivery this run never queued (a full queue dropped it) goes out at the
// next sweep, not the next start.
func TestOwedNoticeGoesAtTheNextSweep(t *testing.T) {
	ctx := context.Background()
	cat := catalog.New(storetest.Open(t), time.Now)
	dest := newFakeDest(t)
	addTarget(t, cat, TargetWebhook, dest.srv.URL, "", KindScanFailed)
	s := newTestService(cat)
	s.sweep = 50 * time.Millisecond
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	s.Run(runCtx)
	// Recorded after the start's read, by a service whose queue no one reads.
	time.Sleep(20 * time.Millisecond)
	stoppedService(cat).ScanFinished(ctx, library.RunReport{Library: catalog.Library{ID: 1, Name: "Fiction"}, Status: catalog.RunFailed})
	if got := dest.wait(t, 1)[0]; got.body["event"] != KindScanFailed {
		t.Fatalf("swept %v", got.body)
	}
	waitOwedNone(t, cat)
	if n := settle(dest); n != 1 {
		t.Fatalf("sent %d times, want once", n)
	}
}
