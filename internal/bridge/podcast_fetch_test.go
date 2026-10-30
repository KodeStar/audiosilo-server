package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func fetchQueue(t *testing.T) *testQueue {
	t.Helper()
	q := podcastQueue(t)
	ctx := context.Background()
	if _, err := q.db.ExecContext(ctx, `INSERT INTO podcast_shows VALUES(?1,'Show','0123456789abcdef','Title','','','','','episodic',0,0,'','','','','',1,NULL,'s','t'); INSERT INTO books(library_id,rel_path,title,indexed_at) VALUES(?1,'Show/episode.mp3','Episode','t'); INSERT INTO podcast_episodes(book_id,library_id,show_path,episode_id,guid,episode_type,explicit,feed_duration,present,generation,removed_at,duration_source) SELECT id,library_id,'Show','fedcba9876543210','','full',0,0,0,0,'','none' FROM books WHERE library_id=?1 AND rel_path='Show/episode.mp3'`, q.lib); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveHeartbeat(ctx, q.token, Status{Version: "0.9.0", Kinds: []string{KindPodcastFetch, KindPodcastRetain}, Libraries: []LibraryStatus{{LibraryID: q.lib, MountPath: "/podcasts", Mapped: true, Writable: true}}}); err != nil {
		t.Fatal(err)
	}
	return q
}
func TestPodcastFetchQueueContractAndIdentity(t *testing.T) {
	q := fetchQueue(t)
	ctx := context.Background()
	p := "Show/episode.mp3"
	params := json.RawMessage(`{"show_id":"0123456789abcdef","episode_id":"fedcba9876543210"}`)
	job, created, err := q.Enqueue(ctx, NewJob{Kind: KindPodcastFetch, LibraryID: &q.lib, Path: &p, Params: params, Priority: 20, RequestedBy: q.admin})
	if err != nil || !created {
		t.Fatalf("enqueue: %+v %v", job, err)
	}
	var key string
	if err = q.db.QueryRowContext(ctx, `SELECT dedupe_key FROM bridge_jobs WHERE id=?`, job.ID).Scan(&key); err != nil || key != "podcast.fetch|"+fmt.Sprint(q.lib)+"|"+p {
		t.Fatalf("dedupe key %s %v", key, err)
	}
	again, created, err := q.Enqueue(ctx, NewJob{Kind: KindPodcastFetch, LibraryID: &q.lib, Path: &p, Params: params})
	if err != nil || created || again.ID != job.ID {
		t.Fatalf("dedupe: %+v %v", again, err)
	}
	if job.Notable(StateQueued, "") || !Internal(job.Kind) || !IsPodcast(job.Kind) {
		t.Fatal("podcast job exposed")
	}
	claimed, err := q.ClaimFor(ctx, q.token, []string{KindPodcastFetch}, 0)
	if err != nil || claimed == nil {
		t.Fatalf("claim %+v %v", claimed, err)
	}
	// A replacement cannot adopt the old claim. Cancellation must survive the
	// transaction, then a fresh intent may be queued after it settles.
	q.db.ExecContext(ctx, `UPDATE podcast_shows SET show_id='1111111111111111' WHERE library_id=?`, q.lib)
	replacement := json.RawMessage(`{"show_id":"1111111111111111","episode_id":"fedcba9876543210"}`)
	if _, _, err = q.Enqueue(ctx, NewJob{Kind: KindPodcastFetch, LibraryID: &q.lib, Path: &p, Params: replacement}); !errors.Is(err, ErrIdentityChanged) {
		t.Fatal(err)
	}
	old, err := q.Job(ctx, job.ID)
	if err != nil || !old.CancelRequested || old.Error != CancelReplacedRequest {
		t.Fatalf("cancel intent: %+v %v", old, err)
	}
	state := StateCancelled
	if old, err = q.UpdateHeld(ctx, job.ID, Update{Attempt: claimed.Attempts, State: &state, Error: ptr("stopped")}); err != nil || old.Error != CancelReplacedRequest {
		t.Fatalf("cancelled = %+v, %v; want the server's reason kept", old, err)
	}
	next, created, err := q.Enqueue(ctx, NewJob{Kind: KindPodcastFetch, LibraryID: &q.lib, Path: &p, Params: replacement})
	if err != nil || !created || next.ID == job.ID {
		t.Fatalf("replacement: %+v %v", next, err)
	}
	// A late request still naming the replaced identity is refused without
	// touching the current show's work, queued or running.
	for _, claim := range []bool{false, true} {
		if claim {
			if running, err := q.ClaimFor(ctx, q.token, []string{KindPodcastFetch}, 0); err != nil || running == nil || running.ID != next.ID {
				t.Fatalf("claim current %+v %v", running, err)
			}
		}
		if _, _, err = q.Enqueue(ctx, NewJob{Kind: KindPodcastFetch, LibraryID: &q.lib, Path: &p, Params: params}); !errors.Is(err, ErrIdentityChanged) {
			t.Fatalf("stale request: %v", err)
		}
		current, err := q.Job(ctx, next.ID)
		if err != nil || current.CancelRequested || current.Ended() {
			t.Fatalf("stale request cancelled current work: %+v %v", current, err)
		}
	}
}
func TestPodcastFetchRetainStrictAndHeld(t *testing.T) {
	q := fetchQueue(t)
	ctx := context.Background()
	p := "Show/episode.mp3"
	show := "Show"
	for _, raw := range []string{`{}`, `{"show_id":"0123456789abcdef"}`, `{"show_id":"0123456789abcdef","episode_id":"fedcba9876543210","url":"https://example.com/"}`} {
		if _, _, err := q.Enqueue(ctx, NewJob{Kind: KindPodcastFetch, LibraryID: &q.lib, Path: &p, Params: json.RawMessage(raw)}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("strict: %s %v", raw, err)
		}
	}
	for _, target := range []string{"Show/no.mp3", "Show/.hidden.mp3", "Show/x.wav", "Show/nested/episode.mp3"} {
		if _, _, err := q.Enqueue(ctx, NewJob{Kind: KindPodcastFetch, LibraryID: &q.lib, Path: &target, Params: json.RawMessage(`{"show_id":"0123456789abcdef","episode_id":"fedcba9876543210"}`)}); err == nil {
			t.Fatalf("target accepted: %s", target)
		}
	}
	if _, _, err := q.Enqueue(ctx, NewJob{Kind: KindPodcastRetain, LibraryID: &q.lib, Path: &show, Params: json.RawMessage(`{"show_id":"0123456789abcdef"}`), Priority: -20}); err != nil {
		t.Fatal(err)
	}
	q.ModuleOn = func(string) bool { return false }
	if job, err := q.ClaimFor(ctx, q.token, []string{KindPodcastRetain}, 0); err != nil || job != nil {
		t.Fatalf("module held %+v %v", job, err)
	}
	q.ModuleOn = func(string) bool { return true }
	job, err := q.ClaimFor(ctx, q.token, []string{KindPodcastRetain}, 0)
	if err != nil || job == nil {
		t.Fatalf("module released %+v %v", job, err)
	}
	message := "failed https://provider.example/private?secret=CANARY"
	result := json.RawMessage(`{"code":"failed","message":"https://provider.example/?secret=CANARY"}`)
	state := StateFailed
	ended, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: job.Attempts, State: &state, Error: &message, Result: result, Progress: json.RawMessage(`{"step":"fetching","bytes_received":3145728000,"bytes_total":null}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ended)
	if strings.Contains(string(raw), "CANARY") || !strings.Contains(string(raw), "3145728000") {
		t.Fatalf("redaction/counters %s", raw)
	}
}
