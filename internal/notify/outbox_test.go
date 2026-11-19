package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
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
	s = New(cat, "1.2.3", func() Server { return Server{Name: "Hearthside"} }, slog.New(slog.DiscardHandler))
	if len(delays) > 0 {
		s.delays = delays
	}
	s.Run(ctx)
	return s, cancel
}

// stoppedService records events but never sends: the server stopped between
// the record and the delivery.
func stoppedService(t *testing.T, cat *catalog.Catalog) *Service {
	t.Helper()
	return New(cat, "1.2.3", func() Server { return Server{Name: "Hearthside"} }, slog.New(slog.DiscardHandler))
}

func (f *fakeDest) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

// settle waits for the deliveries in flight, then says how many dest has had.
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

	stoppedService(t, cat).ScanFinished(ctx, library.RunReport{Library: catalog.Library{ID: 1, Name: "Fiction"}, Status: catalog.RunFailed})
	if hook.count() != 0 {
		t.Fatal("a stopped server sent it")
	}

	_, stop := serviceRun(t, cat)
	if got := hook.wait(t, 1)[0]; got.body["event"] != KindScanFailed {
		t.Fatalf("redelivered %v", got.body)
	}
	push.wait(t, 1)
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
	cat.SetPodcastNotice(stoppedService(t, cat).PodcastListener())
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
	failing.wait(t, 1)
	time.Sleep(50 * time.Millisecond) // the attempt is counted before it is sent; let the worker finish
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
	stoppedService(t, cat).ScanFinished(ctx, library.RunReport{Library: catalog.Library{ID: 1, Name: "Fiction"}, Status: catalog.RunFailed})
	tg.Enabled = false
	if _, err := cat.SaveNotifyTarget(ctx, *tg); err != nil {
		t.Fatal(err)
	}
	serviceRun(t, cat)
	if n := settle(dest); n != 0 {
		t.Fatalf("sent %d to a switched-off destination", n)
	}
	if owed, _ := cat.PendingDeliveries(ctx, 0); len(owed) != 0 {
		t.Fatalf("still owed: %+v", owed)
	}
}
