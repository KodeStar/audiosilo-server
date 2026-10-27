package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// The perf fixture (server#243): a library of realistic shape seeded through the
// catalog's own write path (UpsertBook, as a scan writes it), and the requests
// the benchmark harness replays against it (.perf-parity/bench/harness: the home
// shelf, the library grid's first page, a deep cursor page, and search for a
// common word, an author and a title).
//
//	go test ./internal/api -run '^$' -bench BenchmarkPerfAPI -benchtime 2s
//	AUDIOSILO_PERF_BOOKS=1000 go test ./internal/api -run '^$' -bench BenchmarkPerfAPI

// perfEnv is a seeded API: its handler, an admin session and the library.
type perfEnv struct {
	api     *API
	handler http.Handler
	token   string
	libID   int64
	cat     *catalog.Catalog
	db      *store.DB
}

var perfWords = []string{
	"Garden", "River", "House", "Secret", "Island", "Shadow", "Winter", "Journey", "Kingdom", "Letters",
	"Mystery", "Voyage", "Summer", "Night", "Stone", "Forest", "Castle", "Mountain", "Ocean", "Daughter",
	"Captain", "Window", "Silver", "Golden", "Empire", "Prince", "Widow", "Tales", "Poems", "Stories",
	"Adventures", "History", "Memoirs", "Essays", "Thunder", "Lantern", "Harbour", "Valley", "Meadow", "Tower",
}

// perfThe are title words that share the prefix "the" without being it, so a
// prefix query for "the" expands to several terms, as in a real catalogue.
var perfThe = []string{"Theory", "Them", "Theatre", "Thessaly", "Theodore", "Thermopylae"}

var perfGiven = []string{"Charles", "Jane", "Mark", "Mary", "George", "Louisa", "Arthur", "Edith", "Henry", "Emily",
	"Thomas", "Anne", "Walter", "Virginia", "Robert", "Elizabeth", "Joseph", "Willa", "Oscar", "Kate"}
var perfFamily = []string{"Austen", "Twain", "Shelley", "Eliot", "Alcott", "Doyle", "Wharton", "James", "Bronte",
	"Hardy", "Trollope", "Scott", "Woolf", "Stevenson", "Gaskell", "Conrad", "Cather", "Wilde", "Chopin",
	"Collins", "Kipling", "Wells", "London", "Hawthorne", "Melville", "Poe", "Burnett", "Baum", "Verne", "Dumas"}

// seedPerfLibrary indexes n books into a new library: about 70% folders of MP3
// parts (one chapter per file, 3-40 files), the rest single M4B files with
// 8-40 chapters; about 800 authors, 300 narrators and 600 series (40% of books
// are in one); about a third of the titles hold the word "the"; Charles Dickens
// has 14 books, Bleak House among them. Deterministic for a given n.
func seedPerfLibrary(tb testing.TB, n int) *perfEnv {
	tb.Helper()
	ctx := context.Background()
	db := storetest.Open(tb)
	authSvc := auth.New(db, time.Now)
	cat := catalog.New(db, time.Now)
	admin, err := authSvc.CreateUser(ctx, "admin", "admin-password", auth.RoleAdmin)
	if err != nil {
		tb.Fatal(err)
	}
	root := tb.TempDir()
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Books", Root: root})
	if err != nil {
		tb.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(243, uint64(n)))
	pick := func(s []string) string { return s[rng.IntN(len(s))] }
	authors := make([]string, 800)
	for i := range authors {
		authors[i] = fmt.Sprintf("%s %s", pick(perfGiven), pick(perfFamily))
		if i%3 == 0 {
			authors[i] += fmt.Sprintf(" %c.", 'A'+rune(i%26))
		}
	}
	narrators := make([]string, 300)
	for i := range narrators {
		narrators[i] = fmt.Sprintf("%s %s", pick(perfGiven), pick(perfFamily))
	}
	series := make([]string, 600)
	for i := range series {
		series[i] = fmt.Sprintf("The %s %s Chronicles %d", pick(perfWords), pick(perfWords), i)
		if i%2 == 0 {
			series[i] = fmt.Sprintf("%s Saga %d", pick(perfWords), i)
		}
	}
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		author, title := authors[rng.IntN(len(authors))], ""
		switch r := rng.IntN(100); {
		case i < 14:
			author = "Charles Dickens"
			title = []string{"Bleak House", "Oliver Twist", "Great Expectations", "Hard Times", "Little Dorrit",
				"Dombey and Son", "David Copperfield", "Nicholas Nickleby", "Our Mutual Friend", "Barnaby Rudge",
				"A Christmas Carol", "The Pickwick Papers", "A Tale of Two Cities", "The Old Curiosity Shop"}[i]
		case r < 33:
			title = fmt.Sprintf("The %s of the %s", pick(perfWords), pick(perfWords))
		case r < 38:
			title = fmt.Sprintf("%s and %s", pick(perfThe), pick(perfWords))
		default:
			title = fmt.Sprintf("%s %s", pick(perfWords), pick(perfWords))
		}
		title = fmt.Sprintf("%s %d", title, i) // distinct books (no de-dup groups)
		b := &catalog.Book{
			LibraryID: lib.ID, Author: author, Title: title, Narrator: narrators[rng.IntN(len(narrators))],
			AddedAt:     base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			MTime:       base.Add(time.Duration(i) * time.Minute).Unix(),
			ContentHash: fmt.Sprintf("%016x", rng.Uint64()),
		}
		if rng.IntN(10) < 4 {
			b.Series, b.SeriesIndex = series[rng.IntN(len(series))], float64(1+rng.IntN(12))
		}
		folder := author + "/" + title
		if b.Series != "" {
			folder = author + "/" + b.Series + "/" + title
		}
		if rng.IntN(10) < 7 { // MP3 parts
			b.IsFolder, b.RelPath, b.Format, b.Codec = true, folder, "mp3", "mp3"
			b.CoverPath = folder + "/cover.jpg"
			var off float64
			for f := range 3 + rng.IntN(38) {
				d := 600 + rng.Float64()*1800
				fp := fmt.Sprintf("%s/%02d - %s.mp3", folder, f+1, title)
				size := int64(d * 8000)
				b.Files = append(b.Files, catalog.BookFile{RelPath: fp, Seq: f, Duration: d, Format: "mp3", Codec: "mp3", Size: size})
				b.Chapters = append(b.Chapters, metadata.Chapter{Index: f, Title: fmt.Sprintf("Chapter %d", f+1),
					FileIndex: f, FilePath: fp, Start: 0, End: d, BookOffset: off})
				off += d
				b.Size += size
			}
			b.Duration = off
		} else { // one M4B with chapters
			b.RelPath, b.Format, b.Codec = folder+".m4b", "m4b", "aac"
			if rng.IntN(2) == 0 {
				b.IsFolder, b.RelPath = true, folder
			}
			fp := folder + ".m4b"
			if b.IsFolder {
				fp = folder + "/" + title + ".m4b"
				b.CoverPath = folder + "/cover.jpg"
			}
			yes := true
			b.HasCover = &yes
			var off float64
			for c := range 8 + rng.IntN(33) {
				d := 300 + rng.Float64()*1500
				b.Chapters = append(b.Chapters, metadata.Chapter{Index: c, Title: fmt.Sprintf("Chapter %d", c+1),
					FileIndex: 0, FilePath: fp, Start: off, End: off + d, BookOffset: off})
				off += d
			}
			b.Duration, b.Size = off, int64(off*8000)
			b.Files = []catalog.BookFile{{RelPath: fp, Seq: 0, Duration: off, Format: "m4b", Codec: "aac", Size: b.Size}}
		}
		if _, err := cat.UpsertBook(ctx, b); err != nil {
			tb.Fatalf("seed book %d: %v", i, err)
		}
	}
	token, _, err := authSvc.IssueSession(ctx, admin.ID, "Web browser", "")
	if err != nil {
		tb.Fatal(err)
	}
	cfg := config.Default(tb.TempDir())
	// The rate limits are not what this measures (ratelimit_routes_test.go is).
	cfg.RateLimit.Token = config.RateBucket{Rate: 1e9, Burst: 1e9}
	a := New(cfg, authSvc, cat, library.NewScanner(cat, "", slog.Default()), "", slog.Default())
	return &perfEnv{api: a, handler: a.Handler(), token: token, libID: lib.ID, cat: cat, db: db}
}

