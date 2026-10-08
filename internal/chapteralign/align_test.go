package chapteralign

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// recording is a synthetic community recording: chapter lengths (seconds) and
// titles, with a 2 s pause before each chapter starts and a 0.6 s one in the middle
// of each, so the snap has a wrong pause to pass over.
type recording struct {
	titles  []string
	lengths []float64
}

func newRecording(n int, length float64) recording {
	r := recording{}
	for i := range n {
		r.titles = append(r.titles, fmt.Sprintf("Story %c", 'A'+i))
		r.lengths = append(r.lengths, length+float64(i*37%90))
	}
	return r
}

func (r recording) starts() []float64 {
	var out []float64
	var t float64
	for _, l := range r.lengths {
		out = append(out, t)
		t += l
	}
	return out
}

func (r recording) total() float64 {
	var t float64
	for _, l := range r.lengths {
		t += l
	}
	return t
}

func (r recording) remote() []Remote {
	var out []Remote
	for i, s := range r.starts() {
		out = append(out, Remote{Title: r.titles[i], StartMS: int64(math.Round(s * 1000)), LengthMS: int64(math.Round(r.lengths[i] * 1000))})
	}
	return out
}

// pauses are the recording's pauses on a timeline shifted by -trim (the audio
// with its first trim seconds cut).
func (r recording) pauses(trim float64) []Silence {
	var out []Silence
	for i, s := range r.starts() {
		if i > 0 {
			out = append(out, Silence{Start: s - 2 - trim, End: s - trim})
		}
		mid := s + r.lengths[i]/2 - trim
		out = append(out, Silence{Start: mid, End: mid + 0.6})
	}
	return out
}

// split is a Prober over pauses on the book's timeline, for a book cut into
// files: a window in file f is answered in f's own seconds.
func split(files []File, pauses []Silence) Prober {
	return func(_ context.Context, file string, from, to float64) ([]Silence, error) {
		var off float64
		for _, f := range files {
			if f.Path == file {
				break
			}
			off += f.Duration
		}
		var out []Silence
		for _, p := range pauses {
			s, e := p.Start-off, p.End-off
			if e > from && s < to {
				out = append(out, Silence{Start: max(s, from), End: min(e, to)})
			}
		}
		return out, nil
	}
}

func oneFile(d float64) []File { return []File{{Path: "B/book.m4b", Duration: d}} }

// localChapters are chapters of a single-file book at the given starts.
func localChapters(total float64, starts []float64, titles []string) []metadata.Chapter {
	var out []metadata.Chapter
	for i, s := range starts {
		end := total
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		out = append(out, metadata.Chapter{Index: i, Title: titles[i], FilePath: "B/book.m4b", Start: s, End: end, BookOffset: s})
	}
	return out
}

