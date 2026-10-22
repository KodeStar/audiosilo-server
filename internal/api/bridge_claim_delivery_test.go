package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/bridge"
)

type claimResponseWriter struct {
	*httptest.ResponseRecorder
	write    func([]byte) (int, error)
	flushErr error
}

func (w claimResponseWriter) Write(p []byte) (int, error) { return w.write(p) }
func (w claimResponseWriter) FlushError() error {
	if w.flushErr != nil {
		return w.flushErr
	}
	w.Flush()
	return nil
}

func TestBridgeClaimDeliveryCleanup(t *testing.T) {
	for _, mode := range []string{"normal", "cancelled", "write_failed", "flush_failed", "running", "acknowledged", "server_stopping"} {
		t.Run(mode, func(t *testing.T) {
			b := newBridgeEnv(t)
			queued := b.queue(t, mistbornBook, `{}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server, stop := context.WithCancel(context.Background())
			defer stop()
			b.api.SetBaseContext(server)
			r := httptest.NewRequest("GET", "/api/v1/bridge/jobs/next?wait=0", nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+b.bridgeTok)
			recorder := httptest.NewRecorder()
			w := claimResponseWriter{ResponseRecorder: recorder, write: func(p []byte) (int, error) {
				var claim bridge.Job
				if err := json.Unmarshal(p, &claim); err != nil || claim.ID != queued.ID || claim.Attempts != 1 {
					t.Fatalf("claim response %s, %v", p, err)
				}
				if mode == "running" || mode == "acknowledged" {
					u := bridge.Update{Attempt: claim.Attempts}
					if mode == "running" {
						state := bridge.StateRunning
						u.State = &state
					}
					if _, err := b.api.bridge.UpdateHeld(context.Background(), claim.ID, u); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "write_failed" {
					return 0, errors.New("response connection closed")
				}
				switch mode {
				case "server_stopping":
					// A reply that is written still reaches the Bridge.
					stop()
				case "normal", "flush_failed":
				default:
					cancel() // The lease has committed before writing the response.
				}
				return recorder.Write(p)
			}}
			if mode == "flush_failed" {
				w.flushErr = errors.New("response flush failed")
			}
			b.srv.Config.Handler.ServeHTTP(w, r)
			got, err := b.api.bridge.Job(context.Background(), queued.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantState, wantAttempts := bridge.StateClaimed, 1
			if mode == "cancelled" || mode == "write_failed" || mode == "flush_failed" {
				wantState, wantAttempts = bridge.StateQueued, 0
			}
			if mode == "running" {
				wantState = bridge.StateRunning
			}
			if got.State != wantState || got.Attempts != wantAttempts {
				t.Fatalf("claim after delivery %+v, want %s/%d", got, wantState, wantAttempts)
			}
		})
	}
}

func TestBridgeClaimCancelledAfterCommitBeforeWrite(t *testing.T) {
	b := newBridgeEnv(t)
	queued := b.queue(t, mistbornBook, `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Expiring another job reports its transition after the claim transaction
	// commits, letting the request end before any response is written.
	if _, err := b.db.ExecContext(ctx, `INSERT INTO bridge_jobs(kind,state,attempts,lease_until,dedupe_key,created_at,updated_at)
	 VALUES('bridge.verify','claimed',3,'2000-01-01T00:00:00Z','expired','t','t')`); err != nil {
		t.Fatal(err)
	}
	committed := false
	b.api.bridge.OnTransition = func(_ context.Context, _ bridge.Job, _ string) {
		job, err := b.api.bridge.Job(context.Background(), queued.ID)
		if err != nil || job.State != bridge.StateClaimed || job.Attempts != 1 {
			t.Fatalf("claim did not commit: %+v, %v", job, err)
		}
		committed = true
		cancel()
	}
	r := httptest.NewRequest("GET", "/api/v1/bridge/jobs/next?wait=0", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+b.bridgeTok)
	w := httptest.NewRecorder()
	b.srv.Config.Handler.ServeHTTP(w, r)
	if !committed || w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("cancelled response: committed=%v status=%d body=%s", committed, w.Code, w.Body)
	}
	job, err := b.api.bridge.Job(context.Background(), queued.ID)
	if err != nil || job.State != bridge.StateQueued || job.Attempts != 0 || job.LeaseUntil != nil {
		t.Fatalf("cancelled claim not released: %+v, %v", job, err)
	}
}

func TestBridgeClaimDeadlineAnswers503(t *testing.T) {
	b := newBridgeEnv(t)
	b.api.timeoutDur = 50 * time.Millisecond
	r := httptest.NewRequest("GET", "/api/v1/bridge/jobs/next?wait=5", nil)
	r.Header.Set("Authorization", "Bearer "+b.bridgeTok)
	w := httptest.NewRecorder()
	b.srv.Config.Handler.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("claim past its deadline: %d %s", w.Code, w.Body)
	}
}
