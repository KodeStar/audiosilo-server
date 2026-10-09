package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/covercolors"
)

// TestColorCover: the background pass's Colorer reads the colour of a book's
// custom cover and its sidecar art without filling the thumbnail cache, and
// records that a book without art has none.
func TestColorCover(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	libID, _ := seedCovers(t, e)
	ctx := context.Background()
	if err := e.cat.SetCover(ctx, libID, "Custom", bandedPNG(t, 300, 300), e.adminID, catalog.SourceEdited); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.GetLibrary(ctx, libID)
	if err != nil {
		t.Fatal(err)
	}
	due, _, err := e.cat.CoverColorsDue(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	colored := 0
	for _, d := range due {
		rec, err := e.api.colorCover(ctx, lib, d)
		if err != nil {
			t.Fatalf("%s: %v", d.Path, err)
		}
		if rec.Path != d.Path || rec.Art != d.Source.Art {
			t.Fatalf("%s: record %+v, want it for the book's art", d.Path, rec)
		}
		switch d.Path {
		case "Sidecar", "Custom":
			if rec.Color.Bg == "" {
				t.Fatalf("%s: no colour read", d.Path)
			}
			colored++
			art := e.api.coverArt(ctx, lib, d.Path, d.Source)
			for _, size := range thumbSizes {
				if _, ok := e.api.thumbs.Get(thumbKey(libID, d.Path, size, art.version)); ok {
					t.Fatalf("%s: a %dpx thumbnail landed in the cache", d.Path, size)
				}
			}
		default:
			if rec.Color != (catalog.CoverColor{}) {
				t.Fatalf("%s (no art): colour %+v, want none", d.Path, rec.Color)
			}
		}
	}
	if colored != 2 {
		t.Fatalf("read %d colours, want 2 (Sidecar, Custom)", colored)
	}
}

// TestColorCoverMissingFiles: a book whose art files aren't there (an unmounted
// share the scan kept indexed) is ErrArtMissing, not a record of no colour that
// would outlast the share coming back.
func TestColorCoverMissingFiles(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	libID, root := seedCovers(t, e)
	ctx := context.Background()
	if err := os.Remove(filepath.Join(root, "Sidecar", "cover.jpg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "Bare", "book.m4b")); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.GetLibrary(ctx, libID)
	if err != nil {
		t.Fatal(err)
	}
	due, _, err := e.cat.CoverColorsDue(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	missing := 0
	for _, d := range due {
		if d.Path != "Sidecar" && d.Path != "Bare" {
			continue
		}
		if _, err := e.api.colorCover(ctx, lib, d); !errors.Is(err, covercolors.ErrArtMissing) {
			t.Fatalf("%s (files gone): err %v, want ErrArtMissing", d.Path, err)
		}
		missing++
	}
	if missing != 2 {
		t.Fatalf("checked %d books with missing files, want 2", missing)
	}
}

// TestThumbnailColorsOverNone: a book recorded as having no colour for its art
// (the background pass's read failed in passing) takes the colour of a thumbnail
// of that art that does decode.
func TestThumbnailColorsOverNone(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCovers(t, e)
	ctx := context.Background()
	lib, err := e.cat.GetLibrary(ctx, libID)
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := e.cat.CoverSources(ctx, libID, []string{"Sidecar"})
	if err != nil {
		t.Fatal(err)
	}
	src := srcs["Sidecar"]
	art := e.api.coverArt(ctx, lib, "Sidecar", src)
	if err := e.cat.RecordCoverColors(ctx, []catalog.CoverColorRecord{{
		LibraryID: libID, Path: "Sidecar", Art: src.Art, Version: art.version}}); err != nil {
		t.Fatal(err)
	}
	if b, err := e.cat.GetBookByPath(ctx, libID, "Sidecar"); err != nil || b.CoverColor != nil {
		t.Fatalf("recorded none: book %+v, %v; want no colour", b, err)
	}
	if resp, _ := e.do(t, "GET", coverURL(libID, "Sidecar", "160"), adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("thumbnail = %d", resp.StatusCode)
	}
	if b, err := e.cat.GetBookByPath(ctx, libID, "Sidecar"); err != nil || b.CoverColor == nil {
		t.Fatalf("after a thumbnail: book %+v, %v; want its colour", b, err)
	}
}

// TestAdminBooksCoverColor: an admin book row carries its cover colour on the wire.
func TestAdminBooksCoverColor(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCovers(t, e)
	if resp, _ := e.do(t, "GET", coverURL(libID, "Custom", "160"), adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("thumbnail = %d", resp.StatusCode)
	}
	resp, body := e.do(t, "GET", "/api/v1/admin/books?q=Custom", adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin books = %d %s", resp.StatusCode, body)
	}
	var page struct {
		Books []struct {
			CoverColor *catalog.CoverColor `json:"cover_color"`
		} `json:"books"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Books) != 1 || page.Books[0].CoverColor == nil || page.Books[0].CoverColor.Bg == "" {
		t.Fatalf("admin books = %s, want Custom with its cover colour", body)
	}
}
