package backup

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A backup schedule is one of:
//
//	""                  no scheduled backups
//	"daily:HH:MM"       every day at that time, in the server's time zone
//	"weekly:DAY:HH:MM"  once a week (DAY is mon, tue, wed, thu, fri, sat or sun)
//
// counted from the newest scheduled backup, so a server that was off when one was
// due makes it as soon as it is back.

// ErrInvalidSchedule marks a schedule that isn't one of the forms above.
var ErrInvalidSchedule = errors.New(`must be "", "daily:HH:MM" or "weekly:DAY:HH:MM"`)

// Schedule is a parsed backup schedule; the zero value is "off".
type Schedule struct {
	on         bool
	weekly     bool
	day        time.Weekday
	hour, mins int
}

var days = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// ParseSchedule parses a schedule string (see above).
func ParseSchedule(s string) (Schedule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Schedule{}, nil
	}
	parts := strings.Split(s, ":")
	var sch Schedule
	switch {
	case len(parts) == 3 && parts[0] == "daily":
		parts = parts[1:]
	case len(parts) == 4 && parts[0] == "weekly":
		d := slices.Index(days, parts[1])
		if d < 0 {
			return Schedule{}, ErrInvalidSchedule
		}
		sch.weekly, sch.day = true, time.Weekday(d)
		parts = parts[2:]
	default:
		return Schedule{}, ErrInvalidSchedule
	}
	h, herr := strconv.Atoi(parts[0])
	m, merr := strconv.Atoi(parts[1])
	if len(parts[0]) != 2 || len(parts[1]) != 2 || herr != nil || merr != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return Schedule{}, ErrInvalidSchedule
	}
	sch.on, sch.hour, sch.mins = true, h, m
	return sch, nil
}

// String is the canonical form ("" when off).
func (s Schedule) String() string {
	switch {
	case !s.on:
		return ""
	case s.weekly:
		return fmt.Sprintf("weekly:%s:%02d:%02d", days[s.day], s.hour, s.mins)
	}
	return fmt.Sprintf("daily:%02d:%02d", s.hour, s.mins)
}

// Off reports whether nothing is scheduled.
func (s Schedule) Off() bool { return !s.on }

// Next is when the first backup after from is due, in loc. The zero time when off.
func (s Schedule) Next(from time.Time, loc *time.Location) time.Time {
	if !s.on {
		return time.Time{}
	}
	from = from.In(loc)
	at := time.Date(from.Year(), from.Month(), from.Day(), s.hour, s.mins, 0, 0, loc)
	if s.weekly {
		at = at.AddDate(0, 0, (int(s.day)-int(at.Weekday())+7)%7)
	}
	for !at.After(from) {
		if s.weekly {
			at = at.AddDate(0, 0, 7)
		} else {
			at = at.AddDate(0, 0, 1)
		}
	}
	return at
}