func align(t *testing.T, files []File, local []metadata.Chapter, remote []Remote, probe Prober) *Result {
	t.Helper()
	res, err := Align(context.Background(), files, local, remote, probe)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// wantStarts checks the chapters start at want (book time), within tol.
func wantStarts(t *testing.T, res *Result, want []float64, tol float64) {
	t.Helper()
	if len(res.Chapters) != len(want) {
		t.Fatalf("%d chapters, want %d (status %s, detail %+v)", len(res.Chapters), len(want), res.Status, res.Detail)
	}
	for i, c := range res.Chapters {
		if math.Abs(c.BookOffset-want[i]) > tol {
			t.Errorf("chapter %d %q starts at %.3f, want %.3f", i, c.Title, c.BookOffset, want[i])
		}
	}
}

// snapped is where a chapter starting at s in the recording lands once trim
// seconds are cut and it snaps to its pause (leadIn before the speech).
func snapped(starts []float64, trim float64) []float64 {
	out := make([]float64, len(starts))
	for i, s := range starts {
		if i > 0 {
			out[i] = s - trim - leadIn
		}
	}
	return out
}

func TestAlignFillsAChapterlessFile(t *testing.T) {
	r := newRecording(12, 1500)
	res := align(t, oneFile(r.total()), nil, r.remote(), split(oneFile(r.total()), r.pauses(0)))
	if res.Status != Fill {
		t.Fatalf("status %s, want fill (%+v)", res.Status, res.Detail)
	}
	wantStarts(t, res, snapped(r.starts(), 0), 0.01)
	if res.Detail.Snapped != 11 || res.Detail.Approximate != 0 {
		t.Errorf("snapped %d approximate %d, want 11 and 0", res.Detail.Snapped, res.Detail.Approximate)
	}
	if c := res.Chapters[3]; c.Title != "Story D" || c.FilePath != "B/book.m4b" || c.Start != c.BookOffset {
		t.Errorf("chapter 3 = %+v", c)
	}
}

func TestAlignTrimmedIntroAndOutro(t *testing.T) {
	r := newRecording(10, 1800)
	const intro, outro = 4.0, 6.0
	total := r.total() - intro - outro
	files := oneFile(total)
	res := align(t, files, nil, r.remote(), split(files, r.pauses(intro)))
	if res.Status != Fill {
		t.Fatalf("status %s (%+v)", res.Status, res.Detail)
	}
	wantStarts(t, res, snapped(r.starts(), intro), 0.01)
	last := res.Chapters[len(res.Chapters)-1]
	if last.End != math.Round(total*1000)/1000 {
		t.Errorf("last chapter ends at %v, want the end of the audio %v", last.End, total)
	}
}

func TestAlignWithoutPausesIsApproximate(t *testing.T) {
	r := newRecording(10, 1800)
	const intro = 12.0
	files := oneFile(r.total() - intro)
	res := align(t, files, nil, r.remote(), nil)
	if res.Status != Fill {
		t.Fatalf("status %s (%+v)", res.Status, res.Detail)
	}
	if res.Detail.Snapped != 0 || res.Detail.Approximate != 9 {
		t.Errorf("snapped %d approximate %d, want 0 and 9", res.Detail.Snapped, res.Detail.Approximate)
	}
	// In proportion, a boundary is at most the trim off.
	for i, s := range r.starts() {
		if d := math.Abs(res.Chapters[i].BookOffset - (s - intro)); i > 0 && d > intro {
			t.Errorf("chapter %d is %.1f s off", i, d)
		}
	}
}

func TestAlignOmitsWhatTheCopyLacks(t *testing.T) {
	r := newRecording(8, 2000)
	r.titles[7], r.lengths[7] = "Preview: The Next Book", 600
	total := r.total() - 600
	files := oneFile(total)
	res := align(t, files, nil, r.remote(), split(files, r.pauses(0)))
	if res.Status != Fill {
		t.Fatalf("status %s (%+v)", res.Status, res.Detail)
	}
	if len(res.Chapters) != 7 || !slices.Equal(res.Detail.Omitted, []string{"Preview: The Next Book"}) {
		t.Errorf("%d chapters, omitted %q; want 7 and the preview", len(res.Chapters), res.Detail.Omitted)
	}
}

func TestAlignRejectsAnotherEdition(t *testing.T) {
	r := newRecording(10, 1800)
	files := oneFile(r.total() * 0.95)
	res := align(t, files, nil, r.remote(), split(files, r.pauses(0)))
	if res.Status != LengthMismatch || res.Chapters != nil {
		t.Fatalf("status %s with %d chapters, want length_mismatch and none", res.Status, len(res.Chapters))
	}
	// Half the recording, ending exactly on a chapter boundary: not a copy
	// missing its preview, another (abridged) edition.
	half := 0.0
	for _, l := range r.lengths[:5] {
		half += l
	}
	files = oneFile(half)
	if res = align(t, files, nil, r.remote(), split(files, r.pauses(0))); res.Status != LengthMismatch {
		t.Fatalf("half: status %s, want length_mismatch (%+v)", res.Status, res.Detail)
	}
	// A chaptered copy whose stretches don't match is another edition too.
	starts := r.starts()
	stretched := make([]float64, len(starts))
	for i, s := range starts {
		stretched[i] = s * 1.04
	}
	files = oneFile(r.total() * 1.04)
	res = align(t, files, localChapters(files[0].Duration, stretched, r.titles), r.remote(), nil)
	if res.Status != LengthMismatch {
		t.Fatalf("stretched: status %s, want length_mismatch (%+v)", res.Status, res.Detail)
	}
}

// A stretch short in the book but long in the recording is measured too: the
// copy lacks most of a chapter, so it is another cut.
func TestAlignRejectsAStretchTheCopyLacks(t *testing.T) {
	r := newRecording(10, 1200)
	r.lengths[3] = 300
	// 100 s more than the recording before chapter 4, 300 s of chapter 4 cut to 100.
	var starts []float64
	for i, s := range r.starts() {
		switch {
		case i == 0:
			starts = append(starts, 0)
		case i <= 3:
			starts = append(starts, s+100)
		default:
			starts = append(starts, s-100)
		}
	}
	total := r.total() - 100
	res := align(t, oneFile(total), localChapters(total, starts, r.titles), r.remote(), nil)
	if res.Status != LengthMismatch {
		t.Fatalf("status %s, want length_mismatch (%+v)", res.Status, res.Detail)
	}
}

// folder cuts the recording into files at the given chapter indexes, each file
// one chapter per file as the scanner builds it (titled from its name).
func folder(r recording, cuts []int) ([]File, []metadata.Chapter) {
	starts := append(r.starts(), r.total())
	var files []File
	var chs []metadata.Chapter
	bounds := append([]int{0}, cuts...)
	bounds = append(bounds, len(r.lengths))
	for i := 0; i+1 < len(bounds); i++ {
		d := starts[bounds[i+1]] - starts[bounds[i]]
		p := fmt.Sprintf("B/%02d.mp3", i+1)
		files = append(files, File{Path: p, Duration: d})
		chs = append(chs, metadata.Chapter{Index: i, Title: fmt.Sprintf("Track %02d", i+1), FileIndex: i, FilePath: p, End: d, BookOffset: starts[bounds[i]]})
	}
	return files, chs
}

func TestAlignFolderSplitAtChapters(t *testing.T) {
	r := newRecording(12, 1500)
	files, chs := folder(r, []int{3, 7, 10})
	res := align(t, files, chs, r.remote(), split(files, r.pauses(0)))
	if res.Status != Fill {
		t.Fatalf("status %s (%+v)", res.Status, res.Detail)
	}
	want := snapped(r.starts(), 0)
	for _, cut := range []int{3, 7, 10} {
		want[cut] = r.starts()[cut] // a file boundary is exact
	}
	wantStarts(t, res, want, 0.01)
	for _, c := range res.Chapters {
		f := files[c.FileIndex]
		if c.FilePath != f.Path || c.Start < 0 || c.End > f.Duration+0.001 || c.End <= c.Start {
			t.Errorf("chapter %d %q: %+v outside %+v", c.Index, c.Title, c, f)
		}
	}
	if c := res.Chapters[7]; c.FileIndex != 2 || c.Start != 0 {
		t.Errorf("chapter 7 starts file 3: %+v", c)
	}
}

func TestAlignFolderSplitMidChapter(t *testing.T) {
	r := newRecording(12, 1500)
	files, chs := folder(r, []int{4})
	// Move the cut 10 minutes into chapter 5 ("Story E"'s successor).
	files[0].Duration += 600
	files[1].Duration -= 600
	chs[0].End += 600
	chs[1].End -= 600
	chs[1].BookOffset += 600
	res := align(t, files, chs, r.remote(), split(files, r.pauses(0)))
	if res.Status != CrossesFiles || res.Chapters != nil {
		t.Fatalf("status %s, want crosses_files with no chapters", res.Status)
	}
	s := res.Detail.Straddle
	if s == nil || s.Title != "Story E" || s.From != "B/01.mp3" || s.To != "B/02.mp3" {
		t.Errorf("straddle = %+v", s)
	}
}

func TestAlignTitles(t *testing.T) {
	r := newRecording(6, 1200)
	titles := slices.Clone(r.titles)
	titles[2], titles[4] = "Chapter 3", "story e" // "story e" reads the same
	chs := localChapters(r.total(), r.starts(), titles)
	res := align(t, oneFile(r.total()), chs, r.remote(), nil)
	if res.Status != Titles {
		t.Fatalf("status %s, want titles (%+v)", res.Status, res.Detail)
	}
	want := []TitleDiff{{Index: 2, Current: "Chapter 3", Community: "Story C"}}
	if !slices.Equal(res.Detail.TitleDiffs, want) {
		t.Errorf("diffs %+v, want %+v", res.Detail.TitleDiffs, want)
	}
	// The same titles everywhere: nothing to offer.
	res = align(t, oneFile(r.total()), localChapters(r.total(), r.starts(), r.titles), r.remote(), nil)
	if res.Status != Same {
		t.Errorf("status %s, want same", res.Status)
	}
}

func TestAlignRefineAndRestructure(t *testing.T) {
	r := newRecording(12, 1500)
	starts := r.starts()
	// Four broad chapters, each a community boundary with its title.
	idx := []int{0, 3, 6, 9}
	var bs []float64
	var ts []string
	for _, i := range idx {
		bs, ts = append(bs, starts[i]), append(ts, r.titles[i])
	}
	files := oneFile(r.total())
	res := align(t, files, localChapters(r.total(), bs, ts), r.remote(), split(files, r.pauses(0)))
	if res.Status != Refine || len(res.Chapters) != 12 {
		t.Fatalf("status %s with %d chapters, want refine with 12 (%+v)", res.Status, len(res.Chapters), res.Detail)
	}
	// Ten of the book's own, one not a community boundary: still fits, divided
	// differently.
	bs, ts = slices.Clone(starts[:10]), slices.Clone(r.titles[:10])
	bs[5] += 200
	ts[5] = "Interlude"
	res = align(t, files, localChapters(r.total(), bs, ts), r.remote(), nil)
	if res.Status != Restructure {
		t.Errorf("status %s, want restructure (%+v)", res.Status, res.Detail)
	}
	// Mostly not community boundaries: no fit.
	for i := 1; i < len(bs); i++ {
		bs[i] += 300
		ts[i] = fmt.Sprintf("Part %d", i)
	}
	res = align(t, files, localChapters(r.total(), bs, ts), r.remote(), nil)
	if res.Status != StructureMismatch {
		t.Errorf("status %s, want structure_mismatch (%+v)", res.Status, res.Detail)
	}
}

func TestAlignNeedsTwoChapters(t *testing.T) {
	res := align(t, oneFile(3600), nil, []Remote{{Title: "Whole", LengthMS: 3600000}}, nil)
	if res.Status.Fitted() {
		t.Errorf("status %s from one community chapter", res.Status)
	}
}

func TestAlignStopsOnCancel(t *testing.T) {
	r := newRecording(6, 1200)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probe := func(ctx context.Context, _ string, _, _ float64) ([]Silence, error) { return nil, ctx.Err() }
	if _, err := Align(ctx, oneFile(r.total()), nil, r.remote(), probe); err == nil {
		t.Error("no error from a cancelled fit")
	}
}

func TestTitleSim(t *testing.T) {
	for _, c := range []struct {
		a, b string
		ok   bool
	}{
		{"Out of Chaos", "1. Out of Chaos", true},
		{"The Punishments", "The Punishments - The Gift", true},
		{"Persephone and the Chariot", "2. Persophone and the Chariot", true},
		{"Chapter 12", "12", true},
		{"Chapter 12", "Chapter 13", false},
		{"Chapter 1", "Chapter 1: The Boy Who Lived", true},
		{"1.", "1. Out of Chaos", true},
		{"Part 2", "Chapter 2", false},
		{"Part 2", "Chapter 2: The Vanishing Glass", false},
		{"The End", "The Beginning", false},
		{"Track 01", "Opening Credits", false},
	} {
		if got := titleSim(c.a, c.b) >= minTitleSim; got != c.ok {
			t.Errorf("titleSim(%q, %q) = %.2f", c.a, c.b, titleSim(c.a, c.b))
		}
	}
}
