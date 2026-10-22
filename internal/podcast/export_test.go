package podcast

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/podcast/opml"
)

// exportFixtureRows is export-context.json's followed, visible shows: what
// catalog.PodcastExport selects (hidden and not-followed rows never reach
// BuildExport).
func exportFixtureRows(t *testing.T) []catalog.PodcastExportRow {
	t.Helper()
	raw, err := os.ReadFile("testdata/phase2c3/opml/export-context.json")
	if err != nil {
		t.Fatal(err)
	}
	var ctx struct {
		Shows []struct {
			LibraryID int64  `json:"library_id"`
			Path      string `json:"path"`
			Title     string `json:"title"`
			FeedURL   string `json:"feed_url"`
			Private   bool   `json:"private"`
			Following bool   `json:"following"`
			Visible   bool   `json:"visible"`
		} `json:"shows"`
	}
	if err := json.Unmarshal(raw, &ctx); err != nil {
		t.Fatal(err)
	}
	var rows []catalog.PodcastExportRow
	for _, s := range ctx.Shows {
		if s.Following && s.Visible {
			rows = append(rows, catalog.PodcastExportRow{LibraryID: s.LibraryID, Path: s.Path, Title: s.Title, FeedURL: s.FeedURL, Private: s.Private})
		}
	}
	return rows
}

func TestExportGolden(t *testing.T) {
	rows := exportFixtureRows(t)
	// Reversed input: the winner of a duplicate is decided by library id and
	// path, never input order.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	exp := BuildExport(rows)
	if exp.Exported != 1 || exp.OmittedPrivate != 1 || exp.OmittedUnavailable != 1 {
		t.Fatalf("counts %+v", exp)
	}
	out, err := opml.Render(exp.Entries)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/phase2c3/opml/export-expected.opml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("export differs from the golden:\n%s", out)
	}
	for _, leak := range []string{"Private title", "Hidden", "Not followed", "Duplicate elsewhere", "Missing URL"} {
		if strings.Contains(string(out), leak) {
			t.Fatalf("export names %q", leak)
		}
	}
}

func TestExportOmissions(t *testing.T) {
	exp := BuildExport([]catalog.PodcastExportRow{
		{LibraryID: 1, Path: "b", Title: "B", FeedURL: "https://example.org/b.xml"},
		{LibraryID: 1, Path: "a", Title: "A", FeedURL: "https://example.org/a.xml"},
		// A public row whose URL now looks private is left out as private.
		{LibraryID: 1, Path: "c", Title: "C", FeedURL: "https://example.org/c.xml?token=SECRET"},
		// An unusable URL is unavailable.
		{LibraryID: 1, Path: "d", Title: "D", FeedURL: "file:///etc/passwd"},
		// A private show with an accidental URL in the index is still private.
		{LibraryID: 2, Path: "e", Title: "E", FeedURL: "https://example.org/e.xml", Private: true},
		// The same feed in another library: counted once, the lower library's title.
		{LibraryID: 2, Path: "a", Title: "Other A", FeedURL: "HTTPS://Example.org/a.xml"},
	})
	if exp.Exported != 2 || exp.OmittedPrivate != 2 || exp.OmittedUnavailable != 1 {
		t.Fatalf("counts %+v", exp)
	}
	if exp.Entries[0].URL != "https://example.org/a.xml" || exp.Entries[0].Title != "A" || exp.Entries[1].URL != "https://example.org/b.xml" {
		t.Fatalf("entries %+v", exp.Entries)
	}
	empty := BuildExport(nil)
	if empty.Entries == nil || empty.Exported != 0 {
		t.Fatal("an empty export")
	}
}
