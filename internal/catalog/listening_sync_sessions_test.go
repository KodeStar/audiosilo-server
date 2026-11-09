package catalog

import (
	"testing"
	"time"
)

// A sync finish marks the session it ends finished, as a PUT saying finished
// does (server#284): the token's session on the book whose last span ended
// within SessionGap of it, whether the row applied the finish or found it
// older. A finish long after the device's last span, or another device's,
// marks nothing and opens no session.
func TestSyncFinishMarksItsSession(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	sessions := func() []Session {
		t.Helper()
		s, _, err := f.c.ListSessions(f.ctx, SessionFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	f.syncOne(t, "phone", f.listen("resume", syncBookLen-600, syncBookLen, 2*time.Minute, nil))
	f.syncOne(t, "tablet", f.listen("resume", 100, 700, time.Minute, nil))
	wantResult(t, f.syncOne(t, "phone", f.finishEvent(EventFinish, 0)), "applied", "")
	got := sessions()
	if len(got) != 2 {
		t.Fatalf("sessions = %+v", got)
	}
	for _, s := range got {
		if want := s.DeviceName == "phone's phone"; s.Finished != want {
			t.Fatalf("session %s finished = %v, want %v", s.DeviceName, s.Finished, want)
		}
	}

	// A finish older than the row's last finish-state change still marks the
	// session it was made in.
	g := newSyncFixture(t)
	g.syncOne(t, "phone", g.listen("resume", 0, 600, 20*time.Minute, nil))
	wantResult(t, g.syncOne(t, "tablet", g.finishEvent(EventFinish, 0)), "applied", "")
	wantResult(t, g.syncOne(t, "phone", g.finishEvent(EventFinish, 15*time.Minute)), "not_applied", "older")
	if s, _, err := g.c.ListSessions(g.ctx, SessionFilter{}); err != nil || len(s) != 1 || !s[0].Finished {
		t.Fatalf("sessions after an older finish = %+v, %v", s, err)
	}

	// Long after the last span: nothing marked, nothing opened.
	h := newSyncFixture(t)
	h.syncOne(t, "phone", h.listen("resume", 0, 600, 2*time.Hour, nil))
	wantResult(t, h.syncOne(t, "phone", h.finishEvent(EventFinish, 0)), "applied", "")
	if n := h.count(t, `SELECT COUNT(*) FROM listening_sessions`); n != 1 {
		t.Fatalf("%d sessions, want the finish to open none", n)
	}
	if n := h.count(t, `SELECT COUNT(*) FROM listening_sessions WHERE finished = 1`); n != 0 {
		t.Fatalf("a finish 2 h after the last span marked its session")
	}
}

// A span synced late into a session that already holds a later one adds its
// listening but leaves the session's place, length and speed the later span's
// (server#284): they say where the session is now.
func TestSyncLateSpanKeepsSessionSpeed(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	first := f.listen("resume", 0, 900, 45*time.Minute, nil)       // 07:00-07:15
	middle := f.listen("continue", 900, 1200, 40*time.Minute, nil) // 07:15-07:20
	last := f.listen("continue", 1200, 3000, 25*time.Minute, nil)  // 07:20-07:35 at 2x
	last.StartedAt = f.clock.Add(-40 * time.Minute).Format(time.RFC3339Nano)
	last.PlaybackSpeed, last.Duration = 2, syncBookLen+5
	f.syncOne(t, "phone", first)
	f.syncOne(t, "phone", last)
	f.syncOne(t, "phone", middle)
	s, _, err := f.c.ListSessions(f.ctx, SessionFilter{})
	if err != nil || len(s) != 1 {
		t.Fatalf("sessions = %+v, %v", s, err)
	}
	if got := s[0]; got.Speed != 2 || got.Duration != syncBookLen+5 || got.EndPos != 3000 || got.Listened != 2100 {
		t.Fatalf("session after a late span = %+v, want the last span's speed, length and place, 2100 s listened", got)
	}
}
