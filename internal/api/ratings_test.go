package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// rateEnv is a test env with the fixture library scanned, "kid" granted only the
// Will Wight subtree (through share kidShare) and "eve" the whole library.
type rateEnv struct {
	*testEnv
	libID          int64
	kidID, eveID   int64
	kidShare       int64
	kidTok, eveTok string
}

func newRateEnv(t *testing.T) *rateEnv {
	t.Helper()
	e := newTestEnv(t)
	lib := newFixtureLibrary(t, e)
	re := &rateEnv{testEnv: e, libID: lib.ID}
	re.kidID, re.kidTok = newUserWithSession(t, e, "kid", false)
	re.eveID, re.eveTok = newUserWithSession(t, e, "eve", false)
	re.kidShare = grantWightOnly(t, e, lib.ID, re.kidID)
	if err := e.cat.GrantWholeLibrary(context.Background(), re.eveID, lib.ID); err != nil {
		t.Fatal(err)
	}
	return re
}

// at is a path-addressed URL in the fixture library (bookURL).
func (e *rateEnv) at(endpoint, path string) string { return bookURL(e.libID, endpoint, path) }

// myRatings decodes GET /me/ratings for tok.
func (e *rateEnv) myRatings(t *testing.T, tok string) []catalog.RatedBook {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/me/ratings", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /me/ratings = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Ratings []catalog.RatedBook `json:"ratings"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Ratings
}

func TestRatingEndpoints(t *testing.T) {
	e := newRateEnv(t)
	if resp, body := e.do(t, "GET", e.at("rating", cradleBook), e.kidTok, ""); resp.StatusCode != http.StatusOK || strings.TrimSpace(body) != `{"rating":null}` {
		t.Fatalf("no rating yet = %d %s", resp.StatusCode, body)
	}

	// Allowed: rating through a part path lands on the book's own path, note trimmed.
	resp, body := e.do(t, "PUT", e.at("rating", cradlePart), e.kidTok, `{"rating":4,"note":"  loved it \n"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT rating = %d %s", resp.StatusCode, body)
	}
	var put struct {
		Rating map[string]any `json:"rating"`
	}
	if err := json.Unmarshal([]byte(body), &put); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(put.Rating))
	for k := range put.Rating {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if strings.Join(keys, ",") != "created_at,library_id,note,path,rating,updated_at" ||
		put.Rating["path"] != cradleBook || put.Rating["rating"] != 4.0 || put.Rating["note"] != "loved it" {
		t.Fatalf("PUT rating shape = %s", body)
	}
	if _, body := e.do(t, "GET", e.at("rating", cradleBook), e.kidTok, ""); !strings.Contains(body, `"rating":4`) {
		t.Fatalf("GET the book's rating = %s", body)
	}
	// GET and DELETE are exact, like progress: the part path holds nothing.
	if _, body := e.do(t, "GET", e.at("rating", cradlePart), e.kidTok, ""); strings.TrimSpace(body) != `{"rating":null}` {
		t.Fatalf("GET the part path = %s, want null", body)
	}

	for _, bad := range []string{`{"rating":0}`, `{"rating":6}`, `{"rating":4.5}`, `{"rating":"4"}`, `{"rating":null}`,
		`{}`, `{"note":"x"}`, `{"rating":3,"bogus":1}`, `not json`,
		`{"rating":3,"note":"` + strings.Repeat("é", 501) + `"}`} {
		if resp, body := e.do(t, "PUT", e.at("rating", cradleBook), e.kidTok, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %.40s = %d %s, want 400", bad, resp.StatusCode, body)
		}
	}
	if resp, _ := e.do(t, "PUT", e.at("rating", cradleBook), e.kidTok, `{"rating":3,"note":"`+strings.Repeat("é", 500)+`"}`); resp.StatusCode != http.StatusOK {
		t.Errorf("a 500-character note = %d, want 200", resp.StatusCode)
	}
	// Not a book (an author folder): 404.
	if resp, body := e.do(t, "PUT", e.at("rating", "Will Wight"), e.kidTok, `{"rating":3}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PUT on a folder = %d %s, want 404", resp.StatusCode, body)
	}

	// Denied: a path outside the caller's grant is 403 for every method, and stores nothing.
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		if resp, body := e.do(t, m, e.at("rating", mistbornBook), e.kidTok, `{"rating":5}`); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s rating outside the grant = %d %s, want 403", m, resp.StatusCode, body)
		}
	}
	if r, _ := e.cat.GetRating(context.Background(), e.kidID, catalog.Ref{LibraryID: e.libID, Path: mistbornBook}); r != nil {
		t.Fatalf("a refused PUT stored a rating: %+v", r)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me/ratings", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /me/ratings = %d, want 401", resp.StatusCode)
	}

	// Delete: idempotent.
	for range 2 {
		if resp, _ := e.do(t, "DELETE", e.at("rating", cradleBook), e.kidTok, ""); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE rating = %d, want 204", resp.StatusCode)
		}
	}
	if _, body := e.do(t, "GET", e.at("rating", cradleBook), e.kidTok, ""); strings.TrimSpace(body) != `{"rating":null}` {
		t.Fatalf("after DELETE = %s", body)
	}
}

