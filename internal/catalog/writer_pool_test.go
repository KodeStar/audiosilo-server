package catalog

import (
	"context"
	"testing"
)

// fileCatalogWithBook is a file-backed catalog (as newTestCatalog's always is)
// holding one indexed book and one user.
func fileCatalogWithBook(t *testing.T) (*Catalog, context.Context, int64, Ref) {
	t.Helper()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "A/One", Title: "One", Duration: 100, AddedAt: "x"}); err != nil {
		t.Fatal(err)
	}
	return c, ctx, seedUser(t, c, ctx), Ref{LibraryID: lib.ID, Path: "A/One"}
}

// SaveProgress and SetRating upsert with RETURNING, a write that reads a row
// back: on the reader pool it failed with "attempt to write a readonly database".
func TestSaveProgressOnFileDatabase(t *testing.T) {
	c, ctx, uid, ref := fileCatalogWithBook(t)
	p, err := c.SaveProgress(ctx, uid, Progress{Ref: ref, Position: 10, Duration: 100, Version: 1})
	if err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}
	if p.Position != 10 || p.StartedAt == "" || p.FinishedAt != "" {
		t.Fatalf("SaveProgress = %+v", p)
	}
	p, err = c.SaveProgress(ctx, uid, Progress{Ref: ref, Position: 100, Duration: 100, Finished: true, Version: 2})
	if err != nil {
		t.Fatalf("SaveProgress (finish): %v", err)
	}
	if !p.Finished || p.FinishedAt == "" {
		t.Fatalf("SaveProgress (finish) = %+v", p)
	}
}

func TestSetRatingOnFileDatabase(t *testing.T) {
	c, ctx, uid, ref := fileCatalogWithBook(t)
	r, err := c.SetRating(ctx, uid, ref, 4, " good ")
	if err != nil {
		t.Fatalf("SetRating: %v", err)
	}
	if r.Stars != 4 || r.Note != "good" {
		t.Fatalf("SetRating = %+v", r)
	}
	r, err = c.SetRating(ctx, uid, ref, 2, "")
	if err != nil {
		t.Fatalf("SetRating (update): %v", err)
	}
	if got, err := c.GetRating(ctx, uid, ref); err != nil || got == nil || got.Stars != 2 || *got != *r {
		t.Fatalf("GetRating = %+v, %v; SetRating echoed %+v", got, err, r)
	}
}

// TestUserStateWritesUseTheWriter runs the per-user writes on a file-backed
// database, whose reader pool is query_only: a write issued through the reader
// (QueryRowContext with INSERT ... RETURNING, say) fails there with "attempt to
// write a readonly database", which an in-memory test can't see.
func TestUserStateWritesUseTheWriter(t *testing.T) {
	c, ctx, uid, ref := fileCatalogWithBook(t)
	scopes := []Scope{{LibraryID: ref.LibraryID, AllowAll: true}}

	p, err := c.SaveProgress(ctx, uid, Progress{Ref: ref, Position: 10, Duration: 100, Version: 1})
	if err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}
	if p.StartedAt == "" {
		t.Fatalf("SaveProgress echo has no start date: %+v", p)
	}
	done := true
	if _, err := c.EditProgress(ctx, uid, ref, ProgressEdit{Finished: &done}, scopes[0]); err != nil {
		t.Fatalf("EditProgress: %v", err)
	}

	if _, err := c.SetRating(ctx, uid, ref, 5, ""); err != nil {
		t.Fatalf("SetRating: %v", err)
	}
	if err := c.DeleteRating(ctx, uid, ref); err != nil {
		t.Fatalf("DeleteRating: %v", err)
	}

	if err := c.SetListeningGoal(ctx, uid, 12); err != nil {
		t.Fatalf("SetListeningGoal: %v", err)
	}
	if err := c.DeleteListeningGoal(ctx, uid); err != nil {
		t.Fatalf("DeleteListeningGoal: %v", err)
	}

	if err := c.AddToQueue(ctx, uid, ref, nil); err != nil {
		t.Fatalf("AddToQueue: %v", err)
	}
	if err := c.SetQueue(ctx, uid, []Ref{ref}, scopes); err != nil {
		t.Fatalf("SetQueue: %v", err)
	}
	if err := c.RemoveFromQueue(ctx, uid, ref); err != nil {
		t.Fatalf("RemoveFromQueue: %v", err)
	}

	col, err := c.CreateCollection(ctx, uid, "Mine", "")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	name := "Renamed"
	if err := c.UpdateCollection(ctx, col.ID, uid, &name, nil); err != nil {
		t.Fatalf("UpdateCollection: %v", err)
	}
	if err := c.AddCollectionItem(ctx, col.ID, uid, ref, nil); err != nil {
		t.Fatalf("AddCollectionItem: %v", err)
	}
	if err := c.SetCollectionItems(ctx, col.ID, uid, []Ref{ref}, scopes); err != nil {
		t.Fatalf("SetCollectionItems: %v", err)
	}
	if err := c.SetCollectionShares(ctx, col.ID, uid, nil); err != nil {
		t.Fatalf("SetCollectionShares: %v", err)
	}
	if err := c.RemoveCollectionItem(ctx, col.ID, uid, ref); err != nil {
		t.Fatalf("RemoveCollectionItem: %v", err)
	}
	if err := c.DeleteCollection(ctx, col.ID, uid); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}
}
