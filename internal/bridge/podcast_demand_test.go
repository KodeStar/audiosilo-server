package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func demandJob(q *testQueue, priority int) NewJob {
	path := "Show/episode.mp3"
	return NewJob{LibraryID: &q.lib, Path: &path, Params: json.RawMessage(`{"show_id":"0123456789abcdef","episode_id":"fedcba9876543210"}`), Priority: priority, RequestedBy: q.admin}
}
func TestEnsurePodcastFetchConcurrentPromotion(t *testing.T) {
	q := fetchQueue(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	ids := make(chan int64, 20)
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			priority := []int{-10, 10, 20, 100}[i%4]
			j, _, err := q.EnsurePodcastFetch(ctx, demandJob(q, priority), FetchOptions{Retry: i%3 == 0})
			if err != nil {
				errs <- err
				return
			}
			ids <- j.ID
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Fatal(err)
	}
	var id int64
	for next := range ids {
		if id != 0 && next != id {
			t.Fatal("duplicate intents")
		}
		id = next
	}
	j, err := q.Job(ctx, id)
	if err != nil || j.Priority != 100 || j.Attempts != 0 {
		t.Fatalf("%+v %v", j, err)
	}
	held, err := q.ClaimFor(ctx, q.token, nil, 0)
	if err != nil || held == nil {
		t.Fatal(err)
	}
	state := StateRunning
	if _, err = q.UpdateHeld(ctx, id, Update{Attempt: held.Attempts, State: &state, Progress: json.RawMessage(`{"bytes_received":100,"bytes_total":1000}`)}); err != nil {
		t.Fatal(err)
	}
	wake := q.wake.wait()
	j, created, err := q.EnsurePodcastFetch(ctx, demandJob(q, 20), FetchOptions{})
	if err != nil || created || j.Priority != 100 || j.Attempts != 1 || j.State != StateRunning || string(j.Progress) == "{}" {
		t.Fatalf("promotion reset %+v %v", j, err)
	}
	select {
	case <-wake:
		t.Fatal("an unchanged priority woke the claims")
	default:
	}
}
func TestEnsurePodcastFetchPromotionWakes(t *testing.T) {
	q := fetchQueue(t)
	ctx := context.Background()
	first, _, err := q.EnsurePodcastFetch(ctx, demandJob(q, 10), FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wake := q.wake.wait()
	j, created, err := q.EnsurePodcastFetch(ctx, demandJob(q, 100), FetchOptions{})
	if err != nil || created || j.ID != first.ID || j.Priority != 100 {
		t.Fatalf("promotion %+v %v", j, err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("no post-commit wake")
	}
}
func TestEnsurePodcastFetchUnavailableAndCooldown(t *testing.T) {
	for _, reason := range []string{"offline", "kind", "mapping", "module", "demo"} {
		t.Run(reason, func(t *testing.T) {
			q := fetchQueue(t)
			ctx := context.Background()
			first, _, err := q.EnsurePodcastFetch(ctx, demandJob(q, 10), FetchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			opt := FetchOptions{}
			switch reason {
			case "offline":
				q.advance(OnlineWindow + time.Second)
			case "kind":
				q.beat(t, KindVerify)
			case "mapping":
				if err = q.SaveHeartbeat(ctx, q.token, Status{Kinds: []string{KindPodcastFetch}}); err != nil {
					t.Fatal(err)
				}
			case "module":
				q.ModuleOn = func(string) bool { return false }
			case "demo":
				opt.NoNew = true
			}
			j, created, err := q.EnsurePodcastFetch(ctx, demandJob(q, 100), opt)
			if err != nil || created || j.ID != first.ID || j.Priority != 100 {
				t.Fatalf("existing %+v %v", j, err)
			}
			if _, err = q.db.ExecContext(ctx, `DELETE FROM bridge_jobs`); err != nil {
				t.Fatal(err)
			}
			if _, _, err = q.EnsurePodcastFetch(ctx, demandJob(q, 100), opt); err == nil {
				t.Fatal("unavailable created work")
			}
		})
	}
	q := fetchQueue(t)
	ctx := context.Background()
	j, _, err := q.EnsurePodcastFetch(ctx, demandJob(q, 100), FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	held, err := q.ClaimFor(ctx, q.token, nil, 0)
	if err != nil || held == nil {
		t.Fatal(err)
	}
	failed := StateFailed
	if _, err = q.UpdateHeld(ctx, j.ID, Update{Attempt: held.Attempts, State: &failed, Result: json.RawMessage(`{"code":"too_large","message":"https://secret.example/CANARY"}`)}); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if _, _, err = q.EnsurePodcastFetch(ctx, demandJob(q, 100), FetchOptions{}); !errors.Is(err, ErrFetchCooldown) {
			t.Fatal(err)
		}
	}
	fresh, created, err := q.EnsurePodcastFetch(ctx, demandJob(q, 100), FetchOptions{Retry: true})
	if err != nil || !created || fresh.ID == j.ID {
		t.Fatalf("retry %+v %v", fresh, err)
	}
	for range 20 {
		same, created, err := q.EnsurePodcastFetch(ctx, demandJob(q, 100), FetchOptions{Retry: true})
		if err != nil || created || same.ID != fresh.ID {
			t.Fatal("retry storm", err)
		}
	}
}
func wire2b(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/wire-2b.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}
func assertJSON(t *testing.T, got any, want json.RawMessage) {
	t.Helper()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var g, w any
	if json.Unmarshal(raw, &g) != nil || json.Unmarshal(want, &w) != nil || !reflect.DeepEqual(g, w) {
		t.Fatalf("got %s want %s", raw, want)
	}
}
func TestPodcastFetchStatusGoldenAndSanitization(t *testing.T) {
	q := fetchQueue(t)
	ctx := context.Background()
	fixture := wire2b(t)
	get := func() FetchStatus {
		states, err := q.PodcastFetchStatuses(ctx, q.lib, []string{"Show/episode.mp3"})
		if err != nil {
			t.Fatal(err)
		}
		return states["Show/episode.mp3"]
	}
	if st := get(); st.State != "absent" || st.RequestID != nil || st.BytesReceived != 0 || st.BytesTotal != nil || st.Error != nil || st.RetryAfterSeconds != 0 {
		t.Fatalf("absent %+v", st)
	}
	j, _, err := q.EnsurePodcastFetch(ctx, demandJob(q, 100), FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st := get(); st.State != "queued" || st.RequestID == nil || *st.RequestID != j.ID || st.BytesTotal != nil || st.BytesReceived != 0 {
		t.Fatalf("queued %+v", st)
	}
	if _, err = q.db.ExecContext(ctx, `UPDATE bridge_jobs SET id=81,state='running',progress='{"bytes_received":100,"bytes_total":1000,"message":"CANARY"}' WHERE id=?`, j.ID); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, map[string]any{"fetch": get()}, fixture["status_response"])
	if _, err = q.db.ExecContext(ctx, `UPDATE bridge_jobs SET id=82,state='failed',result='{"code":"too_large","message":"CANARY"}' WHERE id=81`); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, get(), fixture["failed"])
	if _, err = q.db.ExecContext(ctx, `UPDATE bridge_jobs SET state='running',progress='{"bytes_received":-100,"bytes_total":-1}'`); err != nil {
		t.Fatal(err)
	}
	if st := get(); st.BytesReceived != 0 || st.BytesTotal != nil {
		t.Fatalf("negative counters %+v", st)
	}
	if _, err = q.db.ExecContext(ctx, `UPDATE bridge_jobs SET progress='{"bytes_received":9999,"bytes_total":100}'`); err != nil {
		t.Fatal(err)
	}
	if st := get(); st.BytesReceived != 100 || st.BytesTotal == nil || *st.BytesTotal != 100 {
		t.Fatalf("counters %+v", st)
	}
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/feed-fetch-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var feed struct {
		Episodes []struct {
			Fetch json.RawMessage `json:"fetch"`
		} `json:"episodes"`
	}
	if err = json.Unmarshal(raw, &feed); err != nil {
		t.Fatal(err)
	}
	if _, err = q.db.ExecContext(ctx, `UPDATE podcast_episodes SET present=1,fetch_json=?`, string(feed.Episodes[0].Fetch)); err != nil {
		t.Fatal(err)
	}
	q.ModuleOn = func(string) bool { return false }
	assertJSON(t, get(), fixture["ready"])
	if _, err = q.db.ExecContext(ctx, `UPDATE podcast_episodes SET present=0`); err != nil {
		t.Fatal(err)
	}
	if st := get(); st.State != "unavailable" || st.RequestID == nil || st.Error.Code != "module_off" {
		t.Fatalf("held %+v", st)
	}
	if _, err = q.db.ExecContext(ctx, `DELETE FROM bridge_jobs; INSERT INTO podcast_fetch_state(library_id,rel_path,show_id,failures,error_code,error) VALUES(?,'Show/episode.mp3','0123456789abcdef',1,'too_large','CANARY')`, q.lib); err != nil {
		t.Fatal(err)
	}
	q.ModuleOn = nil
	if st := get(); st.State != "failed" || st.RequestID != nil || st.Error.Message != "The episode exceeds the size limit." {
		t.Fatalf("pruned %+v", st)
	}
}
func TestClaimMinimumPriorityBeforeLease(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()
	low := q.verify(t, "", 20)
	high := q.verify(t, "A", 100)
	floor := int32(100)
	claimed, err := q.ClaimForPriority(ctx, q.token, []string{KindVerify}, 0, &floor)
	if err != nil || claimed == nil || claimed.ID != high.ID {
		t.Fatalf("claim %+v %v", claimed, err)
	}
	if j, err := q.Job(ctx, low.ID); err != nil || j.Attempts != 0 || j.LeaseUntil != nil || j.State != StateQueued {
		t.Fatalf("excluded mutated %+v %v", j, err)
	}
	if j, err := q.ClaimForPriority(ctx, q.token, nil, 0, &floor); err != nil || j != nil {
		t.Fatalf("floor %+v %v", j, err)
	}
	if j, err := q.ClaimFor(ctx, q.token, nil, 0); err != nil || j == nil || j.ID != low.ID {
		t.Fatalf("compatible %+v %v", j, err)
	}
}
func TestPodcastWatchBroadcastCleanup(t *testing.T) {
	q := fetchQueue(t)
	a, releaseA := q.WatchPodcast(q.lib, "Show/episode.mp3")
	b, releaseB := q.WatchPodcast(q.lib, "Show/episode.mp3")
	other, releaseOther := q.WatchPodcast(q.lib, "Show/other.mp3")
	releaseA()
	q.NotifyPodcast(q.lib, "Show/episode.mp3")
	for _, ch := range []<-chan struct{}{a, b} {
		select {
		case <-ch:
		default:
			t.Fatal("missed broadcast")
		}
	}
	select {
	case <-other:
		t.Fatal("cross-path wake")
	default:
	}
	releaseB()
	releaseOther()
	if len(q.fetchWait) != 0 {
		t.Fatal("waiter leak")
	}
}

func TestCommittedCallbacksKeepDeadline(t *testing.T) {
	q := fetchQueue(t)
	ctx := context.Background()
	j, _, err := q.EnsurePodcastFetch(ctx, demandJob(q, 100), FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	held, err := q.ClaimFor(ctx, q.token, nil, 0)
	if err != nil || held == nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	bounded, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	called := false
	q.OnEnded = func(ctx context.Context, _ Job) {
		called = true
		got, ok := ctx.Deadline()
		if !ok || !got.Equal(deadline) {
			t.Fatal("outcome fold lost stream budget")
		}
	}
	state := StateFailed
	if _, err = q.UpdateHeld(bounded, j.ID, Update{Attempt: held.Attempts, State: &state}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("no committed callback")
	}
}
func TestClaimMinimumPriorityStillChecksModuleAndToken(t *testing.T) {
	q := fetchQueue(t)
	ctx := context.Background()
	j, _, err := q.EnsurePodcastFetch(ctx, demandJob(q, 100), FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	floor := int32(100)
	q.ModuleOn = func(string) bool { return false }
	if got, err := q.ClaimForPriority(ctx, q.token, nil, 0, &floor); err != nil || got != nil {
		t.Fatalf("module held %+v %v", got, err)
	}
	if got, err := q.Job(ctx, j.ID); err != nil || got.Attempts != 0 || got.LeaseUntil != nil {
		t.Fatal(got, err)
	}
	q.ModuleOn = nil
	if _, err = q.db.ExecContext(ctx, `UPDATE tokens SET revoked=1 WHERE id=?`, q.token); err != nil {
		t.Fatal(err)
	}
	if got, err := q.ClaimForPriority(ctx, q.token, nil, 0, &floor); !errors.Is(err, ErrNotPaired) || got != nil {
		t.Fatalf("revoked %+v %v", got, err)
	}
	if got, err := q.Job(ctx, j.ID); err != nil || got.Attempts != 0 || got.LeaseUntil != nil {
		t.Fatal(got, err)
	}
}

func TestPodcastFetchErrorsKeepSafeCodes(t *testing.T) {
	for _, code := range []string{"dns", "network", "timeout", "tls", "invalid_feed_url", "feed_secret_missing", "write_failed", "identity_changed", "http_429", "http_503"} {
		if err := FetchErrorFor(code); err.Code != code || err.Message == "" {
			t.Fatalf("lost safe error %+v", err)
		}
	}
	for _, code := range []string{"http_+20", "http_99", "http_600", "http_5x0", "https://CANARY.invalid/secret", "CANARY"} {
		if err := FetchErrorFor(code); err.Code != "fetch_failed" {
			t.Fatalf("unsafe code %+v", err)
		}
	}
}

// An index that calls the pin present while its file does not open shows the
// open work (or, with none, absent plus the refusal), never "ready".
func TestUnreadyFetchStatusIgnoresIndexedPin(t *testing.T) {
	q := fetchQueue(t)
	ctx := context.Background()
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/feed-fetch-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var feed struct {
		Episodes []struct {
			Fetch json.RawMessage `json:"fetch"`
		} `json:"episodes"`
	}
	if err = json.Unmarshal(raw, &feed); err != nil {
		t.Fatal(err)
	}
	if _, err = q.db.ExecContext(ctx, `UPDATE podcast_episodes SET present=1,fetch_json=?`, string(feed.Episodes[0].Fetch)); err != nil {
		t.Fatal(err)
	}
	if st, err := q.UnreadyFetchStatus(ctx, q.lib, "Show/episode.mp3"); err != nil || st.State != "absent" || st.RequestID != nil {
		t.Fatalf("absent %+v %v", st, err)
	}
	j, _, err := q.EnsurePodcastFetch(ctx, demandJob(q, 100), FetchOptions{Retry: true})
	if err != nil {
		t.Fatal(err)
	}
	if states, err := q.PodcastFetchStatuses(ctx, q.lib, []string{"Show/episode.mp3"}); err != nil || states["Show/episode.mp3"].State != "ready" {
		t.Fatalf("indexed %+v %v", states, err)
	}
	if st, err := q.UnreadyFetchStatus(ctx, q.lib, "Show/episode.mp3"); err != nil || st.State != "queued" || st.RequestID == nil || *st.RequestID != j.ID {
		t.Fatalf("queued %+v %v", st, err)
	}
	if _, err = q.db.ExecContext(ctx, `DELETE FROM bridge_jobs`); err != nil {
		t.Fatal(err)
	}
	q.ModuleOn = func(string) bool { return false }
	if st, err := q.UnreadyFetchStatus(ctx, q.lib, "Show/episode.mp3"); err != nil || st.State != "unavailable" || st.Error == nil || st.Error.Code != "module_off" {
		t.Fatalf("refusal %+v %v", st, err)
	}
	if st, err := q.UnreadyFetchStatus(ctx, q.lib, "Show/other.mp3"); err != nil || st.State != "absent" {
		t.Fatalf("not indexed %+v %v", st, err)
	}
}
