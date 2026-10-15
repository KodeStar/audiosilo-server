package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// generatedMetaEnv serves one community work ("w", body work) for every lookup.
func generatedMetaEnv(t *testing.T, work string) *testEnv {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/lookup", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"work":{"id":"w","title":"Community Title","authors":[]}}`))
	})
	mux.HandleFunc("GET /api/v1/works/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(work))
	})
	mock := httptest.NewServer(mux)
	t.Cleanup(mock.Close)
	return newTestEnvWith(t, func(c *config.Config) {
		c.Metadata.Enabled = true
		c.Metadata.BaseURL = mock.URL
	})
}

// setGenerated stores generated characters (revealed at chapters 1 and 4) and
// recaps (through 0 and 3, with a summary) for the book at path.
func setGenerated(t *testing.T, e *testEnv, libID int64, path string) {
	t.Helper()
	files := metadata.AttachmentFiles{
		Characters: &model.Characters{Work: "w", Characters: []model.Character{
			{ID: "gen-early", Name: "Early", Reveal: model.Position{Chapter: 1}},
			{ID: "gen-late", Name: "Late", Reveal: model.Position{Chapter: 4}},
		}},
		Recaps: &model.Recaps{Work: "w", Recaps: []model.Recap{
			{Through: model.Position{Chapter: 0}, Text: "Gen before."},
			{Through: model.Position{Chapter: 3}, Text: "Gen three."},
		}, InShort: "Gen short.", Ending: "Gen ending."},
		Provenance: &catalog.AttachmentProvenance{Generator: "audiosilo-bridge", GeneratedAt: "2026-10-15T10:00:00Z"},
	}
	if err := e.cat.SetAttachments(context.Background(), libID, map[string]catalog.AttachmentsReading{path: {Sig: "x", Files: files}}); err != nil {
		t.Fatal(err)
	}
}

// generatedBody is the slice of /meta the generated tests read.
type generatedBody struct {
	Matched bool `json:"matched"`
	Work    *struct {
		ID         string `json:"id"`
		Title      string `json:"title"`
		Characters []struct {
			ID string `json:"id"`
		} `json:"characters"`
		Recaps []struct {
			Text string `json:"text"`
		} `json:"recaps"`
		RecapSummary *struct {
			InShort string `json:"in_short"`
			Ending  string `json:"ending"`
		} `json:"recap_summary"`
		Attribution *json.RawMessage `json:"attribution"`
		Generated   *struct {
			Characters  bool   `json:"characters"`
			Recaps      bool   `json:"recaps"`
			GeneratedAt string `json:"generated_at"`
		} `json:"generated"`
	} `json:"work"`
}

func getGenerated(t *testing.T, e *testEnv, libID int64, path, query, token string) generatedBody {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(libID, 10)+"/meta?path="+escape(path)+query, token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meta %s: %d %s", path, resp.StatusCode, body)
	}
	var g generatedBody
	if err := json.Unmarshal([]byte(body), &g); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return g
}

func ids(g generatedBody) []string {
	var out []string
	for _, c := range g.Work.Characters {
		out = append(out, c.ID)
	}
	return out
}

