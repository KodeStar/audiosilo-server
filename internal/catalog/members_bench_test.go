package catalog

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// The household member rule's cost (plan Phase 8b-1, Performance): a kid on
// "collections" whose owner shares one 1,000-item collection, against an
// ordinary listener with one whole-library grant, on a 3,000-book library. Each
// iteration is what a request does: load the caller's scopes (UserScope /
// UserScopes, which run the member rule) and the query. Run with
//
//	go test ./internal/catalog -run '^$' -bench MemberScopes -benchtime 2s
//
// The numbers recorded at the time are in the server's CLAUDE.md (Household
// members).

const (
	benchBooks = 3000
	benchItems = 1000
)

type memberBench struct {
	c             *Catalog
	ctx           context.Context
	lib           int64
	kid, ordinary int64
}

func newMemberBench(b *testing.B) *memberBench {
	b.Helper()
	ctx := context.Background()
	c := New(storetest.Open(b), time.Now)
	f := &memberBench{c: c, ctx: ctx}
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		b.Fatal(err)
	}
	f.lib = lib.ID
	user := func(name string, owner any, access any) int64 {
		var kind any
		if owner != nil {
			kind = "kid"
		}
		res, err := c.db.ExecContext(ctx, `INSERT INTO users(username, password_hash, role, created_at, updated_at, managed_by, member_kind, member_access)
		  VALUES(?, '', 'user', 't', 't', ?, ?, ?)`, name, owner, kind, access)
		if err != nil {
			b.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	owner := user("adam", nil, nil)
	f.kid = user("evie", owner, "collections")
	f.ordinary = user("bob", nil, nil)
	for _, u := range []int64{owner, f.ordinary} {
		if err := c.GrantWholeLibrary(ctx, u, lib.ID); err != nil {
			b.Fatal(err)
		}
	}
	col, err := c.CreateCollection(ctx, owner, "Evie's books", "")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO collection_shares(collection_id, user_id, created_at) VALUES(?,?,'t')`, col.ID, f.kid); err != nil {
		b.Fatal(err)
	}
	for i := range benchBooks {
		path := fmt.Sprintf("Author %03d/Series %d/Book %04d", i%300, i%7, i)
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: path, Title: fmt.Sprintf("Story %04d", i), Author: fmt.Sprintf("Author %03d", i%300)}); err != nil {
			b.Fatal(err)
		}
		if i%3 != 0 || i/3 >= benchItems {
			continue
		}
		// Every third book is in the collection, and both listeners have progress on it.
		if _, err := c.db.ExecContext(ctx, `INSERT INTO collection_items(collection_id, library_id, rel_path, position, added_at) VALUES(?,?,?,?,'t')`,
			col.ID, lib.ID, path, i/3); err != nil {
			b.Fatal(err)
		}
		for _, u := range []int64{f.kid, f.ordinary} {
			if _, err := c.SaveProgress(ctx, u, Progress{Ref: Ref{LibraryID: lib.ID, Path: path}, Position: 10, Duration: 100,
				UpdatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
				b.Fatal(err)
			}
		}
	}
	return f
}

func BenchmarkMemberScopes(b *testing.B) {
	f := newMemberBench(b)
	for _, who := range []struct {
		name string
		id   int64
	}{{"kid_1000_items", f.kid}, {"ordinary_whole_library", f.ordinary}} {
		b.Run(who.name+"/scopes", func(b *testing.B) {
			for b.Loop() {
				if _, err := f.c.UserScopes(f.ctx, who.id, false); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(who.name+"/browse", func(b *testing.B) {
			for b.Loop() {
				s, err := f.c.UserScope(f.ctx, who.id, f.lib, false)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := f.c.ListBooks(f.ctx, ListOptions{LibraryID: f.lib, Scope: &s, Limit: 50}); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(who.name+"/search", func(b *testing.B) {
			for b.Loop() {
				scopes, err := f.c.UserScopes(f.ctx, who.id, false)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := f.c.Search(f.ctx, "Story", scopes, 50); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(who.name+"/progress", func(b *testing.B) {
			for b.Loop() {
				scopes, err := f.c.UserScopes(f.ctx, who.id, false)
				if err != nil {
					b.Fatal(err)
				}
				got, err := f.c.ListProgress(f.ctx, who.id, scopes)
				if err != nil || len(got) != benchItems {
					b.Fatalf("progress = %d, %v", len(got), err)
				}
			}
		})
	}
}
