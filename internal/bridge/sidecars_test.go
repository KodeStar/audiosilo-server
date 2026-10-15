package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// beatLibrary records a heartbeat offering kinds with the test library mapped as
// given.
func (q *testQueue) beatLibrary(t *testing.T, mapped, writable bool, kinds ...string) {
	t.Helper()
	st := Status{Version: "0.2.0", Kinds: kinds,
		Libraries: []LibraryStatus{{LibraryID: q.lib, MountPath: "/media/books", Mapped: mapped, Writable: writable}}}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveHeartbeat(context.Background(), q.token, st); err != nil {
		t.Fatal(err)
	}
}

func (q *testQueue) sidecars(params string, path *string) (*Job, bool, error) {
	j := NewJob{Kind: KindSidecars, LibraryID: &q.lib, Path: path, RequestedBy: q.admin}
	if params != "" {
		j.Params = json.RawMessage(params)
	}
	return q.Enqueue(context.Background(), j)
}

func TestSidecarsParams(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	q.beatLibrary(t, true, true, KindVerify, KindSidecars)
	// Defaults are filled in, so the Bridge reads one complete object.
	job, created, err := q.sidecars("", ptr("Author/Book"))
	if err != nil || !created || string(job.Params) != `{"source":"auto","contribute":"local"}` {
		t.Fatalf("defaults: %v, %v, %+v", err, created, job)
	}
	job, _, err = q.sidecars(`{"source":"ebook","contribute":"issue","work_id":"the-way-of-kings"}`, ptr("Author/Other"))
	if err != nil || string(job.Params) != `{"source":"ebook","contribute":"issue","work_id":"the-way-of-kings"}` {
		t.Fatalf("explicit: %v, %+v", err, job)
	}
	for name, params := range map[string]string{
		"unknown field":     `{"source":"audio","rm":"-rf"}`,
		"bad source":        `{"source":"video"}`,
		"bad contribute":    `{"contribute":"upstream"}`,
		"source wrong type": `{"source":1}`,
		"work id too long":  `{"work_id":"` + strings.Repeat("w", maxWorkIDLen+1) + `"}`,
		"work id control":   `{"work_id":"the-way\nof-kings"}`,
		"not an object":     `["auto"]`,
	} {
		if _, _, err := q.sidecars(params, ptr("Author/Third")); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	// A book is required: the library alone is refused.
	if _, _, err := q.sidecars("", nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("no path: %v, want ErrInvalid", err)
	}
}

// Security-relevant: a job that writes is queued only for a library the Bridge
// reports mapped AND writable; bridge.verify keeps working on any library.
func TestSidecarsNeedsAWritableLibrary(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	book := ptr("Author/Book")
	for name, l := range map[string][2]bool{"unmapped": {false, true}, "read only": {true, false}, "neither": {false, false}} {
		q.beatLibrary(t, l[0], l[1], KindVerify, KindSidecars)
		if _, _, err := q.sidecars("", book); !errors.Is(err, ErrLibraryNotWritable) {
			t.Errorf("denied (%s): %v, want ErrLibraryNotWritable", name, err)
		}
		if _, created, err := q.Enqueue(context.Background(), NewJob{Kind: KindVerify, LibraryID: &q.lib, Path: ptr(name), RequestedBy: q.admin}); err != nil || !created {
			t.Errorf("bridge.verify on a %s library: %v, %v", name, created, err)
		}
	}
	// A heartbeat that doesn't mention the library at all.
	q.beat(t, KindVerify, KindSidecars)
	if _, _, err := q.sidecars("", book); !errors.Is(err, ErrLibraryNotWritable) {
		t.Errorf("denied (not reported): %v, want ErrLibraryNotWritable", err)
	}
	// Allowed: mapped and writable. A second press returns the same job.
	q.beatLibrary(t, true, true, KindVerify, KindSidecars)
	first, created, err := q.sidecars(`{"source":"audio"}`, book)
	if err != nil || !created || first.Kind != KindSidecars || *first.Path != "Author/Book" {
		t.Fatalf("allowed: %v, %v, %+v", err, created, first)
	}
	again, created, err := q.sidecars(`{"source":"ebook"}`, book)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("second press: %v, %v, %+v; want the open job", err, created, again)
	}
	// The open job is still answered once the library turns read-only (nothing
	// new is queued).
	q.beatLibrary(t, true, false, KindVerify, KindSidecars)
	if again, created, err := q.sidecars("", book); err != nil || created || again.ID != first.ID {
		t.Fatalf("second press, read-only now: %v, %v, %+v", err, created, again)
	}
	open, err := q.OpenJob(context.Background(), KindSidecars, q.lib, "Author/Book")
	if err != nil || open == nil || open.ID != first.ID {
		t.Fatalf("OpenJob = %+v, %v", open, err)
	}
	if none, err := q.OpenJob(context.Background(), KindSidecars, q.lib, "Author/Other"); err != nil || none != nil {
		t.Fatalf("OpenJob for a book with none = %+v, %v", none, err)
	}
}

