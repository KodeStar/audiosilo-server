package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// librivox.fetch: strict params built by the server, a writing job (a books
// library the Bridge can write to), internal and notified, one open per
// library and project; its family is the librivox feature.
func TestLibrivoxFetchJobs(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t)
	ctx := context.Background()
	p := LibrivoxFetchParams{LibrivoxID: 192, WorkID: "johnny-crows-garden", RecordingID: "lv192", LibraryID: q.lib}
	if _, _, err := q.Enqueue(ctx, LibrivoxFetch(p, q.admin)); !errors.Is(err, ErrKindUnavailable) {
		t.Fatalf("no Bridge offering it: %v", err)
	}
	q.beatWritable(t, false, KindVerify, KindLibrivoxFetch)
	if _, _, err := q.Enqueue(ctx, LibrivoxFetch(p, q.admin)); !errors.Is(err, ErrLibraryNotWritable) {
		t.Fatalf("not writable: %v", err)
	}
	q.beatWritable(t, true, KindVerify, KindLibrivoxFetch)
	first, created, err := q.Enqueue(ctx, LibrivoxFetch(p, q.admin))
	if err != nil || !created || *first.LibraryID != q.lib || first.Path != nil || first.RequestedBy == nil || first.RequestedBy.ID != q.admin {
		t.Fatalf("queue = %+v, %t, %v", first, created, err)
	}
	if string(first.Params) != `{"librivox_id":192,"work_id":"johnny-crows-garden","recording_id":"lv192","library_id":`+itoa(q.lib)+`}` {
		t.Fatalf("params = %s", first.Params)
	}
	again, created, err := q.Enqueue(ctx, LibrivoxFetch(p, q.admin))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("dedupe = %+v, %t, %v", again, created, err)
	}
	other := p
	other.LibrivoxID = 193
	second, created, err := q.Enqueue(ctx, LibrivoxFetch(other, q.admin))
	if err != nil || !created || second.ID == first.ID {
		t.Fatalf("another project = %+v, %t, %v", second, created, err)
	}
	spec := kinds[KindLibrivoxFetch]
	if !spec.internal || spec.quiet || ModuleFor(KindLibrivoxFetch, nil) != "" || !spec.writes || spec.libraryKind != libraryBooks {
		t.Fatal("librivox.fetch: internal, notified, no module, a books library it writes to")
	}
	if got := (Status{Kinds: []string{KindLibrivoxFetch}}).Features(); !slices.Equal(got, []string{FeatureLibrivox}) {
		t.Fatalf("features = %v", got)
	}

	open, err := q.OpenLibrivoxFetches(ctx, []int{192, 193, 194})
	if err != nil || len(open) != 2 || open[192].ID != first.ID || open[193].ID != second.ID {
		t.Fatalf("open = %+v, %v", open, err)
	}
	if _, err := q.Cancel(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if open, err := q.OpenLibrivoxFetches(ctx, []int{192}); err != nil || len(open) != 0 {
		t.Fatalf("a cancelled fetch is still open: %+v, %v", open, err)
	}
	if open, err := q.OpenLibrivoxFetches(ctx, nil); err != nil || open == nil || len(open) != 0 {
		t.Fatalf("no projects = %#v, %v", open, err)
	}
	// A fetch whose lease ran out on its last attempt has failed: the read
	// settles it first, as the other reads of the open jobs do.
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if j, err := q.claim(ctx, q.token, []string{KindLibrivoxFetch}); err != nil || j == nil || j.ID != second.ID {
			t.Fatalf("claim %d = %+v, %v", attempt, j, err)
		}
		q.advance(Lease + time.Second)
	}
	if open, err := q.OpenLibrivoxFetches(ctx, []int{193}); err != nil || len(open) != 0 {
		t.Fatalf("a fetch whose last lease ran out is still open: %+v, %v", open, err)
	}

	for name, raw := range map[string]string{
		"unknown field":     `{"librivox_id":1,"work_id":"w","recording_id":"r","library_id":1,"x":1}`,
		"no project":        `{"work_id":"w","recording_id":"r","library_id":1}`,
		"negative project":  `{"librivox_id":-1,"work_id":"w","recording_id":"r","library_id":1}`,
		"no work":           `{"librivox_id":1,"recording_id":"r","library_id":1}`,
		"no recording":      `{"librivox_id":1,"work_id":"w","library_id":1}`,
		"blank recording":   `{"librivox_id":1,"work_id":"w","recording_id":"  ","library_id":1}`,
		"no library":        `{"librivox_id":1,"work_id":"w","recording_id":"r"}`,
		"control character": `{"librivox_id":1,"work_id":"w\n","recording_id":"r","library_id":1}`,
		"long recording":    `{"librivox_id":1,"work_id":"w","recording_id":"` + strings.Repeat("r", maxWorkIDLen+1) + `","library_id":1}`,
		// A community id is a slug (model.ValidSlug, as contribute and upcoming
		// check it, server#386): lower-case kebab, at most 100 bytes.
		"recording no slug":   `{"librivox_id":1,"work_id":"w","recording_id":"rec 1","library_id":1}`,
		"work no slug":        `{"librivox_id":1,"work_id":"-w","recording_id":"r","library_id":1}`,
		"recording slash":     `{"librivox_id":1,"work_id":"w","recording_id":"a/b","library_id":1}`,
		"recording upper":     `{"librivox_id":1,"work_id":"w","recording_id":"LV192","library_id":1}`,
		"recording dot":       `{"librivox_id":1,"work_id":"w","recording_id":"lv.192","library_id":1}`,
		"work underscore":     `{"librivox_id":1,"work_id":"the_martian","recording_id":"r","library_id":1}`,
		"work trailing dash":  `{"librivox_id":1,"work_id":"w-","recording_id":"r","library_id":1}`,
		"recording 101 bytes": `{"librivox_id":1,"work_id":"w","recording_id":"` + strings.Repeat("r", 101) + `","library_id":1}`,
		"long title":          `{"librivox_id":1,"work_id":"w","recording_id":"r","library_id":1,"title":"` + strings.Repeat("t", maxLibrivoxTitle+1) + `"}`,
		"title line break":    `{"librivox_id":1,"work_id":"w","recording_id":"r","library_id":1,"title":"a\nb"}`,
	} {
		if _, err := librivoxFetchParams(json.RawMessage(raw)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, raw := range map[string]string{
		"no title":   `{"librivox_id":1,"work_id":"w","recording_id":"r","library_id":1}`,
		"with title": `{"librivox_id":1,"work_id":"the-martian","recording_id":"the-martian-lv-47","library_id":1,"title":"The Martian"}`,
		"100 bytes":  `{"librivox_id":1,"work_id":"w","recording_id":"` + strings.Repeat("r", 100) + `","library_id":1}`,
	} {
		if _, err := librivoxFetchParams(json.RawMessage(raw)); err != nil {
			t.Errorf("%s: %v, want accepted", name, err)
		}
	}
	// LibrivoxFetch keeps a title to one bounded line, so the params it builds pass.
	j := LibrivoxFetch(LibrivoxFetchParams{LibrivoxID: 1, WorkID: "w", RecordingID: "r", LibraryID: 1,
		Title: " A\nlong " + strings.Repeat("é", maxLibrivoxTitle) + " "}, 1)
	var got LibrivoxFetchParams
	if err := json.Unmarshal(j.Params, &got); err != nil || len(got.Title) > maxLibrivoxTitle || !strings.HasPrefix(got.Title, "A long é") {
		t.Fatalf("title = %q (%d bytes), %v", got.Title, len(got.Title), err)
	}
	if _, err := librivoxFetchParams(j.Params); err != nil {
		t.Fatalf("LibrivoxFetch's params refused: %v", err)
	}
}

// TestLibrivoxFetchFolder (server#386): a result's Adopted read as its three
// states, from the JSON a Bridge sends.
func TestLibrivoxFetchFolder(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]FetchFolder{
		`{"path":"p"}`:                 FolderUnsaid,
		`{"path":"p","adopted":false}`: FolderPlaced,
		`{"path":"p","adopted":true}`:  FolderAdopted,
	} {
		var r LibrivoxFetchResult
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		if got := r.Folder(); got != want {
			t.Errorf("%s: Folder = %d, want %d", raw, got, want)
		}
	}
}