// TestMetaGeneratedFillsRecaps: a matched work with community characters but no
// recaps takes the book's generated recaps and summary, marked generated; the
// characters stay the community's, with their attribution.
func TestMetaGeneratedFillsRecaps(t *testing.T) {
	t.Parallel()
	e := generatedMetaEnv(t, `{"id":"w","title":"Community Title","authors":[],"language":"en",
		"characters":[{"id":"community-hero","name":"Hero","reveal":{"chapter":1}}]}`)
	lib := seedChapteredBook(t, e, "A/Two", "B0TWO")
	setGenerated(t, e, lib.ID, "A/Two")
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	g := getGenerated(t, e, lib.ID, "A/Two", "", tok)
	if !g.Matched || g.Work == nil || !slices.Equal(ids(g), []string{"community-hero"}) {
		t.Fatalf("characters = %+v", g.Work)
	}
	if len(g.Work.Recaps) != 2 || g.Work.RecapSummary == nil || g.Work.RecapSummary.InShort != "Gen short." {
		t.Errorf("recaps = %+v, summary = %+v", g.Work.Recaps, g.Work.RecapSummary)
	}
	if gen := g.Work.Generated; gen == nil || gen.Characters || !gen.Recaps || gen.GeneratedAt != "2026-10-15T10:00:00Z" {
		t.Errorf("generated = %+v", gen)
	}
	if g.Work.Attribution == nil {
		t.Error("the community characters lost their attribution")
	}
	// The shared cache never took the merge: a book with no generated content of
	// the same work gets the community work alone.
	other := &catalog.Book{LibraryID: lib.ID, RelPath: "A/Copy", Title: "Copy", ASIN: "B0TWO", AddedAt: "2020-01-01"}
	if _, err := e.cat.UpsertBook(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if g := getGenerated(t, e, lib.ID, "A/Copy", "", tok); g.Work.Generated != nil || len(g.Work.Recaps) != 0 {
		t.Errorf("the merge leaked into the cache: %+v", g.Work)
	}
}

// TestMetaGeneratedCommunityWins: a work with community characters and recaps
// ignores the book's generated ones entirely.
func TestMetaGeneratedCommunityWins(t *testing.T) {
	t.Parallel()
	e := generatedMetaEnv(t, `{"id":"w","title":"Community Title","authors":[],"language":"en",
		"characters":[{"id":"community-hero","name":"Hero","reveal":{"chapter":1}}],
		"recaps":[{"through":{"chapter":1},"text":"Community recap."}]}`)
	lib := seedChapteredBook(t, e, "A/Two", "B0TWO")
	setGenerated(t, e, lib.ID, "A/Two")
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	g := getGenerated(t, e, lib.ID, "A/Two", "", tok)
	if !slices.Equal(ids(g), []string{"community-hero"}) || len(g.Work.Recaps) != 1 || g.Work.Recaps[0].Text != "Community recap." ||
		g.Work.RecapSummary != nil || g.Work.Generated != nil || g.Work.Attribution == nil {
		t.Errorf("work = %+v", g.Work)
	}
}

// TestMetaGeneratedUnmatched: a book with no match but generated content answers
// matched false with a work of its own: the book's title, the generated content,
// no attribution; spoilers=hide gates it by the caller's progress. Without
// generated content the answer stays {"matched": false}.
func TestMetaGeneratedUnmatched(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	lib := seedChapteredBook(t, e, "A/Two", "") // no ASIN or ISBN: never matched
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	if _, body := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(lib.ID, 10)+"/meta?path="+escape("A/Two"), tok, ""); body != "{\"matched\":false}\n" {
		t.Fatalf("no generated content = %q", body)
	}
	setGenerated(t, e, lib.ID, "A/Two")

	g := getGenerated(t, e, lib.ID, "A/Two", "", tok)
	if g.Matched || g.Work == nil || g.Work.ID != "" || g.Work.Title != "Two" || len(g.Work.Characters) != 2 ||
		len(g.Work.Recaps) != 2 || g.Work.RecapSummary == nil || g.Work.RecapSummary.Ending != "Gen ending." {
		t.Fatalf("unmatched = %+v", g.Work)
	}
	if g.Work.Attribution != nil {
		t.Error("generated content claims the community's attribution")
	}
	if gen := g.Work.Generated; gen == nil || !gen.Characters || !gen.Recaps {
		t.Errorf("generated = %+v", gen)
	}

	// spoilers=hide: not started, the opening character and the chapter-0 recap
	// only, no summary.
	g = getGenerated(t, e, lib.ID, "A/Two", "&spoilers=hide", tok)
	if !slices.Equal(ids(g), []string{"gen-early"}) || len(g.Work.Recaps) != 1 || g.Work.Recaps[0].Text != "Gen before." ||
		g.Work.RecapSummary != nil || g.Work.Attribution != nil {
		t.Errorf("hidden = %+v", g.Work)
	}
	// Inside chapter 5: everything but the summary.
	if _, err := e.cat.SaveProgress(context.Background(), e.adminID, catalog.Progress{Ref: catalog.Ref{LibraryID: lib.ID, Path: "A/Two"}, Position: 450, Duration: 500}); err != nil {
		t.Fatal(err)
	}
	g = getGenerated(t, e, lib.ID, "A/Two", "&spoilers=hide", tok)
	if len(ids(g)) != 2 || len(g.Work.Recaps) != 2 || g.Work.RecapSummary != nil {
		t.Errorf("chapter 5 = %+v", g.Work)
	}
}

// TestMetaGeneratedSpoilersMatched: spoilers=hide over a matched work whose
// recaps are generated gates them like community recaps, and a work left with
// only generated content carries no attribution.
func TestMetaGeneratedSpoilersMatched(t *testing.T) {
	t.Parallel()
	e := generatedMetaEnv(t, `{"id":"w","title":"Community Title","authors":[],"language":"en",
		"characters":[{"id":"community-late","name":"Late","reveal":{"chapter":5}}]}`)
	lib := seedChapteredBook(t, e, "A/Two", "B0TWO")
	setGenerated(t, e, lib.ID, "A/Two")
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)

	g := getGenerated(t, e, lib.ID, "A/Two", "&spoilers=hide", tok)
	if len(g.Work.Characters) != 0 || len(g.Work.Recaps) != 1 || g.Work.Recaps[0].Text != "Gen before." || g.Work.RecapSummary != nil {
		t.Fatalf("hidden = %+v", g.Work)
	}
	if g.Work.Attribution != nil {
		t.Error("only generated content left, yet attribution stayed")
	}
	if gen := g.Work.Generated; gen == nil || !gen.Recaps || gen.Characters {
		t.Errorf("generated = %+v", gen)
	}
}

// TestMetaGeneratedInvalidAttachmentIgnored: a book whose .audiosilo/ file fails
// its checks has no generated content: /meta answers as if there were none.
func TestMetaGeneratedInvalidAttachmentIgnored(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t, true, 0)
	root := copyFixtures(t, cradlePart)
	dir := filepath.Join(root, "Will Wight", "Cradle", ".audiosilo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "characters.json"), []byte(`{"characters":[{"id":"x","name":"","reveal":{"chapter":1}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "recaps.json"), []byte(`{"recaps":[{"through":{"chapter":-1},"text":"Bad."}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Copy", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	b, err := e.cat.GetBookHolding(context.Background(), lib.ID, cradlePart)
	if err != nil {
		t.Fatal(err)
	}
	if _, body := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(lib.ID, 10)+"/meta?path="+escape(b.RelPath), tok, ""); body != "{\"matched\":false}\n" {
		t.Errorf("invalid attachments = %q, want matched false alone", body)
	}
	// Allowed: once the recaps are valid, the book has a generated work.
	if err := os.WriteFile(filepath.Join(dir, "recaps.json"), []byte(`{"recaps":[{"through":{"chapter":0},"text":"Good."}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	if g := getGenerated(t, e, lib.ID, b.RelPath, "", tok); g.Work == nil || len(g.Work.Recaps) != 1 || len(g.Work.Characters) != 0 {
		t.Errorf("valid recaps = %+v", g.Work)
	}
}
