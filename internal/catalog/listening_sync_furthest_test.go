package catalog

import (
	"testing"
	"time"
)

// audibleBatch is an Audible account's furthest-only batch (Bridge Phase 4c),
// as the Bridge's route builds it: device audible:<account>, no token.
func (f *syncFixture) audibleBatch(t *testing.T, events ...SyncEvent) SyncBatch {
	t.Helper()
	b := f.batch(t, "audible:acct0001", events...)
	b.TokenID, b.DeviceName, b.Client, b.FurthestOnly = 0, "Audible (Chris US)", Client{}, true
	return b
}

// audiblePlace is Audible's place as the Bridge posts it: a span of no length
// at pos, at its last_updated (ago before the clock), a jump.
func (f *syncFixture) audiblePlace(pos float64, ago time.Duration) SyncEvent {
	at := f.clock.Add(-ago).Format(time.RFC3339)
	return SyncEvent{ID: nextSyncID("aud-"), Kind: EventListen, LibraryID: f.book.LibraryID, Path: f.book.Path,
		FromPos: pos, ToPos: pos, Start: "jump", StartedAt: at, EndedAt: at}
}

// Furthest only: an Audible place ahead applies over another device's row,
// even one saved later, never moving the row's time back; one behind or at
// the stored place (within a second) is not_applied "behind" with no offer and
// the row untouched; a finished row is at its end; an unfinish is behind; a
// finish applies. A replay is a duplicate.
func TestSyncFurthestOnly(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	wantResult(t, f.syncOne(t, "phone", f.listen("resume", 1000, 2000, time.Minute, base(0))), statusApplied, "")
	before := f.progress(t)

	behind := f.sync(t, f.audibleBatch(t, f.audiblePlace(1500, 2*time.Minute))).Results[0]
	if behind.Status != statusNotApplied || behind.Reason != reasonBehind || behind.OtherPlace != nil {
		t.Fatalf("a place behind: %+v", behind)
	}
	wantResult(t, f.sync(t, f.audibleBatch(t, f.audiblePlace(2000.5, time.Second))).Results[0], statusNotApplied, reasonBehind)
	if p := f.progress(t); p.Version != before.Version || p.Position != 2000 || p.DeviceID != "phone" || p.OtherPlace != nil {
		t.Fatalf("row after places behind: %+v", p)
	}

	// Ahead, though Audible's place is older than the phone's save.
	ahead := f.audiblePlace(3000, 10*time.Minute)
	wantResult(t, f.sync(t, f.audibleBatch(t, ahead)).Results[0], statusApplied, "")
	p := f.progress(t)
	if p.Position != 3000 || p.DeviceID != "audible:acct0001" || p.DeviceName != "Audible (Chris US)" ||
		p.Version != before.Version+1 || p.UpdatedAt != before.UpdatedAt || p.OtherPlace != nil {
		t.Fatalf("row after a place ahead: %+v (before %+v)", p, before)
	}
	wantResult(t, f.sync(t, f.audibleBatch(t, ahead)).Results[0], statusDuplicate, "")

	// The phone, which saw version 1, carries on from 2000 to 2100: diverged
	// from Audible's place, which stays (further) and offers the phone's.
	res := f.syncOne(t, "phone", f.listen("continue", 2000, 2100, 0, base(1)))
	if res.Status != statusDiverged || res.Kept != "stored" {
		t.Fatalf("the phone after Audible: %+v", res)
	}

	wantResult(t, f.sync(t, f.audibleBatch(t, f.finishEvent(EventFinish, 0))).Results[0], statusApplied, "")
	if p := f.progress(t); !p.Finished || p.DeviceID != "audible:acct0001" {
		t.Fatalf("row after Audible's finish: %+v", p)
	}
	f.clock = f.clock.Add(time.Minute)
	wantResult(t, f.sync(t, f.audibleBatch(t, f.audiblePlace(60000, 0))).Results[0], statusNotApplied, reasonBehind)
	wantResult(t, f.sync(t, f.audibleBatch(t, f.finishEvent(EventUnfinish, 0))).Results[0], statusNotApplied, reasonBehind)
	if p := f.progress(t); !p.Finished {
		t.Fatalf("a finished row moved: %+v", p)
	}
}

// Audible's places are a place only: a furthest-only listen records no
// listening session, daily roll-up or history, and its events name no token.
// With no row yet, a place applies.
func TestSyncFurthestOnlyRecordsNoListening(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	ev := f.audiblePlace(5000, time.Minute)
	ev.StartedAt = f.clock.Add(-time.Hour).Format(time.RFC3339) // even a span with a length
	wantResult(t, f.sync(t, f.audibleBatch(t, ev)).Results[0], statusApplied, "")
	if p := f.progress(t); p == nil || p.Position != 5000 {
		t.Fatalf("row = %+v", p)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM listening_sessions`,
		`SELECT COUNT(*) FROM listening_daily`,
		`SELECT COUNT(*) FROM listening_history`,
		`SELECT COUNT(*) FROM listening_events WHERE token_id <> 0`,
	} {
		if n := f.count(t, q); n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM listening_events WHERE device_id = 'audible:acct0001'`); n != 1 {
		t.Fatalf("events logged = %d, want 1", n)
	}
}
