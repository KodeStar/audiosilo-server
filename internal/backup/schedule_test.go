package backup

import (
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":                 "",
		" daily:03:00 ":    "daily:03:00",
		"weekly:sun:23:59": "weekly:sun:23:59",
		"weekly:mon:00:00": "weekly:mon:00:00",
	} {
		s, err := ParseSchedule(in)
		if err != nil || s.String() != want {
			t.Errorf("ParseSchedule(%q) = %q, %v; want %q", in, s.String(), err, want)
		}
	}
	for _, bad := range []string{"daily", "daily:3:00", "daily:24:00", "daily:03:60", "weekly:03:00",
		"weekly:sunday:03:00", "weekly:Sun:03:00", "every:6h", "daily:03:00:00", "daily:-1:00"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("ParseSchedule(%q) accepted", bad)
		}
	}
}

func TestScheduleNext(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	daily, _ := ParseSchedule("daily:03:00")
	weekly, _ := ParseSchedule("weekly:sun:03:00") // 2026-10-04 is a Sunday
	off, _ := ParseSchedule("")
	cases := []struct {
		s          Schedule
		from, want string
	}{
		{daily, "2026-10-04 02:00", "2026-10-04 03:00"},
		{daily, "2026-10-04 03:00", "2026-10-05 03:00"},
		{daily, "2026-10-04 09:30", "2026-10-05 03:00"},
		{weekly, "2026-10-04 02:59", "2026-10-04 03:00"},
		{weekly, "2026-10-04 03:00", "2026-10-11 03:00"},
		{weekly, "2026-10-06 12:00", "2026-10-11 03:00"},
	}
	for _, c := range cases {
		if got := c.s.Next(at(c.from), loc); !got.Equal(at(c.want)) {
			t.Errorf("%s from %s = %s, want %s", c.s, c.from, got, c.want)
		}
	}
	if !off.Next(at("2026-10-04 02:00"), loc).IsZero() {
		t.Error("off schedule has a next time")
	}
}
