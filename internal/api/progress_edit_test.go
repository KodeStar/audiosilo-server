package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// patchProgress PATCHes the caller's own progress and decodes the answer.
func (e *rateEnv) patchProgress(t *testing.T, tok, path, body string) (int, catalog.Progress, string) {
	t.Helper()
	resp, raw := e.do(t, "PATCH", e.at("progress", path), tok, body)
	var out struct {
		Progress catalog.Progress `json:"progress"`
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, out.Progress, raw
}

func TestEditMyProgress(t *testing.T) {
	t.Parallel()
	e := newRateEnv(t)
	ctx := context.Background()
	ref := catalog.Ref{LibraryID: e.libID, Path: cradleBook}

	// Allowed: no progress yet on an indexed book in scope: the row is created,
	// started now.
	status, p, raw := e.patchProgress(t, e.kidTok, cradleBook, `{"position":5}`)
	if status != http.StatusOK || p.Position != 5 || p.Finished || p.StartedAt == "" || p.FinishedAt != "" ||
		strings.Contains(raw, `"finished_at"`) || p.Path != cradleBook || p.Version != 1 {
		t.Fatalf("new row = %d %s", status, raw)
	}
	if strings.Contains(raw, `"title"`) || strings.Contains(raw, `"author"`) {
		t.Fatalf("the player's progress shape, not the admin's: %s", raw)
	}
	started := p.StartedAt
	// A device save that knows the length (the fixtures have none without ffprobe).
	if _, err := e.cat.SaveProgress(ctx, e.kidID, catalog.Progress{Ref: ref, Position: 30, Duration: 120,
		UpdatedAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}

	// Mark finished: the position goes to the end, finished now.
	status, p, raw = e.patchProgress(t, e.kidTok, cradleBook, `{"finished":true}`)
	if status != http.StatusOK || !p.Finished || p.Position != 120 || p.FinishedAt == "" || p.StartedAt != started || p.Version != 3 {
		t.Fatalf("mark finished = %d %s", status, raw)
	}

	// Mark unfinished keeps the position and the start, clears the finish.
	status, p, raw = e.patchProgress(t, e.kidTok, cradleBook, `{"finished":false}`)
	if status != http.StatusOK || p.Finished || p.Position != 120 || p.FinishedAt != "" ||
		strings.Contains(raw, `"finished_at"`) || p.StartedAt != started || p.Version != 4 {
		t.Fatalf("mark unfinished = %d %s", status, raw)
	}
	// Position and dates; a day is the start of that day, server time.
	status, p, raw = e.patchProgress(t, e.kidTok, cradleBook, `{"position":30,"started_at":"2026-09-01"}`)
	startOfDay := time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local).UTC().Format(time.RFC3339)
	if status != http.StatusOK || p.Position != 30 || p.StartedAt != startOfDay {
		t.Fatalf("position + start = %d %s", status, raw)
	}
	// The read paths carry the dates too.
	if _, body := e.do(t, "GET", e.at("progress", cradleBook), e.kidTok, ""); !strings.Contains(body, `"started_at":"`+startOfDay+`"`) {
		t.Fatalf("GET progress = %s", body)
	}
	if _, body := e.do(t, "GET", "/api/v1/me/progress", e.kidTok, ""); !strings.Contains(body, `"started_at":"`+startOfDay+`"`) {
		t.Fatalf("GET /me/progress = %s", body)
	}

	future := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	for _, bad := range []string{
		`{"position":-1}`,
		`{"started_at":"` + future + `"}`,
		`{"finished":true,"started_at":"2026-09-10","finished_at":"2026-09-01"}`,
		`{"finished_at":"2026-09-02"}`, // the book isn't finished
		`{"finished":false,"finished_at":"2026-09-02"}`,
		`{"finished_at":"not a date"}`,
		`{"bogus":1}`,
		`not json`,
	} {
		if status, _, raw := e.patchProgress(t, e.kidTok, cradleBook, bad); status != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d %s, want 400", bad, status, raw)
		}
	}
	if got, _ := e.cat.GetProgress(ctx, e.kidID, ref); got.Position != 30 || got.Finished {
		t.Fatalf("a refused edit changed the row: %+v", got)
	}

	// No progress and no book: 404.
	if status, _, raw := e.patchProgress(t, e.kidTok, "Will Wight/Nothing", `{"finished":true}`); status != http.StatusNotFound ||
		!strings.Contains(raw, `"code":"book_not_found"`) {
		t.Fatalf("no row, no book = %d %s, want 404", status, raw)
	}

	// Denied: outside the caller's grant is 403, for a new row and for an existing
	// one (left from before access was narrowed), and changes nothing.
	if status, _, _ := e.patchProgress(t, e.kidTok, mistbornBook, `{"finished":true}`); status != http.StatusForbidden {
		t.Fatalf("new row outside the grant = %d, want 403", status)
	}
	if got, _ := e.cat.GetProgress(ctx, e.kidID, catalog.Ref{LibraryID: e.libID, Path: mistbornBook}); got != nil {
		t.Fatalf("a refused edit wrote progress: %+v", got)
	}
	if _, err := e.cat.SaveProgress(ctx, e.kidID, catalog.Progress{Ref: catalog.Ref{LibraryID: e.libID, Path: mistbornBook},
		Position: 7, Duration: 100}); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := e.patchProgress(t, e.kidTok, mistbornBook, `{"finished":true}`); status != http.StatusForbidden {
		t.Fatalf("existing row outside the grant = %d, want 403", status)
	}
	if got, _ := e.cat.GetProgress(ctx, e.kidID, catalog.Ref{LibraryID: e.libID, Path: mistbornBook}); got.Finished || got.Position != 7 {
		t.Fatalf("a refused edit changed the row: %+v", got)
	}
	if resp, _ := e.do(t, "PATCH", e.at("progress", cradleBook), "", `{"finished":true}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PATCH = %d, want 401", resp.StatusCode)
	}

	// Only the caller's own row: eve's progress on the same book is untouched.
	if got, _ := e.cat.GetProgress(ctx, e.eveID, ref); got != nil {
		t.Fatalf("kid's edits wrote eve's progress: %+v", got)
	}
	// An edit is not playback: no listening session.
	if sessions, _, err := e.cat.ListSessions(ctx, catalog.SessionFilter{UserID: e.kidID, Limit: 50}); err != nil || len(sessions) != 0 {
		t.Fatalf("edits recorded sessions: %+v %v", sessions, err)
	}
}

// A device can't set the dates through a save; the echo carries the server's.
func TestPutProgressIgnoresClientDates(t *testing.T) {
	t.Parallel()
	e := newRateEnv(t)
	forged := "2001-01-01T00:00:00Z"
	resp, body := e.do(t, "PUT", e.at("progress", cradlePart), e.kidTok,
		`{"position":5,"duration":100,"finished":true,"started_at":"`+forged+`","finished_at":"`+forged+`"}`)
	if resp.StatusCode != http.StatusOK || strings.Contains(body, forged) ||
		!strings.Contains(body, `"started_at":"`) || !strings.Contains(body, `"finished_at":"`) {
		t.Fatalf("PUT with dates = %d %s", resp.StatusCode, body)
	}
	if _, body := e.do(t, "GET", e.at("progress", cradlePart), e.kidTok, ""); strings.Contains(body, forged) {
		t.Fatalf("stored forged dates: %s", body)
	}
}

// The caller's edit wins over an older device save (last-write-wins, server time,
// a higher version), and a newer device save wins over it.
func TestEditMyProgressLastWriteWins(t *testing.T) {
	t.Parallel()
	e := newRateEnv(t)
	save := func(pos float64, at time.Time) catalog.Progress {
		t.Helper()
		resp, body := e.do(t, "PUT", e.at("progress", cradleBook), e.kidTok,
			`{"position":`+strconv.FormatFloat(pos, 'f', -1, 64)+`,"duration":1000,"updated_at":"`+at.UTC().Format(time.RFC3339)+`"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("save = %d %s", resp.StatusCode, body)
		}
		var out struct {
			Progress catalog.Progress `json:"progress"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return out.Progress
	}
	now := time.Now()
	save(50, now.Add(-time.Hour))
	if status, p, raw := e.patchProgress(t, e.kidTok, cradleBook, `{"position":10}`); status != http.StatusOK || p.Position != 10 {
		t.Fatalf("edit after an older save = %d %s", status, raw)
	}
	// A device save from before the edit (an offline replay) loses.
	if p := save(70, now.Add(-30*time.Minute)); p.Position != 10 {
		t.Fatalf("an older save beat the edit: %+v", p)
	}
	// A newer one, from a device with the book loaded, wins.
	if p := save(80, now.Add(2*time.Minute)); p.Position != 80 {
		t.Fatalf("a newer save lost to the edit: %+v", p)
	}
}

// An exact (RFC3339) date in the future is refused, start or finish, beyond a
// client clock running a little fast; a day-only start keeps its day of slack
// (the client's today can be the server's tomorrow). A future finish would
// otherwise sit outside this year's finished books until it came.
func TestEditMyProgressFutureDates(t *testing.T) {
	t.Parallel()
	e := newRateEnv(t)
	at := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }
	for _, bad := range []string{
		`{"finished":true,"finished_at":"` + at(2*time.Hour) + `"}`,
		`{"finished":true,"finished_at":"` + at(20*time.Hour) + `"}`,
		`{"started_at":"` + at(2*time.Hour) + `"}`,
	} {
		if status, _, raw := e.patchProgress(t, e.kidTok, cradleBook, bad); status != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d %s, want 400", bad, status, raw)
		}
	}
	if got, _ := e.cat.GetProgress(context.Background(), e.kidID, catalog.Ref{LibraryID: e.libID, Path: cradleBook}); got != nil {
		t.Fatalf("a refused edit wrote progress: %+v", got)
	}

	// Allowed: a finish a minute ahead (a fast client clock), and a day-only start
	// of tomorrow.
	if status, p, raw := e.patchProgress(t, e.kidTok, cradleBook,
		`{"finished":true,"finished_at":"`+at(time.Minute)+`"}`); status != http.StatusOK || !p.Finished {
		t.Fatalf("a finish a minute ahead = %d %s, want 200", status, raw)
	}
	tomorrow := time.Now().AddDate(0, 0, 1).Format(time.DateOnly)
	if status, _, raw := e.patchProgress(t, e.eveTok, mistbornBook, `{"started_at":"`+tomorrow+`"}`); status != http.StatusOK {
		t.Fatalf("a day-only start of tomorrow = %d %s, want 200", status, raw)
	}
}

// An edit that sets nothing ({} or null) writes nothing: it doesn't start a book
// (no row: 404) or re-stamp an existing row, which would make a device's pending
// older save lose to it.
func TestEditMyProgressEmptyEditWritesNothing(t *testing.T) {
	t.Parallel()
	e := newRateEnv(t)
	ctx := context.Background()
	ref := catalog.Ref{LibraryID: e.libID, Path: cradleBook}
	for _, body := range []string{`{}`, `null`} {
		if status, _, raw := e.patchProgress(t, e.kidTok, cradleBook, body); status != http.StatusNotFound {
			t.Fatalf("PATCH %s with no progress = %d %s, want 404", body, status, raw)
		}
	}
	if got, _ := e.cat.GetProgress(ctx, e.kidID, ref); got != nil {
		t.Fatalf("an empty edit started the book: %+v", got)
	}
	saved, err := e.cat.SaveProgress(ctx, e.kidID, catalog.Progress{Ref: ref, Position: 50, Duration: 120,
		UpdatedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	status, p, raw := e.patchProgress(t, e.kidTok, cradleBook, `{}`)
	if status != http.StatusOK || p.Position != 50 || p.Version != saved.Version || p.UpdatedAt != saved.UpdatedAt {
		t.Fatalf("PATCH {} on a row = %d %s, want it unchanged (%+v)", status, raw, saved)
	}
	// A save the device made half an hour ago still lands.
	if got, err := e.cat.SaveProgress(ctx, e.kidID, catalog.Progress{Ref: ref, Position: 70, Duration: 120,
		UpdatedAt: time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)}); err != nil || got.Position != 70 {
		t.Fatalf("a later device save after an empty edit = %+v %v", got, err)
	}
}
