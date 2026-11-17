package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// A library's metadata source: validated with a code, admin-only, carried by the
// admin list (not the player's), and a change re-resolves the books before the
// answer (a small library: awaitResolve) with no rescan queued.
func TestLibraryMetadataSourceEndpoint(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, memberTok, _ := adminAndMember(t, e)
	ctx := context.Background()
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	const p = "James S. A. Corey/The Expanse/03 - Abaddon's Gate"
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: lib.ID, RelPath: p, IsFolder: true,
		Title: "Abaddon's Gate: Expanse, Book 3", Author: "James S. A. Corey", Series: "The Expanse, book #3",
		AddedAt: "2020-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	url := "/api/v1/admin/libraries/" + strconv.FormatInt(lib.ID, 10)

	if resp, body := e.do(t, "PATCH", url, adminTok, `{"metadata_source":"folders"}`); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body, `"code":"invalid_metadata_source"`) {
		t.Fatalf("bad source = %d %s, want 400 invalid_metadata_source", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "PATCH", url, memberTok, `{"metadata_source":"path"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member PATCH = %d, want 403", resp.StatusCode)
	}
	if b, _ := e.cat.GetBookByPath(ctx, lib.ID, p); b.Title != "Abaddon's Gate: Expanse, Book 3" {
		t.Fatalf("a refused PATCH changed the book: %q", b.Title)
	}

	if resp, body := e.do(t, "PATCH", url, adminTok, `{"metadata_source":"path"}`); resp.StatusCode != 200 ||
		strings.Contains(body, `"job"`) || strings.Contains(body, `"resolving"`) {
		t.Fatalf("source PATCH = %d %s, want 200, no job, and its books resolved", resp.StatusCode, body)
	}
	if _, queued := e.api.scanner.Jobs(); len(queued) != 0 {
		t.Fatalf("a metadata source change queued a scan: %+v", queued)
	}
	if b, _ := e.cat.GetBookByPath(ctx, lib.ID, p); b.Title != "Abaddon's Gate" || b.Series != "The Expanse" || b.SeriesIndex != 3 {
		t.Fatalf("book after the switch = %q / %q #%v, want the path's", b.Title, b.Series, b.SeriesIndex)
	}
	if _, body := e.do(t, "GET", "/api/v1/admin/libraries", adminTok, ""); !strings.Contains(body, `"metadata_source":"path"`) {
		t.Fatalf("admin libraries missing the source: %s", body)
	}
	if _, body := e.do(t, "GET", "/api/v1/libraries", adminTok, ""); strings.Contains(body, "metadata_source") {
		t.Fatalf("the player's library list carries the source: %s", body)
	}
}

// awaitResolve gives a metadata source change's re-resolve as long as the request
// allows, less two seconds to answer in, and returns at once when it ends; a
// re-resolve longer than that goes on after the answer (server#213), which says
// so (the PATCH's resolving).
func TestAwaitResolveBounded(t *testing.T) {
	t.Parallel()
	never := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second+200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if awaitResolve(ctx, never) {
		t.Fatal("a re-resolve still running reported ended")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("waited %v past the request's answer margin", took)
	}
	done := make(chan struct{})
	close(done)
	start = time.Now()
	if !awaitResolve(context.Background(), done) {
		t.Fatal("a re-resolve that had ended reported running")
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("waited %v for a re-resolve that had ended", took)
	}
}

// resolveIdle waits until no re-resolve is running or waiting to be run again.
func resolveIdle(t *testing.T, a *API) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		a.resolveMu.Lock()
		idle := a.resolving == nil && a.resolveRetry == nil
		a.resolveMu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the re-resolve never came to rest")
		}
	}
}

// TestResolveSourcesRetried: a re-resolve that fails leaves its library due, so
// it is run again by itself until one run succeeds; one that can never succeed
// is run again resolveRetries times and then left alone (server#213).
func TestResolveSourcesRetried(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fails int // the runs that fail before one succeeds
		want  int32
	}{
		{"a passing fault", 2, 3},
		{"a library that never resolves", 1 << 30, 1 + resolveRetries},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t)
			var runs atomic.Int32
			e.api.resolveRetryAfter = time.Millisecond
			e.api.resolveRun = func(context.Context) error {
				if int(runs.Add(1)) > tc.fails {
					return nil
				}
				return errors.New("database is locked")
			}
			<-e.api.resolveSources() // the PATCH's own wait ends with the first run
			resolveIdle(t, e.api)
			if got := runs.Load(); got != tc.want {
				t.Fatalf("the re-resolve ran %d times, want %d", got, tc.want)
			}
		})
	}
}

// TestResolveSourcesPanic: a re-resolve that panics is a failed run, retried as
// one, rather than the end of the server (and of every start after it, the
// library still being due).
func TestResolveSourcesPanic(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	var runs atomic.Int32
	e.api.resolveRetryAfter = time.Millisecond
	e.api.resolveRun = func(context.Context) error {
		if runs.Add(1) == 1 {
			panic("a book no resolve expected")
		}
		return nil
	}
	<-e.api.resolveSources()
	resolveIdle(t, e.api)
	if got := runs.Load(); got != 2 {
		t.Fatalf("the re-resolve ran %d times, want the panic and one more", got)
	}
}
