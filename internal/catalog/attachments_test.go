package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Book.Generated comes from the attachments signature alone: a book whose
// signature names characters.json or recaps.json has generated content, one
// with only bridge.json or none hasn't (and the field is left off the wire), on
// a single read and on a list alike.
func TestBookGenerated(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	chars := &model.Characters{Characters: []model.Character{{ID: "hero", Name: "Hero"}}}
	for _, b := range []Book{
		{RelPath: "Indexed", Attachments: AttachmentsReading{Sig: "characters.json:10:1,bridge.json:5:1",
			Files: metadata.AttachmentFiles{Characters: chars}}},
		{RelPath: "Refreshed"},
		{RelPath: "Provenance"},
		{RelPath: "None"},
	} {
		b.LibraryID, b.Title, b.AddedAt = lib.ID, b.RelPath, "2024-01-01T00:00:00Z"
		if _, err := c.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetAttachments(ctx, lib.ID, map[string]AttachmentsReading{
		"Refreshed":  {Sig: "recaps.json:7:1", Files: metadata.AttachmentFiles{Recaps: &model.Recaps{InShort: "Short."}}},
		"Provenance": {Sig: "bridge.json:5:1", Files: metadata.AttachmentFiles{Provenance: &metadata.AttachmentProvenance{Kind: "k"}}},
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"Indexed": true, "Refreshed": true, "Provenance": false, "None": false}
	for path, generated := range want {
		b, err := c.GetBookByPath(ctx, lib.ID, path)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(b)
		if b.Generated != generated || strings.Contains(string(raw), `"generated"`) != generated {
			t.Errorf("%s: generated = %v (%s), want %v", path, b.Generated, raw, generated)
		}
	}
	books, err := c.RecentBooks(ctx, []Scope{{LibraryID: lib.ID, AllowAll: true}}, 10)
	if err != nil || len(books) != len(want) {
		t.Fatalf("recent = %v, %v", books, err)
	}
	for _, b := range books {
		if b.Generated != want[b.RelPath] {
			t.Errorf("listed %s: generated = %v", b.RelPath, b.Generated)
		}
	}

	// The stored content is read by the book's id.
	b, _ := c.GetBookByPath(ctx, lib.ID, "Indexed")
	if a, err := c.BookAttachments(ctx, b.ID); err != nil || a == nil || a.Characters == nil || a.Recaps != nil {
		t.Fatalf("attachments = %+v, %v", a, err)
	}
	none, _ := c.GetBookByPath(ctx, lib.ID, "None")
	if a, err := c.BookAttachments(ctx, none.ID); err != nil || a != nil {
		t.Fatalf("no attachments = %+v, %v", a, err)
	}
}
