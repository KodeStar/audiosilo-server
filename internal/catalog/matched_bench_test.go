package catalog

import (
	"testing"
	"time"
)

// BenchmarkMatchedSummaries runs the catalog reads directly, bypassing the
// response cache. Its 20,000 books form four equal groups: ASIN only, ISBN
// only, recorded match only and unmatched.
func BenchmarkMatchedSummaries(b *testing.B) {
	c, ctx := newTestCatalog(b)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `WITH RECURSIVE seq(n) AS (
			VALUES(1) UNION ALL SELECT n+1 FROM seq WHERE n < 20000
		) INSERT INTO books(library_id, rel_path, title, author, series, series_index, asin, isbn, indexed_at)
		SELECT ?, printf('Author %d/Book %d', n%100, n), 'Book ' || n, 'Author ' || (n%100),
			'Series ' || (n%500), 1 + (n/500),
			CASE WHEN n%4 = 0 THEN printf('B%09d', n) ELSE '' END,
			CASE WHEN n%4 = 1 THEN printf('978%010d', n) ELSE '' END, 't'
		FROM seq`, lib.ID); err != nil {
		b.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO book_matches(library_id, path, work_id, how, set_at)
		SELECT library_id, rel_path, 'work-' || id, 'accepted', 't' FROM books WHERE id%4 = 2`); err != nil {
		b.Fatal(err)
	}

	for _, read := range []struct {
		name string
		run  func() error
	}{
		{"ActivityCoverage", func() error { return c.activityCoverage(ctx, &Activity{}, time.Time{}, time.Time{}) }},
		{"HealthUnmatched", func() error {
			_, err := c.IssueCounts(ctx, []string{IssueUnmatched})
			return err
		}},
		{"BookFacets", func() error {
			_, err := c.BookFacets(ctx, BookFilter{LibraryID: lib.ID})
			return err
		}},
		{"Series", func() error {
			_, err := c.Series(ctx, lib.ID, nil, false)
			return err
		}},
	} {
		b.Run(read.name, func(b *testing.B) {
			for b.Loop() {
				if err := read.run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