// perfRequests are the harness's requests (apps.py latency_spec, AudioSilo),
// the deep page by the cursor of the page holding the middle of the library.
func (e *perfEnv) perfRequests(tb testing.TB) [][2]string {
	tb.Helper()
	page := fmt.Sprintf("/api/v1/libraries/%d/books?sort=recent&limit=200", e.libID)
	var pages []string
	for p := page; ; {
		pages = append(pages, p)
		var body struct {
			Next string `json:"next_cursor"`
		}
		e.getJSON(tb, p, &body)
		if body.Next == "" {
			break
		}
		p = page + "&cursor=" + url.QueryEscape(body.Next)
	}
	deep := pages[min(len(pages)-1, len(pages)/2)]
	return [][2]string{
		{"home_recent", "/api/v1/books/recent?limit=48"},
		{"library_page1", page},
		{"library_deep_page", deep},
		{"search_the", "/api/v1/search?q=the&limit=50"},
		{"search_dickens", "/api/v1/search?q=dickens&limit=50"},
		{"search_title", "/api/v1/search?q=bleak+house&limit=50"},
	}
}

func (e *perfEnv) serve(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "192.0.2.1:4000"
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set(auth.ClientHeader, "AudioSilo/2.4.1 (web)")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *perfEnv) getJSON(tb testing.TB, path string, into any) {
	tb.Helper()
	rec := e.serve(path)
	if rec.Code != http.StatusOK {
		tb.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		tb.Fatal(err)
	}
}

func perfBooks() int {
	if s := strings.TrimSpace(os.Getenv("AUDIOSILO_PERF_BOOKS")); s != "" {
		var n int
		if _, err := fmt.Sscan(s, &n); err == nil && n > 0 {
			return n
		}
	}
	return 5000
}

// BenchmarkPerfAPI times each harness request in-process (middleware, auth,
// query, JSON) against a seeded library of AUDIOSILO_PERF_BOOKS books (5,000).
func BenchmarkPerfAPI(b *testing.B) {
	e := seedPerfLibrary(b, perfBooks())
	ctx := context.Background()
	for _, r := range e.perfRequests(b) {
		// Repeated, as the harness asks: answered from the list cache.
		b.Run(r[0], func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if rec := e.serve(r[1]); rec.Code != http.StatusOK {
					b.Fatalf("%s: %d", r[1], rec.Code)
				}
			}
		})
		// After a write each time (the generation moves): the answer is built.
		b.Run(r[0]+"/uncached", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				if _, err := e.db.ExecContext(ctx, `SELECT 1`); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if rec := e.serve(r[1]); rec.Code != http.StatusOK {
					b.Fatalf("%s: %d", r[1], rec.Code)
				}
			}
		})
	}
}
