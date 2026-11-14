package importer

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/importer/abstest"
)

// TestIdentifiersFor: every ABS book with an identifier is paired with a book
// here by path, then by title and author, whoever can access the book (library
// 2 too); a contested book, a book split into discs here, an item
// that pairs with nothing and an identifier that isn't one give no hint, and a
// book already matched here is counted and left alone.
func TestIdentifiersFor(t *testing.T) {
	books := matchBooks()
	books[13].Matched = true // Leviathan Wakes has an ASIN of its own
	x := newBookIndex(books)
	p := &Payload{Identified: []Item{
		{ID: "phm", RelPath: "Andy Weir/Project Hail Mary", FullPath: "/abs/Andy Weir/Project Hail Mary",
			ASIN: "b08g9prs1k", Duration: 8400},
		{ID: "pooh", RelPath: "Kids/Winnie-the-Pooh", FullPath: "/abs/Kids/Winnie-the-Pooh", ISBN: "978-0-00-000000-7"},
		{ID: "oath", RelPath: "Elsewhere/Book Three", FullPath: "/abs/Elsewhere/Book Three", Title: "Oathbringer",
			Author: "Brandon Sanderson", Series: "The Stormlight Archive", Sequence: "3", ASIN: "B01N4VBIPG", Duration: 10200},
		{ID: "frank", RelPath: "Mary Shelley/Frankenstein", FullPath: "/abs/Mary Shelley/Frankenstein",
			ASIN: "B0FRANK001", ISBN: "9780000000010", Duration: 4500},
		{ID: "leviathan", RelPath: "Some/Other/Path", FullPath: "/abs/Some/Other/Path", ASIN: "B0OTHER001"},
		// Two ABS books on Pride and Prejudice: one by path, one by title.
		{ID: "pp1", RelPath: "Jane Austen/Pride and Prejudice", FullPath: "/abs/Jane Austen/Pride and Prejudice",
			ASIN: "B0PRIDE001"},
		{ID: "pp2", RelPath: "Austen/P and P", FullPath: "/abs/Austen/P and P", Title: "Pride and Prejudice",
			Author: "Jane Austen", ASIN: "B0PRIDE002"},
		{ID: "split", RelPath: "Hollis Varga/The Cartographer's Daughter",
			FullPath: "/abs/Hollis Varga/The Cartographer's Daughter", Title: "CD1", Author: "Hollis Varga",
			ASIN: "B0SPLIT001"}, // a title that would pair with one of the discs
		{ID: "nothing", RelPath: "Nobody/Nothing", FullPath: "/abs/Nobody/Nothing", Title: "Nothing", Author: "Nobody",
			ASIN: "B0NONE0001"},
		{ID: "invalid", RelPath: "Émile Zola/Germinal", FullPath: "/abs/Émile Zola/Germinal", ASIN: "nope"},
	}}
	hints, sum := identifiersFor(p.Identified, x)
	ref := func(lib int64, path string) catalog.Ref { return catalog.Ref{LibraryID: lib, Path: path} }
	want := []catalog.BookHint{
		{Ref: ref(1, "Andy Weir/Project Hail Mary"), ASIN: "B08G9PRS1K"},
		{Ref: ref(2, "Kids/Winnie-the-Pooh"), ISBN: "9780000000007"},
		{Ref: ref(1, "Brandon Sanderson/Stormlight/03 - Oathbringer"), ASIN: "B01N4VBIPG"},
		{Ref: ref(1, "Mary Shelley/Frankenstein/Frankenstein.m4b"), ASIN: "B0FRANK001", ISBN: "9780000000010"},
	}
	if !reflect.DeepEqual(hints, want) {
		t.Errorf("hints =\n%+v\nwant\n%+v", hints, want)
	}
	if wantSum := (catalog.ImportIdentifiers{Books: 4, ASIN: 3, ISBN: 2, ByTitle: 1, AlreadyMatched: 1}); *sum != wantSum {
		t.Errorf("summary = %+v, want %+v", *sum, wantSum)
	}

	// A payload stored before identifiers were kept carries none.
	if hints, sum := identifiersFor(nil, x); hints != nil || *sum != (catalog.ImportIdentifiers{}) {
		t.Errorf("no identified books: %+v %+v", hints, *sum)
	}
}

// TestIdentifiedItemsFromRecordedABS: the payload keeps every ABS book with an
// identifier, in every library, whether or not the person has history for it,
// with what pairing reads, and it survives the stored round trip.
func TestIdentifiedItemsFromRecordedABS(t *testing.T) {
	srv := abstest.New(t)
	c := clientFor(t, srv.URL, abstest.AdminToken)
	items, err := c.items(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := newItemIndex(items).identified
	if len(got) != 13 {
		t.Fatalf("%d identified items, want the 13 with an ASIN", len(got))
	}
	i := slices.IndexFunc(got, func(it Item) bool { return it.Title == "Leviathan Wakes" })
	if i < 0 || got[i].ASIN != "B00P9XDQFY" || got[i].RelPath != "James S. A. Corey/The Expanse/01 - Leviathan Wakes" ||
		got[i].FullPath == "" || got[i].Author != "James S. A. Corey" || got[i].Siblings == 0 || got[i].Duration != 9600 {
		t.Fatalf("Leviathan Wakes = %+v", got)
	}
	if !slices.ContainsFunc(got, func(it Item) bool { return it.ASIN == "B017V568SY" }) {
		t.Error("a book only in another ABS library (no one's history) is missing")
	}

	p := &Payload{V: payloadVersion, Items: []Item{}, Sessions: []Session{}, Progress: []Progress{},
		Bookmarks: []Bookmark{}, Identified: got}
	enc, err := encodePayload(p)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := decodePayload(enc); err != nil || !reflect.DeepEqual(back, p) {
		t.Fatalf("round trip: %v", err)
	}
}
