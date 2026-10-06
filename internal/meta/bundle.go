package meta

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// The /meta "bundle": what GET /libraries/{id}/meta adds to the cached
// envelope per request, so the redesigned player gets its whole community panel
// in one round trip instead of one call per previous book plus its own spoiler
// gating. ?include=previous attaches the works before this one (Previous), and
// ?spoilers=hide gates the current work by the caller's saved progress
// (HideSpoilers). Both work on COPIES: the envelope Enrich returns is shared
// with the cache and every other caller.

// maxPrevious caps how many previous works one envelope carries. Five covers a
// listener catching up on a long series without the bundle growing into the
// whole series' cast lists; older books are a /meta/work call away.
const maxPrevious = 5

// previousWorkIDs returns the work ids before the envelope's work, nearest
// first, at most maxPrevious. They come from each rail's MAIN view only (never
// an alternate ordering: a chronological order's earlier books are the
// reading-order spoiler the main view exists to avoid): every entry whose
// numeric position is below the current work's position on that rail.
// "Nearest" is the numeric distance below the current position, ties kept in
// rail order and then entry order, and an id found on two rails counts once, at
// its nearest. A rail whose own position does not parse as a number contributes
// nothing, nor does an entry whose position does not ("1-3" omnibus, blank).
func previousWorkIDs(env *Enrichment) []string {
	if env == nil {
		return nil
	}
	current := ""
	if env.Work != nil {
		current = env.Work.ID
	}
	type candidate struct {
		id       string
		distance float64
	}
	var cands []candidate
	for _, rail := range env.Series {
		at, ok := parsePosition(rail.Position)
		if !ok {
			continue
		}
		for _, w := range rail.Works {
			p, ok := parsePosition(w.Position)
			if !ok || p >= at || w.ID == "" || w.ID == current {
				continue
			}
			cands = append(cands, candidate{id: w.ID, distance: at - p})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].distance < cands[j].distance })
	var ids []string
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		if seen[c.id] {
			continue
		}
		seen[c.id] = true
		ids = append(ids, c.id)
		if len(ids) == maxPrevious {
			break
		}
	}
	return ids
}

// parsePosition reads a series position as a number ("3", "2.5"); ok is false
// for anything else, including NaN and the infinities ParseFloat accepts.
func parsePosition(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// Previous fetches the works previousWorkIDs names, in that order, each through
// Work - so each is cached (and persisted) on its own and an uncached one waits
// on the same workSem bound as any /meta/work call. They are fetched
// concurrently under the caller's ctx. A work that fails (not found, upstream
// down) is left out: the previous books are an extra, and the envelope they ride
// on must not fail with them. nil when there are none.
//
// The returned works are shared with the cache - treat them as immutable.
func (s *Service) Previous(ctx context.Context, env *Enrichment) []*MetaWork {
	ids := previousWorkIDs(env)
	if len(ids) == 0 {
		return nil
	}
	works := make([]*MetaWork, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() {
			// Each goroutine owns its own index.
			if w, err := s.Work(ctx, id); err == nil {
				works[i] = w
			}
		})
	}
	wg.Wait()
	var out []*MetaWork
	for _, w := range works {
		if w != nil {
			out = append(out, w)
		}
	}
	return out
}

// ChapterAt is the 1-based number of the chapter holding position, given each
// chapter's whole-book start offset (book_offset) in ascending order: the last
// chapter whose start is at or before position. 0 when there are no chapters or
// position <= 0, which gates a book nobody has started (or one without chapters)
// to "the start only". It mirrors the player's meta-gating.ts chapterNumberAt
// exactly, so a server-gated envelope and a client-gated one agree.
func ChapterAt(starts []float64, position float64) int {
	if len(starts) == 0 || position <= 0 {
		return 0
	}
	n := 0
	for i, start := range starts {
		if position < start {
			break
		}
		n = i + 1
	}
	return n
}

// HideSpoilers returns a copy of env gated for a listener at chapter (see
// ChapterAt) of the current work, finished or not:
//   - finished: the current work is kept whole;
//   - a character is kept iff it is revealed by max(chapter, 1);
//   - a recap is kept iff it covers chapter 0 (before this book) or a chapter
//     already behind the listener (through < chapter);
//   - recap_summary is dropped unless finished (in_short and ending both
//     summarize the whole book).
//
// Previous works keep their cast and recaps and lose only their recap_summary's
// ending. A work left with no CC BY-SA content drops its attribution too. env is
// never modified: a changed work is a copy, an unchanged one is shared.
func HideSpoilers(env *Enrichment, chapter int, finished bool) *Enrichment {
	if env == nil {
		return nil
	}
	out := *env
	if env.Work != nil && !finished {
		out.Work = gateWork(env.Work, chapter)
	}
	if len(env.Previous) > 0 {
		prev := make([]*MetaWork, len(env.Previous))
		for i, w := range env.Previous {
			prev[i] = withoutEnding(w)
		}
		out.Previous = prev
	}
	return &out
}

// gateWork is HideSpoilers' copy of an unfinished current work.
func gateWork(w *MetaWork, chapter int) *MetaWork {
	c := *w
	reached := max(chapter, 1)
	c.Characters = nil
	for _, ch := range w.Characters {
		if ch.Reveal.Chapter <= reached {
			c.Characters = append(c.Characters, ch)
		}
	}
	c.Recaps = nil
	for _, r := range w.Recaps {
		if r.Through.Chapter == 0 || r.Through.Chapter < chapter {
			c.Recaps = append(c.Recaps, r)
		}
	}
	c.RecapSummary = nil
	if !carriesCommunityContent(&c) {
		c.Attribution = nil
	}
	return &c
}

// withoutEnding is w without its recap_summary's ending, or w itself when it
// has none. A summary left with nothing in it is dropped whole, as
// toRecapSummary would have.
func withoutEnding(w *MetaWork) *MetaWork {
	if w == nil || w.RecapSummary == nil || w.RecapSummary.Ending == "" {
		return w
	}
	c := *w
	c.RecapSummary = nil
	if w.RecapSummary.InShort != "" {
		c.RecapSummary = &MetaRecapSummary{InShort: w.RecapSummary.InShort}
	}
	if !carriesCommunityContent(&c) {
		c.Attribution = nil
	}
	return &c
}
