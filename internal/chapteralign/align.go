// Package chapteralign fits a community chapter list (a recording's chapters from
// the metadata site, timed on that recording's audio) onto a book's own audio, which
// can differ from it: an intro or outro trimmed, a publisher's preview missing, a
// little drift between releases, the book split into files.
//
// The fit is anchored and piecewise rather than one global shift. Local boundaries
// that can be trusted (a chapter whose title matches a community one nearby, a file
// boundary) are pinned to their community boundary; every community boundary between
// two pins is placed in proportion, then snapped to the pause it sits in (a Prober).
// A book whose audio does not fit (another edition, files split mid-chapter) gets no
// chapters, with the reason in the Result.
//
// The package does no I/O of its own: the files, chapters and pauses come in, a
// Result goes out.
package chapteralign

import (
	"cmp"
	"context"
	"math"
	"slices"
	"sort"

	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

// File is one of the book's audio files, in play order: its library-relative path
// and its duration in seconds. A single-file book is one File.
type File struct {
	Path     string
	Duration float64
}

// Remote is one community chapter, timed on the community recording.
type Remote struct {
	Title    string `json:"title"`
	StartMS  int64  `json:"start_ms"`
	LengthMS int64  `json:"length_ms"`
}

// Silence is a pause in one file, in seconds from the file's start.
type Silence struct{ Start, End float64 }

// Prober finds the pauses in [from, to] of one file (seconds from the file's
// start; file is a File.Path). A nil Prober (no ffmpeg) places boundaries by
// proportion alone; a window it fails on is left unsnapped.
type Prober func(ctx context.Context, file string, from, to float64) ([]Silence, error)

// Status is the outcome of a fit.
type Status string

// The fitted statuses (Chapters is set) and the reasons a fit failed (it is not).
const (
	// Fill: the book has no chapters of its own (none, or one per file); the
	// community chapters replace them.
	Fill Status = "fill"
	// Titles: the book's chapters are the community ones, by time, but some
	// titles differ (TitleDiffs).
	Titles Status = "titles"
	// Refine: every chapter of the book is a community boundary too, and the
	// community has more (finer) chapters.
	Refine Status = "refine"
	// Restructure: the community chapters fit the audio but divide it differently.
	Restructure Status = "restructure"
	// Same: the community chapters are the book's own.
	Same Status = "same"

	// LengthMismatch: the audio is not the community recording's (another
	// edition, abridged, a different cut).
	LengthMismatch Status = "length_mismatch"
	// StructureMismatch: too few of the book's own chapters are community ones.
	StructureMismatch Status = "structure_mismatch"
	// CrossesFiles: a community chapter starts in one file and ends in the next
	// (Straddle), so it can't be played as one chapter of a file.
	CrossesFiles Status = "crosses_files"
)

// Fitted reports whether s carries chapters.
func (s Status) Fitted() bool {
	switch s {
	case Fill, Titles, Refine, Restructure, Same:
		return true
	}
	return false
}

// TitleDiff is one of the book's chapters (Index) whose community title differs.
type TitleDiff struct {
	Index     int    `json:"index"`
	Current   string `json:"current"`
	Community string `json:"community"`
}

// Straddle is the community chapter a file boundary falls inside: its title, where
// the boundary is on the book's timeline, and the files either side.
type Straddle struct {
	Title string  `json:"title"`
	At    float64 `json:"at"`
	From  string  `json:"from"`
	To    string  `json:"to"`
}

// Detail is what a fit found, for the admin: everything but the chapters.
type Detail struct {
	// LocalDuration and CommunityDuration are the two timelines' lengths.
	LocalDuration     float64 `json:"local_duration"`
	CommunityDuration float64 `json:"community_duration"`
	// Ratio is the local/community length of the anchored stretches (weighted
	// median of those long in the book), Worst the stretch furthest from 1; each
	// 0 when unmeasured.
	Ratio float64 `json:"ratio,omitempty"`
	Worst float64 `json:"worst,omitempty"`
	// LocalChapters and CommunityChapters count the two lists (the community's as
	// fitted, after Omitted).
	LocalChapters     int `json:"local_chapters"`
	CommunityChapters int `json:"community_chapters"`
	// Anchors is how many of the book's boundaries were pinned to community ones;
	// Snapped how many placed boundaries were moved onto a pause; Approximate how
	// many could not be and may sit a few seconds off.
	Anchors     int `json:"anchors"`
	Snapped     int `json:"snapped"`
	Approximate int `json:"approximate"`
	// Omitted are community chapters that are not in this copy (a preview, end
	// credits), by title.
	Omitted    []string    `json:"omitted,omitempty"`
	TitleDiffs []TitleDiff `json:"title_diffs,omitempty"`
	Straddle   *Straddle   `json:"straddle,omitempty"`
}

// Result is a fit: its status, what it found, and for a fitted status the
// community chapters on the book's timeline, in the scanner's shape.
type Result struct {
	Status   Status
	Detail   Detail
	Chapters []metadata.Chapter
}

// Tuning. Times are seconds.
const (
	// minTitleSim is the least word overlap (titleSim) that pairs two titles.
	minTitleSim = 0.6
	// titleWindow is how far apart (at least) a title pair may sit; it grows
	// with the position (titleWindowFrac) for a long book's drift.
	titleWindow     = 120.0
	titleWindowFrac = 0.01
	// timeTolerance is how close an untitled boundary must sit to a community
	// one, once the nearby anchors' offset is applied.
	timeTolerance = 5.0
	// offsetSearch bounds the nearest-boundary offsets the global estimate uses.
	offsetSearch = 30.0
	// endTolerance (at least; endToleranceFrac of the book when longer) is how
	// far the end of the audio may sit from the community end, or from the
	// community boundary that is taken as its end.
	endTolerance     = 30.0
	endToleranceFrac = 0.0015
	// maxOmitted (or maxOmittedFrac of the recording, if less) is the most of
	// the community recording that may be missing at the end of the audio (a
	// publisher's preview, end credits); more is another edition.
	maxOmitted     = 1200.0
	maxOmittedFrac = 0.10
	// A stretch between anchors shorter than minStretch on both timelines is not
	// measured; one that is must be within maxStretchSkew of the community
	// length, and the weighted median of those minStretch long in the book within
	// maxMedianSkew.
	minStretch     = 120.0
	maxStretchSkew = 0.10
	maxMedianSkew  = 0.01
	// minAnchored is the share of the book's own chapters that must be anchored.
	// One within edgeGrace of either end (an "Intro", an "Outro": a recording's
	// own credits) need not be.
	minAnchored = 0.8
	edgeGrace   = 30.0
	// snapWindow is how far (at least) around a placed boundary a pause is looked
	// for; the stretch's drift widens it, up to maxSnapWindow.
	snapWindow    = 4.0
	maxSnapWindow = 30.0
	// minPause is the shortest pause a boundary snaps to; leadIn is how long
	// before the speech resumes the chapter starts.
	minPause = 0.4
	leadIn   = 0.3
	// approxDrift is the drift over which an unsnapped boundary is approximate.
	approxDrift = 2.0
	// minChapter is the shortest chapter kept (a "Part One" heading can be half a
	// second); omitNote the shortest omitted community chapter named in
	// Detail.Omitted.
	minChapter = 0.25
	omitNote   = 60.0
)

// Align fits remote onto the book's files and its scanned chapters (local).
// probe may be nil (no snapping). The error is ctx's alone.
func Align(ctx context.Context, files []File, local []metadata.Chapter, remote []Remote, probe Prober) (*Result, error) {
	f := newFit(files, local, remote)
	if err := f.run(ctx, probe); err != nil {
		return nil, err
	}
	return &Result{Status: f.status, Detail: f.detail, Chapters: f.chapters}, nil
}

// boundary is a place on the book's timeline where a file or a chapter starts.
type boundary struct {
	t     float64
	title string
	file  int  // the file starting here; -1 for none
	real  bool // a chapter of the book's own starts here (not one per file)
	idx   int  // the local chapter starting here; -1 for none
	k     int  // the community boundary anchored to; -1 for none
}

// remoteBound is a community chapter's start on the community timeline.
type remoteBound struct {
	m     float64
	title string
	t     float64 // placed on the book's timeline
	pin   bool    // t is an anchor's, exact
}

// anchor pins a book time t to community time m (remote index k; -1 for the end).
type anchor struct {
	t, m float64
	k    int
}

type fit struct {
	files  []File
	cum    []float64 // each file's start on the book's timeline
	total  float64
	local  []boundary
	nLocal int  // the book's chapters
	real   bool // the book has chapters of its own
	remote []remoteBound
	mEnd   float64

	anchors  []anchor
	keptK    []int // the community boundary each emitted chapter starts at
	status   Status
	detail   Detail
	chapters []metadata.Chapter
}

func newFit(files []File, local []metadata.Chapter, remote []Remote) *fit {
	f := &fit{files: files, cum: make([]float64, len(files))}
	for i, fl := range files {
		f.cum[i] = f.total
		f.total += fl.Duration
	}
	f.buildLocal(local)
	rs := slices.Clone(remote)
	slices.SortStableFunc(rs, func(a, b Remote) int { return cmp.Compare(a.StartMS, b.StartMS) })
	for i, r := range rs {
		if i > 0 && r.StartMS == rs[i-1].StartMS {
			continue
		}
		f.remote = append(f.remote, remoteBound{m: float64(r.StartMS) / 1000, title: r.Title})
		f.mEnd = max(f.mEnd, float64(r.StartMS+r.LengthMS)/1000)
	}
	f.detail.LocalDuration, f.detail.CommunityDuration = round3(f.total), round3(f.mEnd)
	f.detail.LocalChapters = f.nLocal
	return f
}

// buildLocal lists the book's boundaries: every file start, and every chapter
// start (a chapter at a file's start is that file's boundary). A file holding two
// or more chapters has chapters of its own; one holding one is one per file.
func (f *fit) buildLocal(local []metadata.Chapter) {
	byPath := make(map[string]int, len(f.files))
	for i, fl := range f.files {
		byPath[fl.Path] = i
	}
	perFile := make([][]metadata.Chapter, len(f.files))
	for _, ch := range local {
		if i, ok := byPath[ch.FilePath]; ok {
			perFile[i] = append(perFile[i], ch)
		}
	}
	for i := range f.files {
		f.local = append(f.local, boundary{t: f.cum[i], file: i, idx: -1, k: -1})
		chs := perFile[i]
		slices.SortStableFunc(chs, func(a, b metadata.Chapter) int { return cmp.Compare(a.Start, b.Start) })
		own := len(chs) > 1
		f.real = f.real || own
		for _, ch := range chs {
			f.nLocal++
			b := &f.local[len(f.local)-1]
			if ch.Start > 0.05 {
				f.local = append(f.local, boundary{t: f.cum[i] + ch.Start, file: -1, k: -1})
				b = &f.local[len(f.local)-1]
			}
			b.title, b.real, b.idx = ch.Title, own, ch.Index
		}
	}
}

func (f *fit) run(ctx context.Context, probe Prober) error {
	if len(f.files) == 0 || f.total <= 0 || len(f.remote) < 2 || f.mEnd <= 0 {
		f.status = LengthMismatch
		return nil
	}
	f.anchorTitles()
	f.anchorTimes()
	if !f.anchorEnd() {
		f.status = LengthMismatch
		return nil
	}
	if st := f.checkStretches(); st != "" {
		f.status = st
		return nil
	}
	if st := f.checkCoverage(); st != "" {
		f.status = st
		return nil
	}
	f.place()
	if err := f.snap(ctx, probe); err != nil {
		return err
	}
	f.emit()
	f.classify()
	return nil
}

// anchorTitles pins each titled boundary to the community boundary nearby with the
// most similar title (the nearest of equals), keeping the pairs in order. The
// book's start is pinned to the community start whatever its title.
func (f *fit) anchorTitles() {
	f.local[0].k = 0
	next := 1
	for i := 1; i < len(f.local); i++ {
		b := &f.local[i]
		if b.title == "" {
			continue
		}
		win := max(titleWindow, titleWindowFrac*b.t)
		best, bestSim, bestDist := -1, 0.0, math.Inf(1)
		for k := next; k < len(f.remote); k++ {
			d := f.remote[k].m - b.t
			if d > win {
				break
			}
			if -d > win {
				continue
			}
			s := titleSim(b.title, f.remote[k].title)
			if s < minTitleSim {
				continue
			}
			if s > bestSim+1e-9 || (math.Abs(s-bestSim) <= 1e-9 && math.Abs(d) < bestDist) {
				best, bestSim, bestDist = k, s, math.Abs(d)
			}
		}
		if best >= 0 {
			b.k, next = best, best+1
		}
	}
}

// anchorTimes pins each boundary still unpinned (a file boundary, an untitled or
// renamed chapter) to the community boundary at its time, once the offset of the
// anchors around it is applied, within timeTolerance; in order, so each new pin
// is an offset for the next.
func (f *fit) anchorTimes() {
	global := f.globalOffset()
	for i := 1; i < len(f.local); i++ {
		b := &f.local[i]
		if b.k >= 0 {
			continue
		}
		lo, hi := 0, len(f.remote) // the open range of community indexes in order
		off, offSet := 0.0, false
		for p := i - 1; p >= 0; p-- {
			if f.local[p].k >= 0 {
				lo = f.local[p].k
				if p > 0 {
					off, offSet = f.remote[lo].m-f.local[p].t, true
				}
				break
			}
		}
		for n := i + 1; n < len(f.local); n++ {
			if f.local[n].k >= 0 {
				hi = f.local[n].k
				if !offSet {
					off, offSet = f.remote[hi].m-f.local[n].t, true
				}
				break
			}
		}
		if !offSet {
			off = global
		}
		want := b.t + off
		best, bestDist := -1, timeTolerance
		for k := lo + 1; k < hi; k++ {
			if d := math.Abs(f.remote[k].m - want); d <= bestDist {
				best, bestDist = k, d
			}
		}
		if best >= 0 {
			b.k = best
		}
	}
}

// globalOffset is the median offset from each boundary to the community boundary
// nearest it (within offsetSearch): how far the community timeline runs ahead of
// the book's when nothing nearer says.
func (f *fit) globalOffset() float64 {
	var offs []float64
	for _, b := range f.local[1:] {
		k := sort.Search(len(f.remote), func(k int) bool { return f.remote[k].m >= b.t })
		best := math.Inf(1)
		for _, c := range []int{k - 1, k} {
			if c >= 0 && c < len(f.remote) {
				if d := f.remote[c].m - b.t; math.Abs(d) < math.Abs(best) {
					best = d
				}
			}
		}
		if math.Abs(best) <= offsetSearch {
			offs = append(offs, best)
		}
	}
	if len(offs) == 0 {
		return 0
	}
	return median(offs)
}

// anchorEnd collects the anchors in order and pins the end of the audio: to the
// community end, or else to the community boundary at the end (what follows it,
// a preview or credits, is not in this copy, so it must be short). false when
// neither is near.
func (f *fit) anchorEnd() bool {
	for _, b := range f.local {
		if b.k >= 0 {
			f.anchors = append(f.anchors, anchor{t: b.t, m: f.remote[b.k].m, k: b.k})
		}
	}
	f.detail.Anchors = len(f.anchors) - 1 // the start is pinned by construction
	last := f.anchors[len(f.anchors)-1]
	want := f.total + (last.m - last.t)
	if last.k == 0 {
		want = f.total
	}
	tol := max(endTolerance, endToleranceFrac*f.total)
	if math.Abs(f.mEnd-want) <= tol {
		f.anchors = append(f.anchors, anchor{t: f.total, m: f.mEnd, k: -1})
		return true
	}
	best, bestDist := -1, tol
	for k := last.k + 1; k < len(f.remote); k++ {
		if d := math.Abs(f.remote[k].m - want); d <= bestDist {
			best, bestDist = k, d
		}
	}
	if best < 0 || f.mEnd-f.remote[best].m > min(maxOmitted, maxOmittedFrac*f.mEnd) {
		return false
	}
	f.anchors = append(f.anchors, anchor{t: f.total, m: f.remote[best].m, k: -1})
	return true
}

// checkStretches measures each stretch between anchors: the audio is another
// edition when one is far from the community's length, or the weighted median is.
func (f *fit) checkStretches() Status {
	var ratios, weights []float64
	worst, measured := 1.0, false
	for i := 1; i < len(f.anchors); i++ {
		lt, lm := f.anchors[i].t-f.anchors[i-1].t, f.anchors[i].m-f.anchors[i-1].m
		// Long on either timeline is held to maxStretchSkew (a short stretch of
		// the book against a long one of the recording is audio this copy lacks);
		// the book's long stretches make the median.
		if max(lt, lm) < minStretch || lm <= 0 {
			continue
		}
		r := lt / lm
		measured = true
		if math.Abs(r-1) > math.Abs(worst-1) {
			worst = r
		}
		if lt >= minStretch {
			ratios, weights = append(ratios, r), append(weights, lt)
		}
	}
	if !measured {
		return ""
	}
	f.detail.Worst = round4(worst)
	skewed := math.Abs(worst-1) > maxStretchSkew
	if len(ratios) > 0 {
		med := weightedMedian(ratios, weights)
		f.detail.Ratio = round4(med)
		skewed = skewed || math.Abs(med-1) > maxMedianSkew
	}
	if skewed {
		return LengthMismatch
	}
	return ""
}

// checkCoverage requires every file boundary anchored (else a community chapter
// straddles it) and most of the book's own chapters.
func (f *fit) checkCoverage() Status {
	var own, pinned int
	for _, b := range f.local[1:] {
		if b.file > 0 && b.k < 0 {
			f.detail.Straddle = f.straddle(b)
			return CrossesFiles
		}
		if b.real && !f.edge(b) {
			own++
			if b.k >= 0 {
				pinned++
			}
		}
	}
	if own > 0 && float64(pinned) < minAnchored*float64(own) {
		return StructureMismatch
	}
	return ""
}

// edge reports whether b is within edgeGrace of either end of the audio.
func (f *fit) edge(b boundary) bool {
	return b.t <= edgeGrace || f.total-b.t <= edgeGrace
}

// straddle names the community chapter the file boundary b falls inside.
func (f *fit) straddle(b boundary) *Straddle {
	m := f.toRemote(b.t)
	k := max(sort.Search(len(f.remote), func(k int) bool { return f.remote[k].m > m })-1, 0)
	return &Straddle{
		Title: f.remote[k].title,
		At:    round3(b.t),
		From:  f.files[b.file-1].Path,
		To:    f.files[b.file].Path,
	}
}

// toRemote maps a book time to the community timeline through the anchors.
func (f *fit) toRemote(t float64) float64 {
	a := f.anchors
	for i := 1; i < len(a); i++ {
		if t <= a[i].t || i == len(a)-1 {
			return interp(t, a[i-1].t, a[i].t, a[i-1].m, a[i].m)
		}
	}
	return t
}

// toLocal maps a community time to the book's timeline through the anchors, with
// the stretch's drift (how much its lengths differ).
func (f *fit) toLocal(m float64) (t, drift float64) {
	a := f.anchors
	for i := 1; i < len(a); i++ {
		if m < a[i].m || i == len(a)-1 {
			drift = math.Abs((a[i].t - a[i-1].t) - (a[i].m - a[i-1].m))
			return interp(m, a[i-1].m, a[i].m, a[i-1].t, a[i].t), drift
		}
	}
	return m, 0
}

// place puts every community boundary on the book's timeline: an anchored one
// exactly, the rest in proportion between the anchors around it.
func (f *fit) place() {
	for _, b := range f.local {
		if b.k >= 0 {
			f.remote[b.k].t, f.remote[b.k].pin = b.t, true
		}
	}
	for k := range f.remote {
		if !f.remote[k].pin {
			f.remote[k].t, _ = f.toLocal(f.remote[k].m)
		}
	}
}

// snap moves each placed (unpinned) boundary inside the audio onto the longest
// pause near it, never past its neighbours. One that can't be snapped is
// approximate when its stretch drifts.
func (f *fit) snap(ctx context.Context, probe Prober) error {
	for k := range f.remote {
		r := &f.remote[k]
		if r.pin || r.t <= 0 || r.t >= f.total {
			continue
		}
		_, drift := f.toLocal(r.m)
		moved := false
		if probe != nil {
			w := min(snapWindow+drift, maxSnapWindow)
			lo, hi := r.t-w, r.t+w
			if k > 0 {
				lo = max(lo, f.remote[k-1].t+minChapter)
			}
			if k+1 < len(f.remote) {
				hi = min(hi, f.remote[k+1].t-minChapter)
			}
			fi := f.fileAt(r.t)
			lo, hi = max(lo, f.cum[fi]), min(hi, f.cum[fi]+f.files[fi].Duration)
			if hi > lo {
				pauses, err := probe(ctx, f.files[fi].Path, lo-f.cum[fi], hi-f.cum[fi])
				if err := ctx.Err(); err != nil {
					return err
				}
				if p, ok := longest(pauses); err == nil && ok {
					r.t = f.cum[fi] + max(p.Start, p.End-leadIn)
					moved = true
					f.detail.Snapped++
				}
			}
		}
		if !moved && drift > approxDrift {
			f.detail.Approximate++
		}
	}
	return nil
}

// longest is the longest pause of at least minPause.
func longest(pauses []Silence) (Silence, bool) {
	var best Silence
	ok := false
	for _, p := range pauses {
		if d := p.End - p.Start; d >= minPause && (!ok || d > best.End-best.Start) {
			best, ok = p, true
		}
	}
	return best, ok
}

// fileAt is the file playing at book time t.
func (f *fit) fileAt(t float64) int {
	return max(sort.Search(len(f.cum), func(i int) bool { return f.cum[i] > t })-1, 0)
}

// emit builds the chapters: each community chapter from its start to the next
// one's (or the end of the audio), dropping those under minChapter (the ones
// past the end are listed as omitted). The chapters cover the audio with no gap:
// the first starts at 0, and the first in each file at the file's start.
func (f *fit) emit() {
	type span struct {
		k          int
		start, end float64
	}
	var kept []span
	for k, r := range f.remote {
		end := f.total
		if k+1 < len(f.remote) {
			end = min(f.remote[k+1].t, f.total)
		}
		start := max(r.t, 0)
		if end-start < minChapter {
			if start >= f.total-minChapter && f.remoteLen(k) >= omitNote {
				f.detail.Omitted = append(f.detail.Omitted, r.title)
			}
			continue
		}
		kept = append(kept, span{k: k, start: start, end: end})
	}
	for i := range kept {
		s := &kept[i]
		fi := f.fileAt(s.start)
		if i == 0 || f.fileAt(kept[i-1].start) != fi {
			s.start = f.cum[fi]
		}
		if i+1 < len(kept) {
			s.end = kept[i+1].start
		}
		s.end = min(s.end, f.cum[fi]+f.files[fi].Duration)
	}
	// An end is read before the next chapter is pulled back to its file's start,
	// but only a chapter in a later file is pulled, and the end is then cut to its
	// own file's end: that start, every file boundary being anchored (so starting a
	// kept chapter), so no gap is left behind.
	f.chapters = make([]metadata.Chapter, 0, len(kept))
	f.keptK = make([]int, 0, len(kept))
	for i, s := range kept {
		f.keptK = append(f.keptK, s.k)
		fi := f.fileAt(s.start)
		f.chapters = append(f.chapters, metadata.Chapter{
			Index:      i,
			Title:      f.remote[s.k].title,
			FileIndex:  fi,
			FilePath:   f.files[fi].Path,
			Start:      round3(s.start - f.cum[fi]),
			End:        round3(s.end - f.cum[fi]),
			BookOffset: round3(s.start),
		})
	}
	f.detail.CommunityChapters = len(f.chapters)
}

// remoteLen is community chapter k's length.
func (f *fit) remoteLen(k int) float64 {
	if k+1 < len(f.remote) {
		return f.remote[k+1].m - f.remote[k].m
	}
	return f.mEnd - f.remote[k].m
}

// classify says what the fitted chapters are to the book's own.
func (f *fit) classify() {
	if !f.real {
		f.status = Fill
		return
	}
	var locals []boundary // the boundaries a chapter of the book starts at
	allPinned := true
	for _, b := range f.local {
		if b.idx < 0 {
			continue
		}
		locals = append(locals, b)
		if b.real && b.k < 0 && !f.edge(b) {
			allPinned = false
		}
	}
	if len(locals) == len(f.keptK) {
		same := true
		for i, b := range locals {
			if b.k != f.keptK[i] {
				same = false
				break
			}
		}
		if same {
			for _, b := range locals {
				if c := f.remote[b.k].title; match.Fold(c) != match.Fold(b.title) {
					f.detail.TitleDiffs = append(f.detail.TitleDiffs, TitleDiff{Index: b.idx, Current: b.title, Community: c})
				}
			}
			f.status = Same
			if len(f.detail.TitleDiffs) > 0 {
				f.status = Titles
			}
			return
		}
	}
	if allPinned && len(f.keptK) > len(locals) {
		f.status = Refine
		return
	}
	f.status = Restructure
}

// ---- small helpers ----

func interp(x, x0, x1, y0, y1 float64) float64 {
	if x1 == x0 {
		return y0
	}
	return y0 + (x-x0)*(y1-y0)/(x1-x0)
}

func median(v []float64) float64 {
	s := slices.Clone(v)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// weightedMedian is the value at half the total weight.
func weightedMedian(v, w []float64) float64 {
	idx := make([]int, len(v))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return v[idx[a]] < v[idx[b]] })
	var total float64
	for _, x := range w {
		total += x
	}
	var acc float64
	for _, i := range idx {
		acc += w[i]
		if acc >= total/2 {
			return v[i]
		}
	}
	return v[idx[len(idx)-1]]
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }
func round4(x float64) float64 { return math.Round(x*10000) / 10000 }