// /me/ratings lists only the caller's own ratings, newest first, with the book in
// the list shape, and only on paths the caller can still reach.
func TestMyRatingsOwnAndScoped(t *testing.T) {
	e := newRateEnv(t)
	ctx := context.Background()
	if resp, _ := e.do(t, "PUT", e.at("rating", cradleBook), e.kidTok, `{"rating":5}`); resp.StatusCode != http.StatusOK {
		t.Fatal("kid's rating")
	}
	for _, p := range []string{cradleBook, mistbornBook} {
		if resp, _ := e.do(t, "PUT", e.at("rating", p), e.eveTok, `{"rating":2,"note":"eve"}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("eve's rating of %s", p)
		}
	}
	// A rating on a path not indexed (stored directly) has no book.
	if _, err := e.cat.SetRating(ctx, e.kidID, catalog.Ref{LibraryID: e.libID, Path: "Will Wight/Gone"}, 1, ""); err != nil {
		t.Fatal(err)
	}

	// (Newest first is the catalog's test: these may share a millisecond.)
	kid := e.myRatings(t, e.kidTok)
	slices.SortFunc(kid, func(a, b catalog.RatedBook) int { return strings.Compare(a.Path, b.Path) })
	if len(kid) != 2 || kid[1].Path != "Will Wight/Gone" || kid[1].Book != nil || kid[0].Path != cradleBook ||
		kid[0].Book == nil || kid[0].Book.RelPath != cradleBook || kid[0].Note != "" || kid[0].Stars != 5 {
		t.Fatalf("kid's ratings = %+v", kid)
	}
	// Denied: never another user's ratings.
	for _, r := range kid {
		if r.Note == "eve" {
			t.Fatalf("eve's rating in kid's list: %+v", r)
		}
	}
	if eve := e.myRatings(t, e.eveTok); len(eve) != 2 || eve[0].Note != "eve" || eve[1].Note != "eve" {
		t.Fatalf("eve's ratings = %+v", eve)
	}
	_, body := e.do(t, "GET", "/api/v1/me/ratings", e.kidTok, "")
	if strings.Contains(body, `"description"`) || !strings.Contains(body, `"book":{"id":`) {
		t.Fatalf("book shape = %s", body)
	}

	// Denied: a revoked share hides the rating without deleting it; a re-grant shows it again.
	if err := e.cat.RevokeShare(ctx, e.kidID, e.kidShare); err != nil {
		t.Fatal(err)
	}
	if kid := e.myRatings(t, e.kidTok); len(kid) != 0 {
		t.Fatalf("ratings under a revoked share = %+v, want none", kid)
	}
	if r, _ := e.cat.GetRating(ctx, e.kidID, catalog.Ref{LibraryID: e.libID, Path: cradleBook}); r == nil {
		t.Fatal("revoking the share deleted the rating")
	}
	if err := e.cat.GrantShare(ctx, e.kidID, e.kidShare); err != nil {
		t.Fatal(err)
	}
	if kid := e.myRatings(t, e.kidTok); len(kid) != 2 {
		t.Fatalf("after the re-grant = %+v", kid)
	}

	// A deleted account takes its ratings with it.
	if err := e.auth.DeleteUser(ctx, e.eveID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cradleBook, mistbornBook} {
		if r, err := e.cat.GetRating(ctx, e.eveID, catalog.Ref{LibraryID: e.libID, Path: p}); err != nil || r != nil {
			t.Fatalf("eve's rating of %s after deleting her = %+v %v", p, r, err)
		}
	}
	if kid := e.myRatings(t, e.kidTok); len(kid) != 2 {
		t.Fatalf("deleting eve touched kid's ratings: %+v", kid)
	}
}

// GET /server advertises the user-state capabilities of the player redesign:
// ratings, progress_edit and my_devices (1b), annotations (4).
func TestUserStateCapabilities(t *testing.T) {
	e := newTestEnv(t)
	_, body := e.do(t, "GET", "/api/v1/server", "", "")
	var out struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"ratings", "progress_edit", "my_devices", "annotations"} {
		if !out.Capabilities[c] {
			t.Errorf("capability %s missing: %s", c, body)
		}
	}
}