func TestFeaturesIncludeSidecars(t *testing.T) {
	t.Parallel()
	st := Status{Kinds: []string{KindSidecars, KindVerify}}
	if got := st.Features(); !slices.Equal(got, []string{"bridge", "sidecars"}) {
		t.Fatalf("Features = %v", got)
	}
}

// heard records what OnTransition is told, as "from>to:park code".
func (q *testQueue) heard() *[]string {
	var got []string
	q.OnTransition = func(_ context.Context, j Job, from string) {
		got = append(got, from+">"+j.State+":"+j.ParkCode())
	}
	return &got
}

// UpdateHeld reports the moves an admin is told about (OnTransition, by
// Notable): into done, failed or needs_attention (not a park that resumes by
// itself), once each; a keepalive is never one.
func TestUpdateHeldReportsTheMove(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	heard := q.heard()
	job := q.verify(t, "A", 0)
	if _, err := q.ClaimFor(ctx, q.token, nil, 0); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		state    string
		progress string
	}{
		{StateRunning, `{"step":"asr"}`},
		{StateRunning, `{"step":"asr","done":2}`}, // a keepalive
		{StateNeedsAttention, `{"step":"waiting","park":{"code":"agent_unavailable","hint":"x"}}`},
		{StateNeedsAttention, `{"step":"waiting","park":{"code":"agent_unavailable"}}`},
		{StateRunning, `{"step":"fact_pass"}`},
		// A park that resumes by itself is not news.
		{StateNeedsAttention, `{"step":"waiting","park":{"code":"agent_rate_limited","retry_at":"2026-10-15T20:00:00Z"}}`},
		{StateRunning, `{"step":"fact_pass"}`},
		{StateDone, ``},
	} {
		u := Update{Attempt: 1, State: ptr(step.state)}
		if step.progress != "" {
			u.Progress = json.RawMessage(step.progress)
		}
		if _, err := q.UpdateHeld(ctx, job.ID, u); err != nil {
			t.Fatalf("%s: %v", step.state, err)
		}
	}
	want := []string{"running>needs_attention:agent_unavailable", "running>done:"}
	if !slices.Equal(*heard, want) {
		t.Fatalf("heard = %v, want %v", *heard, want)
	}
}

// A lease that runs out on the last attempt fails the job, and an admin hears
// about it once (whichever call expires it); one that runs out earlier requeues
// the job, which is not news, and neither is a keepalive.
func TestLeaseExpiryReportsTheFailure(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	heard := q.heard()
	job := q.verify(t, "A", 0)
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if j, err := q.claim(ctx, q.token, []string{KindVerify}); err != nil || j == nil {
			t.Fatalf("claim %d = %+v, %v", attempt, j, err)
		}
		if _, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: attempt, State: ptr(StateRunning)}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.UpdateHeld(ctx, job.ID, Update{Attempt: attempt, Progress: json.RawMessage(`{"step":"hold"}`)}); err != nil {
			t.Fatal(err)
		}
		if len(*heard) != 0 {
			t.Fatalf("attempt %d: heard %v before the lease ran out", attempt, *heard)
		}
		q.advance(Lease + time.Second)
		// A console read expires it (expireDue), and the reads after it find
		// nothing more to expire.
		for range 2 {
			if _, _, err := q.List(ctx, Filter{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if want := []string{"running>failed:"}; !slices.Equal(*heard, want) {
		t.Fatalf("heard = %v, want %v", *heard, want)
	}
	if j, _ := q.Job(ctx, job.ID); j.State != StateFailed {
		t.Fatalf("job = %+v", j)
	}

	// Expired inside a write (here a claim) rather than a read: heard the same.
	other := q.verify(t, "B", 0)
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if j, err := q.claim(ctx, q.token, []string{KindVerify}); err != nil || j == nil || j.ID != other.ID {
			t.Fatalf("claim %d = %+v, %v", attempt, j, err)
		}
		q.advance(Lease + time.Second)
	}
	if _, err := q.claim(ctx, q.token, []string{KindVerify}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"running>failed:", "claimed>failed:"}; !slices.Equal(*heard, want) {
		t.Fatalf("heard = %v, want %v", *heard, want)
	}
}

func TestParkCode(t *testing.T) {
	t.Parallel()
	for progress, want := range map[string]string{
		`{"park":{"code":"core_needed"}}`: "core_needed",
		`{"step":"asr"}`:                  "",
		`{"park":null}`:                   "",
		`{"park":"x"}`:                    "",
		`not json`:                        "",
		`{"park":{"code":"a\u0000b"}}`:    "a b",
	} {
		if got := (&Job{Progress: json.RawMessage(progress)}).ParkCode(); got != want {
			t.Errorf("ParkCode(%s) = %q, want %q", progress, got, want)
		}
	}
}

// decodeStatus decodes a heartbeat as the API does: unknown fields refused.
func decodeStatus(t *testing.T, body string) (Status, error) {
	t.Helper()
	var st Status
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return st, err
	}
	return st, st.Validate()
}

