package library

import (
	"testing"
	"time"
)

// A birth time of 0 (macOS over an rclone NFS share answers it: added_at read
// 1970-01-01) or any before 1980 is none, the mtime standing in; one from
// 1980 on is the file's.
func TestBirthKnown(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		at   time.Time
		want bool
	}{
		{time.Unix(0, 0), false},
		{time.Date(1979, 12, 31, 23, 59, 59, 0, time.UTC), false},
		{time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), true},
	} {
		if got := birthKnown(c.at.Unix()); got != c.want {
			t.Errorf("birthKnown(%v) = %v, want %v", c.at, got, c.want)
		}
	}
}
