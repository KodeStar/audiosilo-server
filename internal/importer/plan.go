package importer

import (
	"cmp"
	"math"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Planning turns a payload and its matches into the rows an apply writes
// (catalog.ImportWrite) and what the review reports, against the person's state
// here (catalog.ImportState). It is pure: the review and the apply run the same
// code, the apply inside its transaction.

const (
	// maxUnmatched bounds the unmatched list a review shows (most listened first).
	maxUnmatched = 500
	// maxSessionListen caps one session's listening: ABS has sessions left open
	// for days, and no sitting is longer than a day.
	maxSessionListen = 24 * 60 * 60
	// clampFactor: a session whose span is more than this many times its
	// listening was left open (overnight, a paused app); its end is moved to its
	// start plus its listening, so the hours of the day aren't smeared over it.
	clampFactor = 3
	// minEstimate is the smallest estimated listening kept (migration 0021's 5 minutes).
	minEstimate = 300
	// bookmarkSlack: an imported bookmark within this many seconds of one the
	// person already has on the book, with the same text, is the same bookmark.
	bookmarkSlack = 2
)

func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// dateStamp is a start or finish date as progress stores it (RFC3339 UTC to the
// second, SaveProgress's form); "" for an unknown time.
func dateStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// itemStats is one item's ABS listening.
type itemStats struct {
	listened float64 // seconds, over every session
	sessions int
	advanced float64 // book time those sessions moved through
	first    time.Time
}

// add counts session s into is (nil: a new one), returning it.
func (is *itemStats) add(s Session) *itemStats {
	if is == nil {
		is = &itemStats{}
	}
	is.listened += math.Min(s.Listened, maxSessionListen)
	is.sessions++
	is.advanced += math.Max(s.EndPos-s.StartPos, 0)
	if start := msTime(s.Started); is.first.IsZero() || start.Before(is.first) {
		is.first = start
	}
	return is
}

// plan works out what importing p (matched by matches) into st writes. loc is the
// server's zone (an estimate's day); now stands in for a time ABS didn't give.
func plan(p *Payload, matches map[string]Match, st catalog.ImportState, loc *time.Location, now time.Time) *catalog.ImportWrite {
	w := &catalog.ImportWrite{}
	sum := &w.Plan.Summary
	cutoff := st.Import.CutoffTime()
	before := func(t time.Time) bool { return cutoff.IsZero() || t.Before(cutoff) }

	// stats is each item's listening (the unmatched list); byRef each matched
	// book's, over every item matched to it (a gone item's sessions merge with
	// the present one's), so the estimate subtracts all the sessions imported
	// onto the book.
	stats := map[string]*itemStats{}
	byRef := map[catalog.Ref]*itemStats{}
	for _, s := range p.Sessions {
		if s.Listened <= 0 {
			continue
		}
		stats[s.ItemID] = stats[s.ItemID].add(s)
		if m := matches[s.ItemID]; m.Tier != "" {
			byRef[m.Ref] = byRef[m.Ref].add(s)
		}
	}

	sum.Items = len(p.Items)
	var unmatched []catalog.UnmatchedItem
	for _, it := range p.Items {
		m := matches[it.ID]
		switch m.Tier {
		case TierPath:
			sum.Matched.Path++
		case TierASIN:
			sum.Matched.ASIN++
		case TierISBN:
			sum.Matched.ISBN++
		case TierTitle:
			sum.Matched.Title++
		default:
			u := catalog.UnmatchedItem{Title: it.Title, Author: it.Author, Reason: m.Reason}
			if is := stats[it.ID]; is != nil {
				u.Listened, u.Sessions = is.listened, is.sessions
			}
			unmatched = append(unmatched, u)
		}
	}
	sum.Unmatched = len(unmatched)
	slices.SortStableFunc(unmatched, func(a, b catalog.UnmatchedItem) int {
		return cmp.Or(cmp.Compare(b.Listened, a.Listened), cmp.Compare(a.Title, b.Title))
	})
	w.Plan.Unmatched = unmatched[:min(len(unmatched), maxUnmatched)]

	planSessions(w, p, matches, before)
	planProgress(w, p, matches, st, byRef, before, loc, now)
	planBookmarks(w, p, matches, st, now)
	return w
}

// planSessions adds the sessions on matched books that started before the
// cutoff, oldest first.
func planSessions(w *catalog.ImportWrite, p *Payload, matches map[string]Match, before func(time.Time) bool) {
	sum := &w.Plan.Summary
	for _, s := range p.Sessions {
		m := matches[s.ItemID]
		if m.Tier == "" || s.Listened <= 0 || s.Started <= 0 {
			continue
		}
		start := msTime(s.Started)
		if !before(start) {
			sum.SkippedAfterCutoff++
			continue
		}
		listened := math.Min(s.Listened, maxSessionListen)
		span := time.Duration(listened * float64(time.Second))
		last := msTime(s.Updated)
		if gap := last.Sub(start); gap < span || gap > clampFactor*span {
			last = start.Add(span)
		}
		w.Sessions = append(w.Sessions, catalog.ImportSession{Ref: m.Ref, DeviceName: s.Device,
			ClientVersion: s.ClientVersion, Platform: s.Platform, StartedAt: start, LastAt: last,
			StartPos: s.StartPos, EndPos: s.EndPos, Duration: s.Duration, Listened: listened})
		sum.Sessions++
		sum.Listened += listened
	}
	slices.SortStableFunc(w.Sessions, func(a, b catalog.ImportSession) int { return a.StartedAt.Compare(b.StartedAt) })
	if n := len(w.Sessions); n > 0 {
		first, last := dateStamp(w.Sessions[0].StartedAt), dateStamp(w.Sessions[n-1].StartedAt)
		sum.FirstListen, sum.LastListen = &first, &last
	}
}

// planProgress merges ABS's progress on each matched book (mergeProgress) and
// estimates the listening its sessions don't cover (byRef: every session on the
// book, a gone item's included).
func planProgress(w *catalog.ImportWrite, p *Payload, matches map[string]Match, st catalog.ImportState,
	byRef map[catalog.Ref]*itemStats, before func(time.Time) bool, loc *time.Location, now time.Time) {
	sum := &w.Plan.Summary
	for _, pr := range p.Progress {
		m := matches[pr.ItemID]
		if m.Tier == "" {
			continue
		}
		var have *catalog.Progress
		if cur, ok := st.Progress[m.Ref]; ok {
			have = &cur
		}
		if wrote, r := mergeProgress(have, pr, m, now); r.changed {
			w.Progress = append(w.Progress, catalog.ImportProgress{Prior: have, Wrote: wrote})
			if r.moved {
				sum.Progress++
			}
			if r.finished {
				sum.Finished++
			}
		}
		// Migration 0021's estimate: the position reached, at the speed the
		// sessions show (book time moved through per second listened; 1 without
		// them), less every session ABS has for the book (those after the cutoff
		// too: that listening is recorded, just not imported). Dated by when ABS
		// says the book was started, kept only when that is before the cutoff.
		// None for a book finished in ABS without one session that recorded
		// listening there (before or after the cutoff): ABS puts a finished
		// book's position at its end, so a book only marked finished (read
		// elsewhere, or heard before ABS) would count as the whole book listened
		// on one day. A finished book with sessions keeps the estimate of what
		// they don't cover.
		is := byRef[m.Ref]
		if is == nil {
			if pr.Done {
				continue
			}
			is = &itemStats{}
		}
		speed := 1.0
		if is.advanced > 0 && is.listened > 0 {
			speed = min(max(is.advanced/is.listened, 0.5), 4)
		}
		started := msTime(pr.Started)
		if started.IsZero() {
			started = is.first
		}
		if est := pr.Position/speed - is.listened; est >= minEstimate && !started.IsZero() && before(started) {
			w.Days = append(w.Days, catalog.ImportDay{Ref: m.Ref, Day: started.In(loc).Format(time.DateOnly),
				Listened: est})
			sum.Estimated += est
		}
	}
}

// mergeResult says what mergeProgress did: changed the row at all, moved it on
// (created it, or advanced its position or finish), took its finish date from ABS.
type mergeResult struct{ changed, moved, finished bool }

// mergeProgress is ABS's progress on a book (a) merged into the person's (have,
// nil when they have none), fill-only:
//
//   - With no row here, ABS's becomes it (unless ABS has nothing: an item opened
//     and never played). Its updated_at is ABS's last update, not now: no device
//     holds the row yet, so last-write-wins has nothing to settle, and the time
//     keeps the player's "continue listening" in the order the person listened.
//   - With an unfinished row, its position and finish move on only when ABS's
//     update wins under SaveProgress's last-write-wins rule (catalog.IsNewer) -
//     its last update later than the row's updated_at; ABS has no version, so
//     the same time keeps the row: listening here since is the person's newer
//     word - and never back: a position is never rewound and a finished book
//     never un-finished. The moved row takes ABS's last update as its
//     updated_at, later than the row's, so it wins against what a device holds
//     and players take it on their next sync, without jumping to the top of
//     "continue listening" as a stamp of now would.
//   - started_at becomes the earlier of the two, and a finished row with no
//     finish date takes ABS's. Those alone leave updated_at as it is.
//
// A changed row gets a version above the one it replaces.
func mergeProgress(have *catalog.Progress, a Progress, m Match, now time.Time) (catalog.Progress, mergeResult) {
	// A time ABS didn't give, or one ahead of this server's clock (the ABS host's
	// clock runs fast): now. A future updated_at would win last-write-wins over
	// every player save until the clock caught up (SaveProgress refuses one).
	updated := msTime(a.Updated)
	if updated.IsZero() || updated.After(now) {
		updated = now
	}
	started := dateStamp(msTime(a.Started))
	finishedAt := ""
	if a.Done {
		finishedAt = dateStamp(cmp.Or(msTime(a.FinishedAt), updated))
	}
	if have == nil {
		if !a.Done && a.Position <= 0 {
			return catalog.Progress{}, mergeResult{}
		}
		w := catalog.Progress{Ref: m.Ref, Position: a.Position, Duration: cmp.Or(m.Duration, a.Duration),
			Finished: a.Done, PlaybackSpeed: 1, Version: 1, UpdatedAt: catalog.FormatStamp(updated),
			StartedAt: cmp.Or(started, dateStamp(updated)), FinishedAt: finishedAt}
		if w.Finished && w.Position <= 0 {
			w.Position = w.Duration // a finished book sits at its end
		}
		return w, mergeResult{changed: true, moved: true, finished: a.Done}
	}
	w := *have
	var r mergeResult
	abs := catalog.Progress{UpdatedAt: catalog.FormatStamp(updated), Version: have.Version}
	if !have.Finished && catalog.IsNewer(abs, *have) {
		if a.Position > w.Position {
			w.Position, r.moved = a.Position, true
		}
		if a.Done {
			w.Finished, w.FinishedAt, r.moved, r.finished = true, finishedAt, true, true
		}
	}
	// Both are RFC3339 UTC to the second (dateStamp; SaveProgress and the edits
	// store that fixed-width form), so they compare as strings.
	if started != "" && (w.StartedAt == "" || started < w.StartedAt) {
		w.StartedAt = started
	}
	if w.Finished && w.FinishedAt == "" && finishedAt != "" {
		w.FinishedAt, r.finished = finishedAt, true
	}
	if w == *have {
		return w, mergeResult{}
	}
	r.changed = true
	w.Version = have.Version + 1
	if r.moved {
		w.UpdatedAt = abs.UpdatedAt
	}
	return w, r
}

// planBookmarks adds the bookmarks on matched books the person doesn't already
// have (same book, within bookmarkSlack seconds, same text).
func planBookmarks(w *catalog.ImportWrite, p *Payload, matches map[string]Match, st catalog.ImportState, now time.Time) {
	have := map[catalog.Ref][]catalog.Bookmark{}
	for ref, bs := range st.Bookmarks {
		have[ref] = slices.Clone(bs)
	}
	for _, b := range p.Bookmarks {
		m := matches[b.ItemID]
		if m.Tier == "" || b.Position < 0 || math.IsNaN(b.Position) || math.IsInf(b.Position, 0) {
			continue
		}
		note := b.Title
		if utf8.RuneCountInString(note) > catalog.MaxBookmarkNote {
			note = string([]rune(note)[:catalog.MaxBookmarkNote])
		}
		if slices.ContainsFunc(have[m.Ref], func(x catalog.Bookmark) bool {
			return x.Note == note && math.Abs(x.Position-b.Position) <= bookmarkSlack
		}) {
			continue
		}
		created := msTime(b.Created)
		if created.IsZero() {
			created = now
		}
		bm := catalog.Bookmark{Ref: m.Ref, Position: b.Position, Note: note, CreatedAt: catalog.FormatStamp(created)}
		have[m.Ref] = append(have[m.Ref], bm)
		w.Bookmarks = append(w.Bookmarks, bm)
	}
	w.Plan.Summary.Bookmarks = len(w.Bookmarks)
}
