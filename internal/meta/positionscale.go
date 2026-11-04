package meta

import (
	"sort"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// A community sidecar's positions (a character's reveal, a recap's through) are
// numbered on the WORK's own scale, which the work states as position_scale: five
// acts, 44 chapters, nine sections. A listener's place is a chapter of THIS book's
// recording, and every gate compares the two as plain numbers (HideSpoilers, the
// player's meta-gating.ts, including the shipped apps'). So where a recording splits
// finer than the scale the reveals come early: at track 6 of a 43-track Hamlet whose
// sidecar is on 5 acts, the Act 5 recap's "through 5" is already behind the listener.
// Only the server can fix that for a shipped app, so a request for a book answers the
// positions translated onto that book (OnRecording) and the gates stay as they are.
//
// One rule decides everything here: later is safe, early is not. A recording no finer
// than the scale is left alone, since its gates already open late.

// MetaPositionScale is the scale a work's characters and recaps are numbered on
// (MetaWork.PositionScale). Partial says the sidecar stops short of Length on
// purpose; Length is the book's length either way, so a translation reads it the same.
type MetaPositionScale struct {
	Length  int    `json:"length"`
	Unit    string `json:"unit"`
	Partial bool   `json:"partial,omitempty"`
}

// toPositionScale maps upstream's scale to the outward shape, nil for none and for
// a length nothing can be placed on.
func toPositionScale(in *upstreamPositionScale) *MetaPositionScale {
	if in == nil || in.Length < 1 {
		return nil
	}
	return &MetaPositionScale{Length: in.Length, Unit: in.Unit, Partial: in.Partial}
}

// OnRecording is w with its positions translated onto a book with these chapters and
// this duration (Book.Duration, 0 when unknown), for an answer about that book. It
// returns w itself unless the work states a scale and the recording splits finer than
// it (more chapters than the scale's length), which also leaves a book with no chapters
// or a lone one alone. Otherwise it returns a copy without PositionScale, whose presence
// says the numbers beside it are on that scale: these are this book's chapters now. w is
// shared with the cache and never modified.
//
// A reveal and a through mean different things (chapterPlacer). Generated content is
// never passed through here: it carries no scale and is numbered on the book already.
func OnRecording(w *MetaWork, chapters []metadata.Chapter, duration float64) *MetaWork {
	if w == nil || w.PositionScale == nil || len(chapters) <= w.PositionScale.Length {
		return w
	}
	place := newChapterPlacer(chapters, duration, w.PositionScale.Length)
	c := *w
	c.PositionScale = nil
	if w.Characters != nil {
		c.Characters = make([]MetaCharacter, len(w.Characters))
		for i, ch := range w.Characters {
			ch.Reveal.Chapter = place.reveal(ch.Reveal.Chapter)
			c.Characters[i] = ch
		}
	}
	if w.Recaps != nil {
		c.Recaps = make([]MetaRecap, len(w.Recaps))
		for i, r := range w.Recaps {
			r.Through.Chapter = place.through(r.Through.Chapter)
			c.Recaps[i] = r
		}
	}
	return &c
}

// chapterPlacer places a work's positions on a recording finer than its scale. Each
// answer is the LATER of two estimates, by time and by count, so that uneven tracks
// can't bring one forward: time alone can read early when a track runs long (41 tracks
// on 40 chapters, an opening track holding a fifth of the book), and a count alone when
// a track runs short. Without usable times (chapterTimeline) the count stands alone.
type chapterPlacer struct {
	starts []float64 // nil without usable times
	total  float64
	n      int
	length int
}

func newChapterPlacer(chapters []metadata.Chapter, duration float64, length int) chapterPlacer {
	starts, total, ok := chapterTimeline(chapters, duration)
	if !ok {
		starts = nil
	}
	return chapterPlacer{starts: starts, total: total, n: len(chapters), length: length}
}

// reveal places a character's position: the character appears somewhere in unit p, and
// the gate shows it on REACHING the number, so p is the first chapter starting at or
// past the START of that unit, fraction (p-1)/length. Position 1 is chapter 1, so the
// opening cast shows from the start. 0 stays 0 (before this book); past the length,
// which metacheck refuses, the last chapter.
func (c chapterPlacer) reveal(p int) int {
	switch {
	case p <= 0:
		return p
	case p > c.length:
		return c.n
	}
	// Chapter k of n starts at (k-1)/n: the smallest k with (k-1)/n >= (p-1)/length.
	at := ceilDiv((p-1)*c.n, c.length) + 1
	if c.starts != nil {
		at = max(at, sort.SearchFloat64s(c.starts, float64(p-1)*c.total/float64(c.length))+1)
	}
	return min(at, c.n)
}

// through places a recap's position: the recap covers up to the END of unit p, and the
// gate shows it once the listener is PAST the number, so p is the first chapter ending
// at or past fraction p/length, the one holding that end. The gate then opens on
// entering the next chapter, which starts with the unit over. (The last chapter ending
// BEFORE the fraction would open it a chapter too soon, whenever a chapter straddles the
// unit's end.) 0 stays 0; at or past the length, the last chapter, shown once finished.
func (c chapterPlacer) through(p int) int {
	switch {
	case p <= 0:
		return p
	case p >= c.length:
		return c.n
	}
	// Chapter k of n ends at k/n: the smallest k with k/n >= p/length.
	at := ceilDiv(p*c.n, c.length)
	if c.starts != nil {
		// A chapter ends where the next one starts.
		at = max(at, sort.SearchFloat64s(c.starts[1:], float64(p)*c.total/float64(c.length))+1)
	}
	return min(at, c.n)
}

// ceilDiv is a/b rounded up, in integers so no rounding can bring a position forward.
func ceilDiv(a, b int) int {
	return (a + b - 1) / b
}

// chapterTimeline is each chapter's whole-book start and the book's length in seconds,
// ok only when they can be trusted: in order (two may share a start, a zero-length
// marker), not all alike, and a total past the last start, from duration, else the last
// chapter's own end.
func chapterTimeline(chapters []metadata.Chapter, duration float64) (starts []float64, total float64, ok bool) {
	starts = chapterStarts(chapters)
	if !sort.Float64sAreSorted(starts) || startsAllAlike(starts) {
		return nil, 0, false
	}
	lastChapter := chapters[len(chapters)-1]
	total = duration
	if total <= lastChapter.BookOffset {
		total = lastChapter.BookOffset + lastChapter.End - lastChapter.Start
	}
	if total <= lastChapter.BookOffset {
		return nil, 0, false
	}
	return starts, total, true
}

func chapterStarts(chapters []metadata.Chapter) []float64 {
	starts := make([]float64, len(chapters))
	for i, ch := range chapters {
		starts[i] = ch.BookOffset
	}
	return starts
}

// startsAllAlike reports whether chapter starts say nothing about a place: several
// chapters, every one at the same start. An on-demand index can give every chapter
// book_offset 0, which walked as a timeline puts any place in the last chapter. Starts
// merely out of order don't do that: the walk stops at the first one past the position.
func startsAllAlike(starts []float64) bool {
	if len(starts) < 2 {
		return false
	}
	for _, s := range starts[1:] {
		if s != starts[0] {
			return false
		}
	}
	return true
}

// ListeningChapter is the chapter a saved position is in, for spoilers=hide: ChapterAt
// over the book's chapter starts. Where those are all alike the position is read by
// count, each chapter an equal share of duration, rounded DOWN to the chapters it has
// certainly passed, since later is safe; with no duration either, 0: not started, never
// "at the end".
func ListeningChapter(chapters []metadata.Chapter, duration, position float64) int {
	starts := chapterStarts(chapters)
	if !startsAllAlike(starts) {
		return ChapterAt(starts, position)
	}
	if duration <= 0 || position <= 0 {
		return 0
	}
	return min(int(position/duration*float64(len(chapters))), len(chapters))
}
