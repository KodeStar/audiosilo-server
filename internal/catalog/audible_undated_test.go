package catalog

import (
	"maps"
	"slices"
	"testing"
	"time"
)

// The Bridge's event id for a place Audible sent no last_updated for
// (CROSS-REPO §23's formula, its last_updated empty), pinned by the values the
// Bridge's own audiblesync.EventID gives for this place: the listen and the
// finish.
func TestAudibleUndatedID(t *testing.T) {
	t.Parallel()
	ref := Ref{LibraryID: 1, Path: "Author/Book"}
	for kind, want := range map[string]string{
		"":          "aud-bcba95b12c2bb665a13b0dcc0cb5ddac",
		EventFinish: "aud-71841ac49adbb0c3fce441ddea6c829a",
	} {
		if got := audibleUndatedID("acct0001", "B000000001", 500000, kind, ref); got != want {
			t.Errorf("kind %q: id %s, want %s", kind, got, want)
		}
	}
}

// AudibleUndatedEvents picks the events the Bridge dated on its own clock by
// their ids: a listen whose id is the hash of its own place under the book's
// synced title with no last_updated, and the finish at that place in the same
// batch. Not an id made with a last_updated (any other id), one for another
// place than the event's, a finish whose listen is not in the batch, nor a
// book that is none of the account's titles'.
func TestAudibleUndatedEvents(t *testing.T) {
	t.Parallel()
	const path = "Author/Book"
	f := syncedTitleAt(t, path)
	ref := Ref{LibraryID: f.lib.ID, Path: path}
	other := Ref{LibraryID: f.lib.ID, Path: "Author/Other"}
	id := func(ms int64, kind string, r Ref) string {
		return audibleUndatedID("acct0001", syncedASIN, ms, kind, r)
	}
	listen := func(id string, r Ref, pos float64) SyncEvent {
		return SyncEvent{ID: id, Kind: EventListen, LibraryID: r.LibraryID, Path: r.Path, FromPos: pos, ToPos: pos}
	}
	finish := func(id string, r Ref) SyncEvent {
		return SyncEvent{ID: id, Kind: EventFinish, LibraryID: r.LibraryID, Path: r.Path}
	}
	got, err := f.c.AudibleUndatedEvents(f.ctx, "acct0001", []SyncEvent{
		listen(id(1234500, "", ref), ref, 1234.5),
		finish(id(1234500, EventFinish, ref), ref),
		listen("aud-0123456789abcdef0123456789abcdef", ref, 1234.5), // made with a last_updated
		listen(id(600000, "", ref), ref, 700),                       // another place's id
		finish(id(900000, EventFinish, ref), ref),                   // its listen is not here
		listen(id(500000, "", other), other, 500),                   // no title of the account's
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{id(1234500, "", ref), id(1234500, EventFinish, ref)}
	slices.Sort(want)
	if ids := slices.Sorted(maps.Keys(got)); !slices.Equal(ids, want) {
		t.Fatalf("undated = %v, want %v", ids, want)
	}
}

// server#415: a furthest-only batch's clock shift moves only the events its
// sender dated itself (SenderDated). With the Bridge's clock ten minutes
// behind and a restart here five minutes ago, the place it stamped with its
// own clock is corrected to now and applies; one with Audible's own time, the
// same instant, stays there and is behind. Each is logged with its own shift.
func TestSyncFurthestOnlyShiftsSenderDated(t *testing.T) {
	t.Parallel()
	const behind = 10 * time.Minute
	f := newSyncFixture(t)
	wantResult(t, f.syncOne(t, "phone", f.listen("resume", 0, 1000, 20*time.Minute, base(0))), statusApplied, "")
	if _, err := f.c.db.ExecContext(f.ctx, `UPDATE progress SET finish_changed_at = ? WHERE user_id = ?`,
		FormatStamp(f.clock.Add(-5*time.Minute)), f.user); err != nil {
		t.Fatal(err)
	}
	dated, undated := f.audiblePlace(3000, behind), f.audiblePlace(2000, behind)
	b := f.audibleBatch(t, dated, undated)
	sent := f.clock.Add(-behind)
	b.SentAt, b.SenderDated = &sent, map[string]bool{undated.ID: true}
	res := f.sync(t, b).Results
	wantResult(t, res[0], statusNotApplied, reasonBehind)
	wantResult(t, res[1], statusApplied, "")
	if p := f.progress(t); p.Position != 2000 {
		t.Fatalf("row %+v, want the sender-dated place", p)
	}
	for id, want := range map[string]int{dated.ID: 0, undated.ID: int(behind.Seconds())} {
		if got := f.count(t, `SELECT CAST(clock_shift AS INTEGER) FROM listening_events WHERE event_id = ?`, id); got != want {
			t.Errorf("event %s logged with shift %d s, want %d", id, got, want)
		}
	}
}
