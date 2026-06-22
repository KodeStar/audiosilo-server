package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// TestReorderLibrariesEndpoint covers the admin reorder route: a non-admin is
// denied, and an admin's ordering persists (and is what ListLibraries returns).
func TestReorderLibrariesEndpoint(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	libA, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "A", Root: t.TempDir()})
	libB, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "B", Root: t.TempDir()})

	member, _ := e.auth.CreateUser(ctx, "member", "member-password", auth.RoleUser)
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)
	body := `{"ids":[` + itoa(libB.ID) + `,` + itoa(libA.ID) + `]}`
	if resp, _ := e.do(t, "PUT", "/api/v1/admin/libraries/order", memberTok, body); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin reorder = %d, want 403", resp.StatusCode)
	}

	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	resp, out := e.do(t, "PUT", "/api/v1/admin/libraries/order", adminTok, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin reorder = %d %s, want 200", resp.StatusCode, out)
	}
	var got struct {
		Libraries []catalog.Library `json:"libraries"`
	}
	json.Unmarshal([]byte(out), &got)
	if len(got.Libraries) != 2 || got.Libraries[0].ID != libB.ID || got.Libraries[1].ID != libA.ID {
		t.Fatalf("reorder response not in new order: %+v", got.Libraries)
	}
	libs, _ := e.cat.ListLibraries(ctx)
	if libs[0].ID != libB.ID || libs[1].ID != libA.ID {
		t.Fatalf("persisted order wrong: %+v", libs)
	}
}

// TestSearchDedupWire verifies the de-duplicated search result reaches the client
// with dedup_key set and the duplicate copy under other_locations.
func TestSearchDedupWire(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	libA, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "A", Root: t.TempDir()})
	libB, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "B", Root: t.TempDir()})
	_ = e.cat.ReorderLibraries(ctx, []int64{libA.ID, libB.ID})
	for _, lib := range []int64{libA.ID, libB.ID} {
		if _, err := e.cat.UpsertBook(ctx, &catalog.Book{
			LibraryID: lib, RelPath: "hobbit.m4b", Title: "The Hobbit", Author: "Tolkien",
			Format: "m4b", Size: 1000, Duration: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}

	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	resp, out := e.do(t, "GET", "/api/v1/search?q=hobbit", adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search = %d %s", resp.StatusCode, out)
	}
	var got struct {
		Books []catalog.Book `json:"books"`
	}
	json.Unmarshal([]byte(out), &got)
	if len(got.Books) != 1 {
		t.Fatalf("expected 1 deduped book, got %d: %s", len(got.Books), out)
	}
	b := got.Books[0]
	if b.DedupKey == "" {
		t.Fatalf("expected dedup_key on the wire: %s", out)
	}
	if b.LibraryID != libA.ID || len(b.OtherLocations) != 1 || b.OtherLocations[0].LibraryID != libB.ID {
		t.Fatalf("unexpected winner/other_locations: %+v", b)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
