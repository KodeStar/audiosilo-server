package api

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// TestBookLabelsAPI: a bulk edit adds a tag to each book's own list and takes
// one away; the player's /books filters by tag and genre (any case), /search
// finds a book by a tag, /item and the list carry the new fields, and the admin
// list and facets read them too.
func TestBookLabelsAPI(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, base := seedCatalog(t, e)
	lib := strconv.FormatInt(libID, 10)
	ctx := context.Background()
	if err := e.cat.EditBook(ctx, libID, "Andy Weir/The Martian", catalog.BookEdit{Set: map[string]string{
		catalog.FieldTags: `["Keep"]`, catalog.FieldGenres: `["Science Fiction"]`, catalog.FieldSubtitle: "A Novel",
		catalog.FieldPublisher: "Podium", catalog.FieldLanguage: "English", catalog.FieldAbridged: "false",
	}}); err != nil {
		t.Fatal(err)
	}
	both := `[{"library_id":` + lib + `,"path":"Andy Weir/The Martian"},{"library_id":` + lib + `,"path":"Andy Weir/Artemis"}]`
	if resp, body := e.do(t, "POST", "/api/v1/admin/books/bulk", adminTok,
		`{"books":`+both+`,"add":{"tags":["Owned","Read"]}}`); resp.StatusCode != 200 {
		t.Fatalf("bulk add = %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "POST", "/api/v1/admin/books/bulk", adminTok,
		`{"books":`+both+`,"remove":{"tags":["read"]}}`); resp.StatusCode != 200 {
		t.Fatalf("bulk remove = %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "POST", "/api/v1/admin/books/bulk", adminTok,
		`{"books":`+both+`,"set":{"tags":"[]"},"add":{"tags":["x"]}}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("set and add of one field = %d %s, want 400", resp.StatusCode, body)
	}

	books := func(query string) []string {
		t.Helper()
		resp, body := e.do(t, "GET", "/api/v1/libraries/"+lib+"/books?"+query, adminTok, "")
		if resp.StatusCode != 200 {
			t.Fatalf("books?%s = %d %s", query, resp.StatusCode, body)
		}
		var page struct {
			Books []struct {
				Path string   `json:"rel_path"`
				Tags []string `json:"tags"`
			} `json:"books"`
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range page.Books {
			out = append(out, b.Path+":"+strings.Join(b.Tags, ","))
		}
		slices.Sort(out)
		return out
	}
	if got, want := books("tag=OWNED"), []string{"Andy Weir/Artemis:Owned", "Andy Weir/The Martian:Keep,Owned"}; !slices.Equal(got, want) {
		t.Errorf("tag=OWNED = %v, want %v", got, want)
	}
	if got, want := books("genre=science%20fiction&tag=keep"), []string{"Andy Weir/The Martian:Keep,Owned"}; !slices.Equal(got, want) {
		t.Errorf("genre and tag = %v, want %v", got, want)
	}
	if got := books("tag=read"); got != nil {
		t.Errorf("tag=read = %v, want none (removed)", got)
	}

	_, body := e.do(t, "GET", "/api/v1/search?q=keep", adminTok, "")
	if !strings.Contains(body, `"rel_path":"Andy Weir/The Martian"`) || strings.Contains(body, "Artemis") {
		t.Errorf("search by tag = %s", body)
	}

	resp, body := e.do(t, "GET", "/api/v1/libraries/"+lib+"/item?path="+escape("Andy Weir/The Martian"), adminTok, "")
	if resp.StatusCode != 200 {
		t.Fatalf("item = %d %s", resp.StatusCode, body)
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &item); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"subtitle": `"A Novel"`, "publisher": `"Podium"`, "language": `"English"`, "abridged": `false`,
		"genres": `["Science Fiction"]`, "tags": `["Keep","Owned"]`,
	} {
		if got := string(item[key]); got != want {
			t.Errorf("item %s = %s, want %s", key, got, want)
		}
	}
	// Artemis has none of them: the keys are absent, abridged included (unknown).
	_, body = e.do(t, "GET", "/api/v1/libraries/"+lib+"/item?path="+escape("Andy Weir/Artemis"), adminTok, "")
	for _, key := range []string{`"subtitle"`, `"publisher"`, `"language"`, `"abridged"`, `"genres"`} {
		if strings.Contains(body, key) {
			t.Errorf("Artemis item carries %s: %s", key, body)
		}
	}

	_, body = e.do(t, "GET", "/api/v1/admin/books?tag=keep", adminTok, "")
	if !strings.Contains(body, `"tags":["Keep","Owned"]`) || !strings.Contains(body, `"subtitle":"A Novel"`) || strings.Contains(body, "Artemis") {
		t.Errorf("admin list tag=keep = %s", body)
	}
	_, body = e.do(t, "GET", "/api/v1/admin/books/facets?library_id="+lib, adminTok, "")
	if !strings.Contains(body, `"tags":[{"value":"Owned","count":2},{"value":"Keep","count":1}]`) ||
		!strings.Contains(body, `"genres":[{"value":"Science Fiction","count":1}]`) {
		t.Errorf("facets = %s", body)
	}
	_, body = e.do(t, "GET", base+"/book?path="+escape("Andy Weir/Artemis"), adminTok, "")
	if !strings.Contains(body, `"tags":{"value":"[\"Owned\"]","source":"edited"`) {
		t.Errorf("book page tags provenance = %s", body)
	}
}

// TestExportIncludes: the allowed and denied pairs for ?include=paths,tags: an
// admin's export names each row's paths and carries its tags only when asked
// (each with its own value), a member is refused either way, and an unknown
// include is a 400.
func TestExportIncludes(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, memberTok, _ := adminAndMember(t, e)
	libID, _ := seedCatalog(t, e)
	if err := e.cat.EditBook(context.Background(), libID, "Andy Weir/Artemis", catalog.BookEdit{Set: map[string]string{
		catalog.FieldTags: `["Owned"]`,
	}}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/admin/libraries/" + strconv.FormatInt(libID, 10) + "/export"
	export := func(query string) (paths, tags map[string][]string, body string) {
		t.Helper()
		resp, body := e.do(t, "GET", path+query, adminTok, "")
		if resp.StatusCode != 200 {
			t.Fatalf("export%s = %d %s", query, resp.StatusCode, body)
		}
		var out struct {
			Books []struct {
				Title string   `json:"title"`
				Paths []string `json:"paths"`
				Tags  []string `json:"tags"`
			} `json:"books"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		paths, tags = map[string][]string{}, map[string][]string{}
		for _, b := range out.Books {
			paths[b.Title], tags[b.Title] = b.Paths, b.Tags
		}
		return paths, tags, body
	}

	paths, tags, body := export("?include=paths,tags")
	if !slices.Equal(paths["The Martian"], []string{"Andy Weir/The Martian"}) || !slices.Equal(paths["Artemis"], []string{"Andy Weir/Artemis"}) {
		t.Errorf("paths = %v", paths)
	}
	if !slices.Equal(tags["Artemis"], []string{"Owned"}) || tags["The Martian"] != nil {
		t.Errorf("tags = %v", tags)
	}
	if strings.Contains(body, `"library_id"`) {
		t.Errorf("a row names its library (the envelope does): %s", body)
	}
	if _, _, body := export("?include=paths"); strings.Contains(body, `"tags"`) || !strings.Contains(body, `"paths"`) {
		t.Errorf("include=paths: %s", body)
	}
	if _, _, body := export("?include=tags"); strings.Contains(body, `"paths"`) || !strings.Contains(body, `"tags":["Owned"]`) {
		t.Errorf("include=tags: %s", body)
	}
	// A repeated include counts each value, as the comma list does.
	if _, _, body := export("?include=paths&include=tags"); !strings.Contains(body, `"paths"`) || !strings.Contains(body, `"tags":["Owned"]`) {
		t.Errorf("include=paths&include=tags: %s", body)
	}
	if _, _, body := export(""); strings.Contains(body, `"paths"`) || strings.Contains(body, `"tags"`) ||
		strings.Contains(body, "Owned") || strings.Contains(body, "Andy Weir/") {
		t.Errorf("the default export carries paths or tags: %s", body)
	}
	// The private file is named apart from the shareable one.
	for query, want := range map[string]string{"": `\d\.json"$`, "?include=paths": `-private\.json"$`, "?include=tags": `-private\.json"$`} {
		resp, body := e.do(t, "GET", path+query, adminTok, "")
		if cd := resp.Header.Get("Content-Disposition"); resp.StatusCode != 200 || !regexp.MustCompile(want).MatchString(cd) {
			t.Errorf("export%s: Content-Disposition %q (%d %s), want it to match %s", query, cd, resp.StatusCode, body, want)
		}
	}

	if resp, _ := e.do(t, "GET", path+"?include=paths,tags", memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("member export with paths = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", path+"?include=paths,tags", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous export with paths = %d, want 401", resp.StatusCode)
	}
	for _, q := range []string{"?include=files", "?include=paths,files", "?include=paths,", "?include=Tags"} {
		if resp, _ := e.do(t, "GET", path+q, adminTok, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("export%s = %d, want 400", q, resp.StatusCode)
		}
	}
}