func TestHeartbeatEstimates(t *testing.T) {
	t.Parallel()
	st, err := decodeStatus(t, `{"version":"0.2.0","estimates":{"sidecars":{"asr_seconds_per_audio_hour":900,
		"agent_usd_per_audio_hour":1.6,"agent_usd_per_ebook":null,"samples":12,"agent_backend":"claude\u0007","agent_ready":true,
		"subscription":true,"monthly_budget_usd":100,"month_spent_usd":12.3,"future_figure":7},"read_along":{"x":1}}}`)
	if err != nil {
		t.Fatalf("a full estimate with unknown keys: %v", err)
	}
	sc := st.Estimates.Sidecars
	if sc == nil || sc.ASRSecondsPerAudioHour != 900 || sc.AgentUSDPerAudioHour == nil || *sc.AgentUSDPerAudioHour != 1.6 ||
		sc.AgentUSDPerEbook != nil || sc.Samples != 12 || sc.AgentBackend != "claude " || !sc.AgentReady || !sc.Subscription ||
		sc.MonthlyBudgetUSD != 100 || sc.MonthSpentUSD != 12.3 {
		t.Fatalf("estimates = %+v", sc)
	}
	// Stored and read back (the database's copy is decoded leniently).
	body, _ := json.Marshal(st)
	var back Status
	if err := json.Unmarshal(body, &back); err != nil || back.Estimates == nil || back.Estimates.Sidecars.Samples != 12 {
		t.Fatalf("round trip: %v, %s", err, body)
	}
	if st, err := decodeStatus(t, `{"version":"0.2.0"}`); err != nil || st.Estimates != nil {
		t.Fatalf("no estimates: %+v, %v", st.Estimates, err)
	}
	for name, est := range map[string]string{
		"negative asr":     `{"asr_seconds_per_audio_hour":-1}`,
		"huge asr":         `{"asr_seconds_per_audio_hour":1e12}`,
		"negative cost":    `{"agent_usd_per_audio_hour":-0.5}`,
		"huge ebook cost":  `{"agent_usd_per_ebook":1e9}`,
		"negative samples": `{"samples":-3}`,
		"negative budget":  `{"monthly_budget_usd":-100}`,
		"negative spent":   `{"month_spent_usd":-1}`,
		"wrong type":       `{"samples":"twelve"}`,
	} {
		// Dropped, never refused: a refused heartbeat takes the Bridge offline.
		st, err := decodeStatus(t, `{"estimates":{"sidecars":`+est+`}}`)
		if err != nil || (st.Estimates != nil && st.Estimates.Sidecars != nil) {
			t.Errorf("%s: kept %+v, %v", name, st.Estimates, err)
		}
	}
}

func TestEstimateSidecars(t *testing.T) {
	t.Parallel()
	est := &SidecarsEstimates{ASRSecondsPerAudioHour: 900, AgentUSDPerAudioHour: ptr(1.6), AgentUSDPerEbook: ptr(4.0),
		AgentReady: true, Subscription: true}
	// Ten hours of audio: 2.5 hours of ASR, 16 dollars of agent.
	c := EstimateSidecars(36000, false, est)
	if c.Source != SourceAudio || c.ASRHours != 2.5 || c.AgentCostUSD == nil || *c.AgentCostUSD != 16 || !c.AgentReady || !c.Subscription {
		t.Fatalf("audio = %+v", c)
	}
	// An ebook needs no ASR and costs its flat figure.
	c = EstimateSidecars(36000, true, est)
	if c.Source != SourceEbook || c.ASRHours != 0 || c.AgentCostUSD == nil || *c.AgentCostUSD != 4 {
		t.Fatalf("ebook = %+v", c)
	}
	// No history yet: no cost, the ASR time still.
	c = EstimateSidecars(5400, false, &SidecarsEstimates{ASRSecondsPerAudioHour: 600})
	if c.AgentCostUSD != nil || c.ASRHours != 0.25 {
		t.Fatalf("no history = %+v", c)
	}
	if c := EstimateSidecars(5400, true, &SidecarsEstimates{}); c.AgentCostUSD != nil {
		t.Fatalf("no ebook history = %+v", c)
	}
	// No figures at all (an older Bridge): nothing but the source.
	if c := EstimateSidecars(36000, false, nil); c.Source != SourceAudio || c.ASRHours != 0 || c.AgentCostUSD != nil || c.AgentReady {
		t.Fatalf("no figures = %+v", c)
	}
	// Rounded to hundredths; a nonsense duration costs nothing.
	if c := EstimateSidecars(1000, false, &SidecarsEstimates{ASRSecondsPerAudioHour: 1000, AgentUSDPerAudioHour: ptr(1.0)}); c.ASRHours != 0.08 || *c.AgentCostUSD != 0.28 {
		t.Fatalf("rounding = %+v", c)
	}
	if c := EstimateSidecars(-10, false, est); c.ASRHours != 0 || *c.AgentCostUSD != 0 {
		t.Fatalf("negative duration = %+v", c)
	}
}
