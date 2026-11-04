package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

const hamletWork = `{"id":"hamlet","title":"Hamlet","authors":[],"language":"en",` +
	`"characters":[{"id":"prologue","name":"P","reveal":{"chapter":0}},{"id":"hamlet","name":"H","reveal":{"chapter":1}},{"id":"fortinbras","name":"F","reveal":{"chapter":5}}],` +
	`"recaps":[{"through":{"chapter":0},"scope":"series","text":"Before."},{"through":{"chapter":1},"text":"Act 1."},{"through":{"chapter":5},"text":"Act 5."}],` +
	`"recap_summary":{"in_short":"All die.","ending":"All die."},"position_scale":{"length":5,"unit":"act"}}`

// scaleEnv serves Hamlet on its 5 acts upstream, for any ASIN but B0PLAIN, whose work
// states no scale, and an empty library with an admin's token.
func scaleEnv(t *testing.T) (*testEnv, *catalog.Library, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		id := "hamlet"
		if r.URL.Query().Get("asin") == "B0PLAIN" {
			id = "plain"
		}
		_, _ = w.Write([]byte(`{"work":{"id":"` + id + `","title":"T","authors":[]}}`))
	})
	mux.HandleFunc("GET /api/v1/works/hamlet", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(hamletWork))
	})
	mux.HandleFunc("GET /api/v1/works/plain", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"plain","title":"Plain","authors":[],"language":"en",` +
			`"characters":[{"id":"late","name":"L","reveal":{"chapter":5}}],` +
			`"recaps":[{"through":{"chapter":5},"text":"Act 5."}]}`))
	})
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Metadata.Enabled = true
		c.Metadata.BaseURL = mock.URL
	})
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	return e, lib, tok
}

// seedTracks seeds a book of n 100-second chapters.
func seedTracks(t *testing.T, e *testEnv, lib *catalog.Library, path, asin string, n int) {
	t.Helper()
	book := &catalog.Book{LibraryID: lib.ID, RelPath: path, Title: "T", Author: "A", ASIN: asin, AddedAt: "2020-01-01", Duration: float64(n * 100)}
	for i := range n {
		at := float64(i * 100)
		book.Chapters = append(book.Chapters, metadata.Chapter{Index: i, Title: "Ch", FilePath: path, Start: at, End: at + 100, BookOffset: at})
	}
	if _, err := e.cat.UpsertBook(context.Background(), book); err != nil {
		t.Fatal(err)
	}
}

// scaleBody is the slice of the answer these tests read.
type scaleBody struct {
	Work struct {
		Characters []struct {
			ID     string `json:"id"`
			Reveal struct {
				Chapter int `json:"chapter"`
			} `json:"reveal"`
		} `json:"characters"`
		Recaps []struct {
			Text    string `json:"text"`
			Scope   string `json:"scope"`
			Through struct {
				Chapter int `json:"chapter"`
			} `json:"through"`
		} `json:"recaps"`
		RecapSummary  json.RawMessage `json:"recap_summary"`
		PositionScale *struct {
			Length int    `json:"length"`
			Unit   string `json:"unit"`
		} `json:"position_scale"`
	} `json:"work"`
}

func (b scaleBody) reveals() []int {
	var out []int
	for _, c := range b.Work.Characters {
		out = append(out, c.Reveal.Chapter)
	}
	return out
}

func (b scaleBody) throughs() []int {
	var out []int
	for _, r := range b.Work.Recaps {
		out = append(out, r.Through.Chapter)
	}
	return out
}

func (b scaleBody) recapTexts() []string {
	var out []string
	for _, r := range b.Work.Recaps {
		out = append(out, r.Text)
	}
	return out
}

func getScale(t *testing.T, e *testEnv, lib *catalog.Library, path, query, tok string) scaleBody {
	t.Helper()
	url := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/meta?path=" + escape(path) + query
	resp, body := e.do(t, "GET", url, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s", url, resp.StatusCode, body)
	}
	var b scaleBody
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	return b
}

// Finer than the scale: the numbers answered are the recording's, a reveal at its act's
// start and a through at its act's end, with position 0 untouched, and no scale beside
// them. Books sharing the ASIN with chapters on the scale, or coarser, get the numbers as
// published, whichever asked first, because the envelope cached by the ASIN is never
// written to.
func TestMetaTranslatesPositionsOntoTheBook(t *testing.T) {
	t.Parallel()
	e, lib, tok := scaleEnv(t)
	seedTracks(t, e, lib, "A/Fine", "B0HAMLET", 43)
	seedTracks(t, e, lib, "A/Acts", "B0HAMLET", 5)
	seedTracks(t, e, lib, "A/Coarse", "B0HAMLET", 3)

	wantFine := func(b scaleBody) {
		t.Helper()
		if got := b.reveals(); !slices.Equal(got, []int{0, 1, 36}) {
			t.Errorf("fine reveals = %v", got)
		}
		if got := b.throughs(); !slices.Equal(got, []int{0, 9, 43}) {
			t.Errorf("fine throughs = %v", got)
		}
		if b.Work.PositionScale != nil {
			t.Errorf("fine position_scale = %+v", b.Work.PositionScale)
		}
		if b.Work.Recaps[0].Scope != "series" {
			t.Errorf("a recap's scope was lost: %+v", b.Work.Recaps[0])
		}
	}
	wantFine(getScale(t, e, lib, "A/Fine", "", tok))

	for _, path := range []string{"A/Acts", "A/Coarse"} {
		b := getScale(t, e, lib, path, "", tok)
		if got := b.reveals(); !slices.Equal(got, []int{0, 1, 5}) {
			t.Errorf("%s reveals = %v", path, got)
		}
		if got := b.throughs(); !slices.Equal(got, []int{0, 1, 5}) {
			t.Errorf("%s throughs = %v", path, got)
		}
		if p := b.Work.PositionScale; p == nil || p.Length != 5 || p.Unit != "act" {
			t.Errorf("%s position_scale = %+v", path, p)
		}
	}

	// And the first again, off the now-cached envelope.
	wantFine(getScale(t, e, lib, "A/Fine", "", tok))

	// /meta/work names no book, so its numbers are the work's, with their scale.
	resp, body := e.do(t, "GET", "/api/v1/meta/work?id=hamlet", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta/work = %d %s", resp.StatusCode, body)
	}
	var work scaleBody
	if err := json.Unmarshal([]byte(body), &work); err != nil {
		t.Fatal(err)
	}
	if got := work.throughs(); !slices.Equal(got, []int{0, 1, 5}) || work.Work.PositionScale == nil {
		t.Errorf("meta/work throughs = %v, scale = %+v", got, work.Work.PositionScale)
	}
}

// No chapters, and a lone chapter (no place to read), pass through; so does a work
// stating no scale, on any recording.
func TestMetaPositionsPassThrough(t *testing.T) {
	t.Parallel()
	e, lib, tok := scaleEnv(t)
	seedTracks(t, e, lib, "A/None", "B0HAMLET", 0)
	seedTracks(t, e, lib, "A/One", "B0HAMLET", 1)
	seedTracks(t, e, lib, "A/Plain", "B0PLAIN", 43)

	for _, path := range []string{"A/None", "A/One"} {
		b := getScale(t, e, lib, path, "", tok)
		if got := b.throughs(); !slices.Equal(got, []int{0, 1, 5}) {
			t.Errorf("%s throughs = %v", path, got)
		}
		if b.Work.PositionScale == nil {
			t.Errorf("%s lost its scale", path)
		}
	}
	plain := getScale(t, e, lib, "A/Plain", "", tok)
	if got := plain.throughs(); !slices.Equal(got, []int{5}) || plain.Work.PositionScale != nil {
		t.Errorf("no scale: throughs = %v, scale = %+v", got, plain.Work.PositionScale)
	}
}

// progressSaver saves the admin's place in a book, each save newer than the last, the
// only kind a save may replace.
func progressSaver(t *testing.T, e *testEnv, ref catalog.Ref) func(position float64, finished bool) {
	t.Helper()
	saves := 0
	return func(position float64, finished bool) {
		t.Helper()
		saves++
		at := "2020-01-" + fmt.Sprintf("%02d", saves) + "T00:00:00Z"
		if _, err := e.cat.SaveProgress(context.Background(), e.adminID, catalog.Progress{Ref: ref, Position: position, Duration: 4300, Finished: finished, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
}

// The defect by name: the 43-track Hamlet on 5 acts, through spoilers=hide, which gates
// on the translated numbers and returns those same numbers, so a client gating again
// agrees.
func TestMetaHamletSpoilersHideOnTranslatedPositions(t *testing.T) {
	t.Parallel()
	e, lib, tok := scaleEnv(t)
	seedTracks(t, e, lib, "A/Fine", "B0HAMLET", 43)
	save := progressSaver(t, e, catalog.Ref{LibraryID: lib.ID, Path: "A/Fine"})
	hide := func() scaleBody { t.Helper(); return getScale(t, e, lib, "A/Fine", "&spoilers=hide", tok) }

	// Not started: the opening cast shows, Act 1's lead included.
	if got := hide().reveals(); !slices.Equal(got, []int{0, 1}) {
		t.Fatalf("not started: reveals = %v", got)
	}

	save(550, false) // inside track 6: denied, Act 5's recap, and everything past Act 1's cast
	b := hide()
	if got := b.recapTexts(); !slices.Equal(got, []string{"Before."}) {
		t.Fatalf("recaps at track 6 = %v", got)
	}
	if got := b.reveals(); !slices.Equal(got, []int{0, 1}) || b.Work.RecapSummary != nil {
		t.Fatalf("at track 6: reveals = %v, summary = %s", got, b.Work.RecapSummary)
	}

	save(850, false) // inside track 9, with Act 1 (to 860 s) still playing: denied
	if got := hide().recapTexts(); !slices.Equal(got, []string{"Before."}) {
		t.Fatalf("recaps at track 9 = %v", got)
	}

	save(950, false) // inside track 10, Act 1 over: allowed, on the translated number
	b = hide()
	if got := b.recapTexts(); !slices.Equal(got, []string{"Before.", "Act 1."}) {
		t.Fatalf("recaps at track 10 = %v", got)
	}
	if got := b.throughs(); !slices.Equal(got, []int{0, 9}) {
		t.Fatalf("the numbers kept must be the translated ones: %v", got)
	}

	save(3450, false) // inside track 35, Act 5 (from 3440 s) begun but no track starting it
	if got := hide().reveals(); !slices.Equal(got, []int{0, 1}) {
		t.Fatalf("reveals at track 35 = %v", got)
	}

	save(3550, false) // track 36, the first starting past Act 5's start
	if got := hide().reveals(); !slices.Equal(got, []int{0, 1, 36}) {
		t.Fatalf("reveals at track 36 = %v", got)
	}

	save(4250, false) // inside the last track: still not Act 5's recap
	if got := hide().recapTexts(); slices.Contains(got, "Act 5.") {
		t.Fatalf("recaps at the last track = %v", got)
	}

	save(100, true) // finished, wherever the place: everything, still translated
	b = hide()
	if got := b.throughs(); !slices.Equal(got, []int{0, 9, 43}) || b.Work.RecapSummary == nil {
		t.Fatalf("finished: throughs = %v, summary = %s", got, b.Work.RecapSummary)
	}
}

// The older bug: every chapter's book_offset 0 walked as a timeline put any place in the
// last chapter, so spoilers=hide showed everything. It reads by count now, rounded down,
// and with no duration to read it against, as not started.
func TestMetaSpoilersHideUnknownOffsets(t *testing.T) {
	t.Parallel()
	e, lib, tok := scaleEnv(t)
	ctx := context.Background()
	seedZeroOffsets := func(path string, duration float64) {
		t.Helper()
		book := &catalog.Book{LibraryID: lib.ID, RelPath: path, Title: "T", Author: "A", ASIN: "B0PLAIN", AddedAt: "2020-01-01", Duration: duration}
		for i := range 10 {
			book.Chapters = append(book.Chapters, metadata.Chapter{Index: i, Title: "Ch", FilePath: path})
		}
		if _, err := e.cat.UpsertBook(ctx, book); err != nil {
			t.Fatal(err)
		}
	}
	seedZeroOffsets("A/Timed", 1000)
	seedZeroOffsets("A/Untimed", 0)
	saves := 0
	save := func(path string, position, duration float64) {
		t.Helper()
		saves++
		at := "2020-01-" + fmt.Sprintf("%02d", saves) + "T00:00:00Z"
		if _, err := e.cat.SaveProgress(ctx, e.adminID, catalog.Progress{Ref: catalog.Ref{LibraryID: lib.ID, Path: path}, Position: position, Duration: duration, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}

	// The plain work's cast and recap sit at position 5.
	save("A/Timed", 300, 0) // 3 of 10 shares: denied
	if b := getScale(t, e, lib, "A/Timed", "&spoilers=hide", tok); len(b.Work.Characters) != 0 || len(b.Work.Recaps) != 0 {
		t.Fatalf("a place early in the book read as late: %+v", b.Work)
	}
	save("A/Untimed", 300, 0) // no duration anywhere: not started, denied
	if b := getScale(t, e, lib, "A/Untimed", "&spoilers=hide", tok); len(b.Work.Characters) != 0 || len(b.Work.Recaps) != 0 {
		t.Fatalf("an unknown place read as the end: %+v", b.Work)
	}
	save("A/Timed", 650, 0) // 6 of 10 shares passed: allowed, the count still reads a place
	b := getScale(t, e, lib, "A/Timed", "&spoilers=hide", tok)
	if len(b.Work.Characters) != 1 || len(b.Work.Recaps) != 1 {
		t.Fatalf("a place read by count = %+v", b.Work)
	}
	save("A/Untimed", 650, 1000) // none on the book: the one saved with the place reads it
	b = getScale(t, e, lib, "A/Untimed", "&spoilers=hide", tok)
	if len(b.Work.Characters) != 1 || len(b.Work.Recaps) != 1 {
		t.Fatalf("a place read by the saved duration = %+v", b.Work)
	}
}

// Generated content carries no scale and is numbered on the book already: a recap a
// Bridge generated keeps its number beside community characters on the same chapters.
func TestMetaGeneratedPositionsUntouched(t *testing.T) {
	t.Parallel()
	e := generatedMetaEnv(t, `{"id":"w","title":"T","authors":[],"language":"en",
		"characters":[{"id":"hamlet","name":"H","reveal":{"chapter":1}}],"position_scale":{"length":5,"unit":"act"}}`)
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	seedTracks(t, e, lib, "A/Fine", "B0HAMLET", 43)
	setGenerated(t, e, lib.ID, "A/Fine")
	b := getScale(t, e, lib, "A/Fine", "", tok)
	if got := b.reveals(); !slices.Equal(got, []int{1}) {
		t.Fatalf("community reveals = %v", got)
	}
	if got := b.throughs(); !slices.Equal(got, []int{0, 3}) {
		t.Fatalf("generated throughs = %v, want its own 0 and 3", got)
	}
}
