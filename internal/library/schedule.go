package library

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Scheduled scans. A library's schedule is one of:
//
//	""             no scheduled scans (the default)
//	"every:<N>h"   N hours after the last scan started (N in scheduleIntervals)
//	"daily:HH:MM"  every day at that time, in the server's time zone
//
// "The last scan" is the library's newest scan_runs row whatever started it, so a
// manual rescan pushes an interval schedule back, and a server that was off when a
// daily scan was due runs it as soon as it is back.

// scheduleIntervals are the intervals an "every:" schedule may use (hours).
var scheduleIntervals = map[int]bool{1: true, 3: true, 6: true, 12: true, 24: true}

// ErrInvalidSchedule marks a schedule string that isn't one of the forms above.
var ErrInvalidSchedule = errors.New("invalid scan schedule")

// Schedule is a parsed scan schedule; the zero value is "none".
type Schedule struct {
	every      time.Duration // interval schedules
	hour, mins int           // daily schedules (daily is set)
	daily      bool
}

// ParseSchedule parses a stored schedule string.
func ParseSchedule(s string) (Schedule, error) {
	bad := fmt.Errorf("%w: %q", ErrInvalidSchedule, s)
	kind, arg, ok := strings.Cut(strings.TrimSpace(s), ":")
	switch {
	case s == "":
		return Schedule{}, nil
	case !ok:
		return Schedule{}, bad
	case kind == "every":
		h, err := strconv.Atoi(strings.TrimSuffix(arg, "h"))
		if err != nil || !strings.HasSuffix(arg, "h") || !scheduleIntervals[h] {
			return Schedule{}, bad
		}
		return Schedule{every: time.Duration(h) * time.Hour}, nil
	case kind == "daily":
		hh, mm, ok := strings.Cut(arg, ":")
		h, herr := strconv.Atoi(hh)
		m, merr := strconv.Atoi(mm)
		if !ok || len(hh) != 2 || len(mm) != 2 || herr != nil || merr != nil || h > 23 || m > 59 || h < 0 || m < 0 {
			return Schedule{}, bad
		}
		return Schedule{daily: true, hour: h, mins: m}, nil
	}
	return Schedule{}, bad
}

// Off reports whether the schedule runs nothing.
func (s Schedule) Off() bool { return s.every == 0 && !s.daily }

// Next returns when the next scheduled scan is due, given when the library's last
// scan started (zero if it never ran: then from now). The zero time when Off.
func (s Schedule) Next(last, now time.Time) time.Time {
	if s.Off() {
		return time.Time{}
	}
	from := last
	if from.IsZero() {
		from = now
	}
	if s.every > 0 {
		return from.Add(s.every)
	}
	from = from.In(now.Location())
	at := time.Date(from.Year(), from.Month(), from.Day(), s.hour, s.mins, 0, 0, now.Location())
	if !at.After(from) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}
