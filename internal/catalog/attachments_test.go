package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// Book.Generated comes from the attachments signature alone: a book whose
// signature names characters.json, recaps.json or description.json has
// generated content, one with only bridge.json or none hasn't (and the field is left off the wire), on
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
		{RelPath: "Described"},
		{RelPath: "Provenance"},
		{RelPath: "None"},
	} {
		b.LibraryID, b.Title, b.AddedAt = lib.ID, b.RelPath, "2024-01-01T00:00:00Z"
		if _, err := c.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetAttachments(ctx, lib.ID, map[string]AttachmentsReading{
		"Refreshed": {Sig: "recaps.json:7:1", Files: metadata.AttachmentFiles{Recaps: &model.Recaps{InShort: "Short."}}},
		"Described": {Sig: "description.json:300:1,bridge.json:5:1", Files: metadata.AttachmentFiles{
			Description: &model.Description{Work: "w", Text: "A described book.", License: "CC-BY-SA-4.0"},
			Provenance:  &metadata.AttachmentProvenance{Kind: "sidecars.describe", DescriptionAt: "2026-10-16T08:00:00Z"}}},
		"Provenance": {Sig: "bridge.json:5:1", Files: metadata.AttachmentFiles{Provenance: &metadata.AttachmentProvenance{Kind: "k"}}},
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"Indexed": true, "Refreshed": true, "Described": true, "Provenance": false, "None": false}
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
	if a, err := c.BookAttachments(ctx, b.ID, metadata.AllAttachmentParts); err != nil || a == nil || a.Characters == nil || a.Recaps != nil {
		t.Fatalf("attachments = %+v, %v", a, err)
	}
	// A description round-trips, and the console's book page shows it with its
	// own time.
	described, _ := c.GetBookByPath(ctx, lib.ID, "Described")
	if a, err := c.BookAttachments(ctx, described.ID, metadata.AllAttachmentParts); err != nil || a == nil || a.Description == nil ||
		a.Description.Text != "A described book." || a.Characters != nil || a.Provenance.DescriptionAt != "2026-10-16T08:00:00Z" {
		t.Fatalf("described attachments = %+v, %v", a, err)
	}
	d, err := c.AdminBookDetail(ctx, lib.ID, "Described")
	if err != nil || d.GeneratedDescription == nil ||
		*d.GeneratedDescription != (GeneratedDescription{Text: "A described book.", GeneratedAt: "2026-10-16T08:00:00Z"}) {
		t.Fatalf("detail = %+v, %v", d.GeneratedDescription, err)
	}
	if d, err := c.AdminBookDetail(ctx, lib.ID, "Indexed"); err != nil || d.GeneratedDescription != nil {
		t.Fatalf("detail without a description = %+v, %v", d.GeneratedDescription, err)
	} else if raw, _ := json.Marshal(d); strings.Contains(string(raw), "generated_description") {
		t.Errorf("detail without a description carries the field: %s", raw)
	}
	none, _ := c.GetBookByPath(ctx, lib.ID, "None")
	if a, err := c.BookAttachments(ctx, none.ID, metadata.AllAttachmentParts); err != nil || a != nil {
		t.Fatalf("no attachments = %+v, %v", a, err)
	}
}

// A scan's reading (Was: the state it read when it began) never lands over a
// fresher one recorded since (the Bridge's attachments refresh, which writes
// whatever is there): it is skipped. Over the state it read, it lands.
func TestSetAttachmentsCompareAndSet(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	b := Book{LibraryID: lib.ID, RelPath: "Book", Title: "Book", AddedAt: "2024-01-01T00:00:00Z"}
	if _, err := c.UpsertBook(ctx, &b); err != nil {
		t.Fatal(err)
	}
	recaps := func(s string) metadata.AttachmentFiles {
		return metadata.AttachmentFiles{Recaps: &model.Recaps{InShort: s}}
	}
	state := func() AttachmentsState {
		t.Helper()
		sigs, err := c.Signatures(ctx, lib.ID)
		if err != nil {
			t.Fatal(err)
		}
		return AttachmentsState{Sig: sigs["Book"].AttachmentsSig, Unchecked: sigs["Book"].AttachmentsUnchecked}
	}
	short := func() string {
		t.Helper()
		got, _ := c.GetBookByPath(ctx, lib.ID, "Book")
		a, err := c.BookAttachments(ctx, got.ID, metadata.AllAttachmentParts)
		if err != nil || a == nil || a.Recaps == nil {
			t.Fatalf("attachments = %+v, %v", a, err)
		}
		return a.Recaps.InShort
	}
	set := func(r AttachmentsReading) {
		t.Helper()
		if err := c.SetAttachments(ctx, lib.ID, map[string]AttachmentsReading{"Book": r}); err != nil {
			t.Fatal(err)
		}
	}

	scanBegan := state()
	set(AttachmentsReading{Sig: "recaps.json:2:2", Files: recaps("Fresh.")}) // the Bridge's refresh
	set(AttachmentsReading{Sig: "recaps.json:1:1", Files: recaps("Stale."), Was: &scanBegan})
	if got := short(); got != "Fresh." || state().Sig != "recaps.json:2:2" {
		t.Fatalf("after a stale scan write: %q, sig %q; want the refresh kept", got, state().Sig)
	}
	// Over the state it read, a scan's reading lands.
	now := state()
	set(AttachmentsReading{Sig: "recaps.json:3:3", Files: recaps("Newer."), Was: &now})
	if got := short(); got != "Newer." || state().Sig != "recaps.json:3:3" {
		t.Fatalf("after a current scan write: %q, sig %q", got, state().Sig)
	}
}

// BookAttachments reads only the parts asked for (provenance always), and
// BookAttachmentParts says which parts are stored without reading them.
func TestBookAttachmentsParts(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	all := Book{LibraryID: lib.ID, RelPath: "All", Title: "All", AddedAt: "2024-01-01T00:00:00Z",
		Attachments: AttachmentsReading{Sig: "characters.json:1:1,recaps.json:1:1,description.json:1:1,bridge.json:1:1",
			Files: metadata.AttachmentFiles{
				Characters:  &model.Characters{Characters: []model.Character{{ID: "hero", Name: "Hero"}}},
				Recaps:      &model.Recaps{InShort: "Short."},
				Description: &model.Description{Work: "w", Text: "A described book.", License: "CC-BY-SA-4.0"},
				Provenance:  &metadata.AttachmentProvenance{Kind: "sidecars.generate"},
			}}}
	recapsOnly := Book{LibraryID: lib.ID, RelPath: "Recaps", Title: "Recaps", AddedAt: "2024-01-01T00:00:00Z",
		Attachments: AttachmentsReading{Sig: "recaps.json:1:1",
			Files: metadata.AttachmentFiles{Recaps: &model.Recaps{InShort: "Short."}}}}
	none := Book{LibraryID: lib.ID, RelPath: "None", Title: "None", AddedAt: "2024-01-01T00:00:00Z"}
	for _, b := range []*Book{&all, &recapsOnly, &none} {
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	allID, _ := c.GetBookByPath(ctx, lib.ID, "All")
	recapsID, _ := c.GetBookByPath(ctx, lib.ID, "Recaps")
	noneID, _ := c.GetBookByPath(ctx, lib.ID, "None")

	// The description alone: the characters and recaps stored beside it are never read.
	a, err := c.BookAttachments(ctx, allID.ID, metadata.AttachmentParts{Description: true})
	if err != nil || a == nil || a.Description == nil || a.Characters != nil || a.Recaps != nil || a.Provenance == nil {
		t.Fatalf("description only = %+v, %v", a, err)
	}
	// A part the book doesn't have, with no provenance: nothing.
	if a, err := c.BookAttachments(ctx, recapsID.ID, metadata.AttachmentParts{Description: true}); err != nil || a != nil {
		t.Fatalf("a part it lacks = %+v, %v", a, err)
	}

	for _, tc := range []struct {
		id   int64
		want metadata.AttachmentParts
	}{
		{allID.ID, metadata.AllAttachmentParts},
		{recapsID.ID, metadata.AttachmentParts{Recaps: true}},
		{noneID.ID, metadata.AttachmentParts{}},
	} {
		if got, err := c.BookAttachmentParts(ctx, tc.id); err != nil || got != tc.want {
			t.Errorf("parts of %d = %+v, %v; want %+v", tc.id, got, err, tc.want)
		}
	}
}
