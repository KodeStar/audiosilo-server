package catalog

import (
	"fmt"
	"testing"
	"time"
)

// The household member rule's cost (plan Phase 8b-1, Performance): a kid on
// "collections" whose owner shares one 1,000-item collection, against an
// ordinary listener with one whole-library grant, on a 3,000-book library (the
// member fixture's library A). Each iteration is what a request does: load the
// caller's scopes (CallerScope / CallerScopes, with the member rule the token
// lookup read, as the transport passes it) and the query; scopes_by_id is the
// scope load for a caller known only by id (UserScopes, which reads the rule).
// Run with
//
//	go test ./internal/catalog -run '^$' -bench MemberScopes -benchtime 2s
//
// The numbers recorded at the time are in the server's CLAUDE.md (Household
// members).

const (
	benchBooks = 3000
	benchItems = 1000
)

func BenchmarkMemberScopes(b *testing.B) {
	f := newMemberFixture(b)
	owner := f.user(b, "adam", "user", 0, "")
	kid := f.user(b, "evie", "user", owner, "collections")
	ordinary := f.user(b, "bob", "user", 0, "")
	f.grant(b, owner, f.libA, "")
	f.grant(b, ordinary, f.libA, "")
	var items []string
	for i := range benchBooks {
		path := fmt.Sprintf("Author %03d/Series %d/Book %04d", i%300, i%7, i)
		if _, err := f.c.UpsertBook(f.ctx, &Book{LibraryID: f.libA, RelPath: path, Title: fmt.Sprintf("Story %04d", i), Author: fmt.Sprintf("Author %03d", i%300)}); err != nil {
			b.Fatal(err)
		}
		if i%3 != 0 || i/3 >= benchItems {
			continue
		}
		// Every third book is in the collection, and both listeners have progress on it.
		items = append(items, path)
		for _, u := range []int64{kid, ordinary} {
			if _, err := f.c.SaveProgress(f.ctx, u, Progress{Ref: Ref{LibraryID: f.libA, Path: path}, Position: 10, Duration: 100,
				UpdatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
				b.Fatal(err)
			}
		}
	}
	f.collection(b, owner, items, kid)

	for _, who := range []struct {
		name string
		id   int64
	}{{"kid_1000_items", kid}, {"ordinary_whole_library", ordinary}} {
		// The Caller a request's token lookup gives (callerByID reads the same rule).
		caller, err := f.c.callerByID(f.ctx, who.id, false)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(who.name+"/scopes", func(b *testing.B) {
			for b.Loop() {
				if _, err := f.c.CallerScopes(f.ctx, caller); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(who.name+"/scopes_by_id", func(b *testing.B) {
			for b.Loop() {
				if _, err := f.c.UserScopes(f.ctx, who.id, false); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(who.name+"/browse", func(b *testing.B) {
			for b.Loop() {
				s, err := f.c.CallerScope(f.ctx, caller, f.libA)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := f.c.ListBooks(f.ctx, ListOptions{LibraryID: f.libA, Scope: &s, Limit: 50}); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(who.name+"/search", func(b *testing.B) {
			for b.Loop() {
				scopes, err := f.c.CallerScopes(f.ctx, caller)
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
				scopes, err := f.c.CallerScopes(f.ctx, caller)
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
