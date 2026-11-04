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
// returns w itself when there is nothing to translate: no work, no stated scale, a
// chapter count equal to the scale's length, or fewer than two chapters (no list, or
// a lone chapter that says nothing about a place: every position would land on it and
// the whole cast would show from the start). Otherwise it returns a copy without
// PositionScale, whose presence says the numbers beside it are on that scale: these
// are this book's chapters now. w is shared with the cache and never modified.
//
// Position 0 stays 0 (before this book), a position at or past the length is the last
// chapter, and any other is placed by chapterPlacer. Generated content is never
// passed through here: it carries no scale and is numbered on the book already.
func OnRecording(w *MetaWork, chapters []metadata.Chapter, duration float64) *MetaWork {
	if w == nil || w.PositionScale == nil || len(chapters) < 2 || len(chapters) == w.PositionScale.Length {
		return w
	}
	length, last := w.PositionScale.Length, len(chapters)
	place := chapterPlacer(chapters, duration, length)
	translate := func(pos MetaPosition) MetaPosition {
		switch {
		case pos.Chapter <= 0:
		case pos.Chapter >= length:
			pos.Chapter = last
		default:
			pos.Chapter = min(place(pos.Chapter), last)
		}
		return pos
	}
	c := *w
	c.PositionScale = nil
	if w.Characters != nil {
		c.Characters = make([]MetaCharacter, len(w.Characters))
		for i, ch := range w.Characters {
			ch.Reveal = translate(ch.Reveal)
			c.Characters[i] = ch
		}
	}
	if w.Recaps != nil {
		c.Recaps = make([]MetaRecap, len(w.Recaps))
		for i, r := range w.Recaps {
			r.Through = translate(r.Through)
			c.Recaps[i] = r
		}
	}
	return &c
}

// chapterPlacer returns the 1-based number of the first chapter at or past fraction
// p/length of the book, for a p of 1 to length-1; past the last chapter when none is,
// which the caller clamps. A chapter is at or past the fraction when it STARTS there,
// so nothing is shown before the listener has reached it: a character gated on
// reaching the number is never early, and a recap gated on being past it is at worst
// one chapter late, the safe side. Positions never decrease with p, though two can
// land on one chapter.
//
// By chapter start times where the book's are usable, because tracks vary and a
// fraction of the book is a place in time: a long opening track holds more of the book
// than a count would give it. Without them (chapterTimeline) by count, each chapter
// taken as an equal share of the book.
func chapterPlacer(chapters []metadata.Chapter, duration float64, length int) func(p int) int {
	if starts, total, ok := chapterTimeline(chapters, duration); ok {
		return func(p int) int {
			return sort.SearchFloat64s(starts, float64(p)*total/float64(length)) + 1
		}
	}
	n := len(chapters)
	return func(p int) int {
		// Chapter k of n starts at (k-1)/n: the smallest k with (k-1)/n >= p/length,
		// in integers so no rounding can bring it forward.
		return (p*n+length-1)/length + 1
	}
}

// chapterTimeline is each chapter's whole-book start and the book's length in
// seconds, ok only when they can be trusted: starts strictly ascending (an on-demand
// index can give every chapter book_offset 0) and a total past the last start, from
// duration, else the last chapter's own end.
func chapterTimeline(chapters []metadata.Chapter, duration float64) (starts []float64, total float64, ok bool) {
	starts = make([]float64, len(chapters))
	for i, ch := range chapters {
		if i > 0 && ch.BookOffset <= starts[i-1] {
			return nil, 0, false
		}
		starts[i] = ch.BookOffset
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
