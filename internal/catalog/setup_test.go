package catalog

import (
	"errors"
	"maps"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// A fresh server's setup state is empty and not finished; an update merges what
// it names into what is stored, and an unknown step or outcome stores nothing.
func TestSetupState(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	st, err := c.SetupState(ctx)
	if err != nil || st.Finished || st.Dismissed || st.Steps == nil || len(st.Steps) != 0 {
		t.Fatalf("fresh state = %+v, %v", st, err)
	}
	if _, err := c.UpdateSetupState(ctx, SetupPatch{Steps: map[string]string{"folder": SetupStepDone}}); err != nil {
		t.Fatal(err)
	}
	yes := true
	st, err = c.UpdateSetupState(ctx, SetupPatch{Finished: &yes, Steps: map[string]string{"naming": SetupStepSkipped}})
	if err != nil {
		t.Fatal(err)
	}
	// Finishing marks every step left unmarked skipped, beside the two named.
	if !st.Finished || st.Dismissed || len(st.Steps) != len(SetupSteps) || st.Steps["folder"] != SetupStepDone || st.Steps["naming"] != SetupStepSkipped || st.Steps["phone"] != SetupStepSkipped {
		t.Fatalf("merged state = %+v", st)
	}
	for _, steps := range []map[string]string{{"wizard": SetupStepDone}, {"folder": "later"}, {"bridge": ""}} {
		if _, err := c.UpdateSetupState(ctx, SetupPatch{Dismissed: &yes, Steps: steps}); !errors.Is(err, ErrInvalidSetupStep) {
			t.Errorf("steps %v = %v, want ErrInvalidSetupStep", steps, err)
		}
	}
	if got, _ := c.SetupState(ctx); got.Dismissed || len(got.Steps) != len(SetupSteps) {
		t.Errorf("a refused update stored something: %+v", got)
	}
}

// The patch that finishes the flow marks the steps it left unmarked (server#183):
// the folder step done once a books library exists, the rest skipped, and a step
// another tab marked keeps its own. Before a books library (a podcast one holds
// no books) the folder step is skipped too; a later finish (the flow already
// finished) fills nothing.
func TestSetupFinishFillsUnmarkedSteps(t *testing.T) {
	t.Parallel()
	yes := true
	finish := func(c *Catalog, steps map[string]string) SetupState {
		t.Helper()
		st, err := c.UpdateSetupState(t.Context(), SetupPatch{Finished: &yes, Steps: steps})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	t.Run("with a books library", func(t *testing.T) {
		t.Parallel()
		c, ctx := newTestCatalog(t)
		if _, err := c.CreateLibrary(ctx, Library{Name: "Books", Root: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
		// Another tab recorded the backups step done; this one leaves the phone step.
		if _, err := c.UpdateSetupState(ctx, SetupPatch{Steps: map[string]string{"account": SetupStepDone, "backups": SetupStepDone}}); err != nil {
			t.Fatal(err)
		}
		st := finish(c, map[string]string{"phone": SetupStepDone})
		want := map[string]string{
			"account": SetupStepDone, "folder": SetupStepDone, "naming": SetupStepSkipped,
			"metadata": SetupStepSkipped, "reach": SetupStepSkipped, "abs": SetupStepSkipped,
			"backups": SetupStepDone, "bridge": SetupStepSkipped, "phone": SetupStepDone,
		}
		if !st.Finished || !maps.Equal(st.Steps, want) {
			t.Fatalf("finished state = %+v, want steps %v", st, want)
		}
		if got, _ := c.SetupState(ctx); !maps.Equal(got.Steps, want) {
			t.Errorf("stored steps = %v, want %v", got.Steps, want)
		}
	})

	t.Run("podcasts only", func(t *testing.T) {
		t.Parallel()
		c, ctx := newTestCatalog(t)
		if _, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts}); err != nil {
			t.Fatal(err)
		}
		if st := finish(c, map[string]string{"phone": SetupStepSkipped}); st.Steps["folder"] != SetupStepSkipped {
			t.Errorf("folder = %q with only a podcast library, want skipped", st.Steps["folder"])
		}
	})

	t.Run("already finished", func(t *testing.T) {
		t.Parallel()
		c, ctx := newTestCatalog(t)
		// A server marked finished with no marks (0053, an upgraded server): a
		// finish sent again records only what it names.
		if err := putServerState(ctx, c.db, setupKey, SetupState{Finished: true, Steps: map[string]string{}}, c.stamp()); err != nil {
			t.Fatal(err)
		}
		if st := finish(c, map[string]string{"bridge": SetupStepDone}); len(st.Steps) != 1 {
			t.Errorf("steps = %v, want only bridge", st.Steps)
		}
	})
}

// setup.skip (server#184): a fresh flow is finished and dismissed, the folder
// step done once a books library exists, a step the admin marked keeping its
// outcome; a flow already finished is left as it is (an admin's reopened card
// survives every later start).
func TestSkipSetup(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	if _, err := c.CreateLibrary(ctx, Library{Name: "Books", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateSetupState(ctx, SetupPatch{Steps: map[string]string{"backups": SetupStepDone}}); err != nil {
		t.Fatal(err)
	}
	skipped, err := c.SkipSetup(ctx)
	if err != nil || !skipped {
		t.Fatalf("SkipSetup = %v, %v; want true", skipped, err)
	}
	st, err := c.SetupState(ctx)
	if err != nil || !st.Finished || !st.Dismissed || len(st.Steps) != len(SetupSteps) ||
		st.Steps["folder"] != SetupStepDone || st.Steps["backups"] != SetupStepDone || st.Steps["phone"] != SetupStepSkipped {
		t.Fatalf("state after SkipSetup = %+v, %v", st, err)
	}

	// The admin reopens the card; the next start's SkipSetup changes nothing.
	no := false
	if _, err := c.UpdateSetupState(ctx, SetupPatch{Dismissed: &no}); err != nil {
		t.Fatal(err)
	}
	if skipped, err := c.SkipSetup(ctx); err != nil || skipped {
		t.Fatalf("SkipSetup on a finished flow = %v, %v; want false", skipped, err)
	}
	if st, _ := c.SetupState(ctx); st.Dismissed || !st.Finished {
		t.Errorf("a finished flow was changed: %+v", st)
	}
}

// The preview's root check refuses exactly what a books library create refuses
// (a root overlapping a podcast library, either way round), allows what it allows
// (a books root beside or inside another books root), and names a library already
// at exactly this root.
func TestCheckLibraryRoot(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	base := t.TempDir()
	if _, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: filepath.Join(base, "pods"), Kind: KindPodcasts}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateLibrary(ctx, Library{Name: "Books", Root: filepath.Join(base, "books")}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		root, same string
		err        error
	}{
		{root: filepath.Join(base, "pods"), err: ErrRootOverlaps},
		{root: filepath.Join(base, "pods", "inner"), err: ErrRootOverlaps},
		{root: base, err: ErrRootOverlaps},
		{root: filepath.Join(base, "books"), same: "Books"},
		{root: filepath.Join(base, "books") + "/", same: "Books"},
		{root: filepath.Join(base, "books", "more")},
		{root: filepath.Join(base, "other")},
	} {
		same, err := c.CheckLibraryRoot(ctx, tc.root)
		if !errors.Is(err, tc.err) || same != tc.same {
			t.Errorf("CheckLibraryRoot(%q) = %q, %v; want %q, %v", tc.root, same, err, tc.same, tc.err)
		}
		_, createErr := c.CreateLibrary(ctx, Library{Name: "New " + tc.root, Root: tc.root})
		if (createErr != nil) != (tc.err != nil) {
			t.Errorf("create at %q = %v, but the check said %v", tc.root, createErr, tc.err)
		}
	}
}

// ResolveScanned names a book exactly as a stored one in a library of each
// metadata source shows it (the effective values after a scan, with no
// enrichment or edits), an Audiobookshelf metadata.json included.
func TestResolveScannedMatchesStoredBook(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	book := func() *Book {
		return &Book{
			RelPath: "Somebody Else/Some Series/03 - Other Title", IsFolder: true,
			Title: "Unsouled", Author: "Will Wight", Narrator: "Reader", Series: "Some Series", SeriesIndex: 3,
			Files: []BookFile{{RelPath: "Somebody Else/Some Series/03 - Other Title/01.m4b"}},
		}
	}
	withABS := func() *Book {
		b := book()
		b.RelPath = "Author X/Book Y"
		b.Files = []BookFile{{RelPath: "Author X/Book Y/a.mp3"}}
		b.Title, b.Author, b.Series, b.SeriesIndex = "Book Y", "Author X", "", 0
		b.ABS = ABSReading{Sig: "s", File: &metadata.ABSFile{Author: "Abs Author"}}
		return b
	}
	for _, source := range []string{MetadataFromTags, MetadataFromPath} {
		lib, err := c.CreateLibrary(ctx, Library{Name: source, Root: "/" + source, MetadataSource: source})
		if err != nil {
			t.Fatal(err)
		}
		for _, mk := range []func() *Book{book, withABS} {
			b := mk()
			b.LibraryID = lib.ID
			want := ResolveScanned(mk(), source)
			if _, err := c.UpsertBook(ctx, b); err != nil {
				t.Fatal(err)
			}
			got, err := c.GetBookByPath(ctx, lib.ID, b.RelPath)
			if err != nil {
				t.Fatal(err)
			}
			stored := ScannedNames{Title: got.Title, Author: got.Author, Narrator: got.Narrator, Series: got.Series,
				Position: FormatSeriesRange(got.SeriesIndex, got.SeriesIndexEnd)}
			if stored != want {
				t.Errorf("%s %s: stored %+v, ResolveScanned %+v", source, b.RelPath, stored, want)
			}
		}
	}
	tags, path := ResolveScanned(book(), MetadataFromTags), ResolveScanned(book(), MetadataFromPath)
	if tags.Title != "Unsouled" || tags.Author != "Will Wight" || path.Title != "Other Title" || path.Author != "Somebody Else" {
		t.Errorf("the two sources should disagree: tags %+v, path %+v", tags, path)
	}
}
