package catalog

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func base(v int64) *int64 { return &v }

// ResolvePlace is Sync v2's conflict rule: a save diverges only when a row is
// stored, a base is sent that isn't the stored version, and another device wrote
// the row; a diverged save keeps the further place, any other is
// last-write-wins.
func TestResolvePlace(t *testing.T) {
	t.Parallel()
	const earlier, later = "2026-10-25T10:00:00Z", "2026-10-25T11:00:00Z"
	stored := &Place{Progression: 0.5, DeviceID: "phone", UpdatedAt: earlier, Version: 3}
	for _, tc := range []struct {
		name   string
		stored *Place
		in     Place
		base   *int64
		want   PlaceOutcome
	}{
		{"nothing stored", nil, Place{Progression: 0.1, DeviceID: "web", UpdatedAt: earlier, Version: 1}, base(7), PlaceApply},
		// Not diverged: last-write-wins.
		{"saw the stored row, later", stored, Place{Progression: 0.2, DeviceID: "web", UpdatedAt: later, Version: 4}, base(3), PlaceApply},
		{"saw the stored row, older", stored, Place{Progression: 0.9, DeviceID: "web", UpdatedAt: "2026-10-25T09:00:00Z", Version: 4}, base(3), PlaceStale},
		{"same device, stale base", stored, Place{Progression: 0.1, DeviceID: "phone", UpdatedAt: later, Version: 4}, base(1), PlaceApply},
		{"same device, stale base, older", stored, Place{Progression: 0.9, DeviceID: "phone", UpdatedAt: "2026-10-25T09:00:00Z", Version: 4}, base(1), PlaceStale},
		{"no base sent, back", stored, Place{Progression: 0.1, DeviceID: "web", UpdatedAt: later, Version: 4}, nil, PlaceApply},
		{"same time, the version breaks the tie", stored, Place{Progression: 0.1, DeviceID: "web", UpdatedAt: earlier, Version: 4}, base(3), PlaceApply},
		// Diverged: the further place, whatever the times say.
		{"diverged, further", stored, Place{Progression: 0.7, DeviceID: "web", UpdatedAt: "2026-10-25T09:00:00Z", Version: 4}, base(2), PlaceApplyDiverged},
		{"diverged, behind", stored, Place{Progression: 0.3, DeviceID: "web", UpdatedAt: later, Version: 4}, base(2), PlaceKeepStored},
		{"diverged, level", stored, Place{Progression: 0.5, DeviceID: "web", UpdatedAt: later, Version: 4}, base(2), PlaceKeepStored},
		{"never saw a row, behind", stored, Place{Progression: 0.3, DeviceID: "web", UpdatedAt: later, Version: 4}, base(0), PlaceKeepStored},
		{"never saw a row, further", stored, Place{Progression: 0.6, DeviceID: "web", UpdatedAt: later, Version: 4}, base(0), PlaceApplyDiverged},
	} {
		if got := ResolvePlace(tc.stored, tc.in, tc.base); got != tc.want {
			t.Errorf("%s: ResolvePlace = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// readingFixture is the user-state fixture with reading-progress helpers.
type readingFixture struct{ *userStateFixture }

func newReadingFixture(t *testing.T) *readingFixture {
	t.Helper()
	return &readingFixture{newUserStateFixture(t, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))}
}

func (f *readingFixture) save(t *testing.T, user int64, in ReadingSave) (*ReadingProgress, *OtherPlace) {
	t.Helper()
	if in.Ref == (Ref{}) {
		in.Ref = f.ref("A/Book")
	}
	if err := CheckReadingSave(in); err != nil {
		t.Fatalf("CheckReadingSave(%+v): %v", in, err)
	}
	kept, other, err := f.c.SaveReadingProgress(t.Context(), user, in)
	if err != nil {
		t.Fatal(err)
	}
	return kept, other
}

func TestSaveReadingProgress(t *testing.T) {
	t.Parallel()
	f := newReadingFixture(t)
	ctx := t.Context()
	if p, err := f.c.GetReadingProgress(ctx, f.ann, f.ref("A/Book")); err != nil || p != nil {
		t.Fatalf("nothing saved yet = %+v, %v", p, err)
	}
	// The first save is version 1; its time is stored as fixed-width UTC.
	kept, other := f.save(t, f.ann, ReadingSave{CFI: "epubcfi(/6/4!/4/2/1:0)", Progression: 0.2, DeviceID: "phone",
		DeviceName: "Ann's phone", UpdatedAt: "2026-10-25T11:00:00+01:00", BaseVersion: base(0)})
	if other != nil || kept.Version != 1 || kept.UpdatedAt != "2026-10-25T10:00:00.000Z" || kept.DeviceName != "Ann's phone" {
		t.Fatalf("first save = %+v, other %+v", kept, other)
	}
	// The same device reads on: applied, version 2.
	kept, other = f.save(t, f.ann, ReadingSave{CFI: "epubcfi(/6/8!/4/2/1:0)", Progression: 0.4, DeviceID: "phone",
		DeviceName: "Ann's phone", UpdatedAt: "2026-10-25T10:05:00Z", BaseVersion: base(1)})
	if other != nil || kept.Version != 2 || kept.Progression != 0.4 {
		t.Fatalf("same device = %+v, other %+v", kept, other)
	}
	// A stale save that saw the row: the stored row echoed, nothing offered.
	kept, other = f.save(t, f.ann, ReadingSave{CFI: "epubcfi(/6/2!/4/2/1:0)", Progression: 0.9, DeviceID: "web",
		UpdatedAt: "2026-10-25T10:01:00Z", BaseVersion: base(2)})
	if other != nil || kept.Version != 2 || kept.Progression != 0.4 {
		t.Fatalf("stale save = %+v, other %+v", kept, other)
	}
	// Another device that never saw version 2 and is behind: the further place
	// stays, the save is offered back.
	kept, other = f.save(t, f.ann, ReadingSave{CFI: "epubcfi(/6/6!/4/2/1:0)", Progression: 0.3, DeviceID: "web",
		DeviceName: "Firefox", UpdatedAt: "2026-10-25T10:10:00Z", BaseVersion: base(1)})
	if kept.Version != 2 || kept.Progression != 0.4 || kept.DeviceID != "phone" ||
		other == nil || other.Progression != 0.3 || other.DeviceID != "web" || other.DeviceName != "Firefox" ||
		other.CFI != "epubcfi(/6/6!/4/2/1:0)" {
		t.Fatalf("diverged behind = %+v, other %+v", kept, other)
	}
	// ... and one that is further: applied, the stored place offered back.
	kept, other = f.save(t, f.ann, ReadingSave{CFI: "epubcfi(/6/12!/4/2/1:0)", Progression: 0.6, DeviceID: "web",
		DeviceName: "Firefox", UpdatedAt: "2026-10-25T10:11:00Z", BaseVersion: base(1)})
	if kept.Version != 3 || kept.Progression != 0.6 || kept.DeviceID != "web" ||
		other == nil || other.Progression != 0.4 || other.DeviceID != "phone" || other.DeviceName != "Ann's phone" {
		t.Fatalf("diverged further = %+v, other %+v", kept, other)
	}
	// Stay on purpose: the next save carries the returned version, so it moves back.
	kept, other = f.save(t, f.ann, ReadingSave{CFI: "epubcfi(/6/4!/4/2/1:0)", Progression: 0.1, DeviceID: "phone",
		UpdatedAt: "2026-10-25T10:12:00Z", BaseVersion: base(3)})
	if other != nil || kept.Version != 4 || kept.Progression != 0.1 {
		t.Fatalf("deliberate move back = %+v, other %+v", kept, other)
	}
	if got, _ := f.c.GetReadingProgress(ctx, f.ann, f.ref("A/Book")); got == nil || *got != *kept {
		t.Fatalf("stored = %+v, want %+v", got, kept)
	}
	// A far-future time is the server's.
	kept, _ = f.save(t, f.ann, ReadingSave{CFI: "epubcfi(/6/4!/4/2/1:0)", Progression: 0.15, DeviceID: "phone",
		UpdatedAt: "2099-01-01T00:00:00Z", BaseVersion: base(4)})
	if kept.UpdatedAt != "2026-10-25T12:00:00.000Z" {
		t.Fatalf("far-future updated_at stored as %q", kept.UpdatedAt)
	}
	// Another user's place is theirs alone.
	if p, _ := f.c.GetReadingProgress(ctx, f.bob, f.ref("A/Book")); p != nil {
		t.Fatalf("bob sees ann's place: %+v", p)
	}
}

func TestCheckReadingSave(t *testing.T) {
	t.Parallel()
	ok := ReadingSave{CFI: "epubcfi(/6/4!/4/2/1:0)", Progression: 0.5, DeviceID: "d", UpdatedAt: "2026-10-25T10:00:00Z"}
	if err := CheckReadingSave(ok); err != nil {
		t.Fatalf("a good save: %v", err)
	}
	for name, mod := range map[string]func(*ReadingSave){
		"no cfi":           func(s *ReadingSave) { s.CFI = "" },
		"not a cfi":        func(s *ReadingSave) { s.CFI = "/6/4!/4" },
		"cfi too long":     func(s *ReadingSave) { s.CFI = "epubcfi(" + strings.Repeat("1", maxCFIBytes) + ")" },
		"below 0":          func(s *ReadingSave) { s.Progression = -0.01 },
		"above 1":          func(s *ReadingSave) { s.Progression = 1.01 },
		"bad time":         func(s *ReadingSave) { s.UpdatedAt = "yesterday" },
		"no time":          func(s *ReadingSave) { s.UpdatedAt = "" },
		"device too long":  func(s *ReadingSave) { s.DeviceID = strings.Repeat("d", maxDeviceIDBytes+1) },
		"negative version": func(s *ReadingSave) { s.BaseVersion = base(-1) },
	} {
		s := ok
		mod(&s)
		if err := CheckReadingSave(s); !errors.Is(err, ErrInvalidReadingProgress) {
			t.Errorf("%s: err = %v, want ErrInvalidReadingProgress", name, err)
		}
	}
	for _, edge := range []float64{0, 1} {
		s := ok
		s.Progression = edge
		if err := CheckReadingSave(s); err != nil {
			t.Errorf("progression %v: %v", edge, err)
		}
	}
}

// A move carries every reader's place to the book's new path; a place already
// there is replaced only by a newer one, under a version above both.
func TestReadingProgressMoves(t *testing.T) {
	t.Parallel()
	f := newReadingFixture(t)
	ctx := t.Context()
	f.save(t, f.ann, ReadingSave{Ref: f.ref("old/Book"), CFI: "epubcfi(/6/4!)", Progression: 0.4, DeviceID: "phone",
		UpdatedAt: "2026-10-25T10:00:00Z"})
	f.save(t, f.bob, ReadingSave{Ref: f.ref("old/Book"), CFI: "epubcfi(/6/8!)", Progression: 0.7, DeviceID: "tab",
		UpdatedAt: "2026-10-25T09:00:00Z"})
	f.save(t, f.bob, ReadingSave{Ref: f.ref("new/Book"), CFI: "epubcfi(/6/2!)", Progression: 0.1, DeviceID: "tab",
		UpdatedAt: "2026-10-25T09:30:00Z"})
	if err := f.c.MoveDurableState(ctx, f.lib, "old/Book", "new/Book"); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.c.GetReadingProgress(ctx, f.ann, f.ref("old/Book")); p != nil {
		t.Fatalf("the old path still holds ann's place: %+v", p)
	}
	if p, _ := f.c.GetReadingProgress(ctx, f.ann, f.ref("new/Book")); p == nil || p.Progression != 0.4 || p.Version != 1 {
		t.Fatalf("ann's place moved = %+v", p)
	}
	// Bob's place at the new path is newer than the carried one: it stays.
	if p, _ := f.c.GetReadingProgress(ctx, f.bob, f.ref("new/Book")); p == nil || p.Progression != 0.1 || p.Version != 2 {
		t.Fatalf("bob's place after the move = %+v, want the newer 0.1 at version 2", p)
	}
	// A carried place newer than the one there replaces it, whole.
	f.save(t, f.bob, ReadingSave{Ref: f.ref("old/Two"), CFI: "epubcfi(/6/10!)", Progression: 0.8, DeviceID: "web",
		DeviceName: "Firefox", UpdatedAt: "2026-10-25T11:00:00Z"})
	f.save(t, f.bob, ReadingSave{Ref: f.ref("new/Two"), CFI: "epubcfi(/6/2!)", Progression: 0.2, DeviceID: "tab",
		DeviceName: "Tablet", UpdatedAt: "2026-10-25T10:00:00Z"})
	if err := f.c.MoveDurableState(ctx, f.lib, "old/Two", "new/Two"); err != nil {
		t.Fatal(err)
	}
	want := ReadingProgress{Ref: f.ref("new/Two"), CFI: "epubcfi(/6/10!)", Progression: 0.8, Version: 2,
		DeviceID: "web", DeviceName: "Firefox", UpdatedAt: "2026-10-25T11:00:00.000Z"}
	if p, _ := f.c.GetReadingProgress(ctx, f.bob, f.ref("new/Two")); p == nil || *p != want {
		t.Fatalf("bob's newer carried place = %+v, want %+v", p, want)
	}
}
