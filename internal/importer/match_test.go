package importer

import (
	"testing"

	"golang.org/x/text/unicode/norm"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// matchBooks is a library here: library 1 the person can access, library 2 not.
func matchBooks() []catalog.Book {
	b := func(lib int64, path string, folder bool, title, author string, dur float64) catalog.Book {
		return catalog.Book{LibraryID: lib, RelPath: path, IsFolder: folder, Title: title, Author: author, Duration: dur}
	}
	books := []catalog.Book{
		b(1, "Andy Weir/Project Hail Mary", true, "Project Hail Mary", "Andy Weir", 8400),
		b(1, "media/Jane Austen/Pride and Prejudice", true, "Pride and Prejudice", "Jane Austen", 7200),
		b(1, "Mary Shelley/Frankenstein/Frankenstein.m4b", false, "Frankenstein", "Mary Shelley", 4500),
		b(1, "Two/Files/a.m4b", false, "A", "Two", 100),
		b(1, "Two/Files/b.m4b", false, "B", "Two", 100),
		b(1, "Loose", true, "Loose", "Nobody", 60),
		b(1, "Dup/One/Book", true, "Dup One", "X", 0),
		b(1, "Dup/Two/Book", true, "Dup Two", "X", 0),
		b(1, "Émile Zola/Germinal", true, "Germinal", "Émile Zola", 6000),
		b(1, "Hollis Varga/The Cartographer's Daughter/CD1", true, "CD1", "Hollis Varga", 1800),
		b(1, "Hollis Varga/The Cartographer's Daughter/CD2", true, "CD2", "Hollis Varga", 1800),
		b(1, "Brandon Sanderson/Stormlight/01 - The Way of Kings", true, "The Way of Kings", "Brandon Sanderson", 10800),
		b(1, "Brandon Sanderson/Stormlight/03 - Oathbringer", true, "Oathbringer", "Brandon Sanderson", 10200),
		b(1, "Some/Other/Path", true, "Leviathan Wakes", "James S. A. Corey", 9600),
		b(1, "Isbn/Book", true, "Isbn Book", "I", 1000),
		b(2, "Kids/Winnie-the-Pooh", true, "Winnie-the-Pooh", "A. A. Milne", 2700),
	}
	books[9].SplitParent = "Hollis Varga/The Cartographer's Daughter"
	books[10].SplitParent = books[9].SplitParent
	books[11].SeriesIndex, books[11].Series = 1, "The Stormlight Archive"
	books[12].SeriesIndex, books[12].Series = 3, "The Stormlight Archive"
	books[13].ASIN = "B00P9XDQFY"
	books[14].ISBN = "9780000000001"
	return books
}

func onlyLib1(ref catalog.Ref) bool { return ref.LibraryID == 1 }

func TestMatchTiers(t *testing.T) {
	m := newBookIndex(matchBooks()).matcher(onlyLib1)
	for _, tc := range []struct {
		name     string
		it       Item
		tier     string
		path     string
		reason   string
		duration float64
	}{
		{name: "exact path", it: Item{RelPath: "Andy Weir/Project Hail Mary",
			FullPath: "/abs/books/Andy Weir/Project Hail Mary", Duration: 8400},
			tier: TierPath, path: "Andy Weir/Project Hail Mary"},
		{name: "ABS rooted deeper (its relPath a suffix of ours)", it: Item{RelPath: "Jane Austen/Pride and Prejudice",
			FullPath: "/data/Jane Austen/Pride and Prejudice"},
			tier: TierPath, path: "media/Jane Austen/Pride and Prejudice"},
		{name: "ABS rooted higher (ours a suffix of its path)", it: Item{RelPath: "audio/Andy Weir/Project Hail Mary",
			FullPath: "/mnt/audio/Andy Weir/Project Hail Mary"},
			tier: TierPath, path: "Andy Weir/Project Hail Mary"},
		{name: "ABS folder is a single file here", it: Item{RelPath: "Mary Shelley/Frankenstein",
			FullPath: "/abs/Mary Shelley/Frankenstein", Duration: 4501},
			tier: TierPath, path: "Mary Shelley/Frankenstein/Frankenstein.m4b"},
		{name: "a folder with two files is not one book", it: Item{RelPath: "Two/Files", FullPath: "/abs/Two/Files"},
			reason: catalog.ReasonNoMatch},
		{name: "ABS single file alone in a folder that is a book here", it: Item{RelPath: "Loose.m4b",
			FullPath: "/abs/Loose/Loose.m4b", IsFile: true, Siblings: 1},
			tier: TierPath, path: "Loose"},
		{name: "a lone file with siblings is not", it: Item{RelPath: "Loose.m4b",
			FullPath: "/abs/Loose/Loose.m4b", IsFile: true, Siblings: 2},
			reason: catalog.ReasonNoMatch},
		{name: "decomposed accents still match", it: Item{RelPath: norm.NFD.String("Émile Zola/Germinal"),
			FullPath: norm.NFD.String("/abs/Émile Zola/Germinal")},
			tier: TierPath, path: "Émile Zola/Germinal"},
		{name: "two equally good suffixes: no path match", it: Item{RelPath: "Book", FullPath: "/abs/Book"},
			reason: catalog.ReasonNoMatch},
		{name: "the duration guard refuses a path match", it: Item{RelPath: "Andy Weir/Project Hail Mary",
			FullPath: "/x/Andy Weir/Project Hail Mary", Duration: 3600},
			reason: catalog.ReasonNoMatch},
		{name: "a book split across disc folders", it: Item{RelPath: "Hollis Varga/The Cartographer's Daughter",
			FullPath: "/abs/Hollis Varga/The Cartographer's Daughter", Title: "CD1", Author: "Hollis Varga"},
			reason: catalog.ReasonSplit},
		{name: "ASIN", it: Item{RelPath: "Elsewhere/Leviathan", FullPath: "/abs/Elsewhere/Leviathan",
			ASIN: "b00p9xdqfy"}, tier: TierASIN, path: "Some/Other/Path"},
		{name: "ISBN with hyphens", it: Item{Gone: true, ISBN: "978-0-00-000000-1"}, tier: TierISBN, path: "Isbn/Book"},
		{name: "fuzzy title", it: Item{Gone: true, Title: "Oathbringer", Author: "Brandon Sanderson",
			Series: "The Stormlight Archive", Sequence: "3", Duration: 10300},
			tier: TierTitle, path: "Brandon Sanderson/Stormlight/03 - Oathbringer"},
		{name: "fuzzy title refused by the duration guard", it: Item{Gone: true, Title: "Oathbringer",
			Author: "Brandon Sanderson", Series: "The Stormlight Archive", Sequence: "3", Duration: 2000},
			reason: catalog.ReasonNoMatch},
		{name: "a book the person can't access", it: Item{RelPath: "Kids/Winnie-the-Pooh",
			FullPath: "/abs/Kids/Winnie-the-Pooh"}, reason: catalog.ReasonNoAccess},
		{name: "fuzzy only to a book the person can't access", it: Item{Gone: true, Title: "Winnie-the-Pooh",
			Author: "A. A. Milne"}, reason: catalog.ReasonNoAccess},
		{name: "nothing", it: Item{RelPath: "Unknown/Book", FullPath: "/abs/Unknown/Book", Title: "Unknown",
			Author: "Nobody Else"}, reason: catalog.ReasonNoMatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := m.match(tc.it)
			if got.Tier != tc.tier || got.Ref.Path != tc.path || got.Reason != tc.reason {
				t.Errorf("match = %+v, want tier %q path %q reason %q", got, tc.tier, tc.path, tc.reason)
			}
		})
	}
}

// TestMatchContested: two items present in ABS resolving to one book are both
// skipped (each has its own progress); items ABS deleted (gone: sessions only)
// match the book with the one present item there, their sessions merging.
func TestMatchContested(t *testing.T) {
	m := newBookIndex(matchBooks()).matcher(onlyLib1)
	got := m.matchAll([]Item{
		{ID: "a", Gone: true, ASIN: "B00P9XDQFY"},
		{ID: "b", RelPath: "Some/Other/Path", FullPath: "/abs/Some/Other/Path"},
		{ID: "b2", Gone: true, Title: "Leviathan Wakes", Author: "James S. A. Corey", Duration: 9600},
		{ID: "c", RelPath: "Andy Weir/Project Hail Mary", FullPath: "/abs/Andy Weir/Project Hail Mary"},
		{ID: "d", RelPath: "Isbn/Book", FullPath: "/abs/Isbn/Book"},
		{ID: "e", ISBN: "9780000000001", RelPath: "Elsewhere/Isbn", FullPath: "/abs/Elsewhere/Isbn"},
		{ID: "f", Gone: true, ISBN: "9780000000001"},
		{ID: "g", Gone: true, Title: "Germinal", Author: "Émile Zola", Duration: 6000},
		{ID: "h", Gone: true, Title: "Germinal", Author: "Émile Zola", Duration: 6000},
	})
	for _, id := range []string{"a", "b", "b2"} {
		if r := got[id]; r.Tier == "" || r.Ref.Path != "Some/Other/Path" {
			t.Errorf("gone + present %s = %+v", id, r)
		}
	}
	if got["c"].Tier != TierPath {
		t.Errorf("uncontested = %+v", got["c"])
	}
	// Two present items (and a gone one with them) on one book: all contested.
	for _, id := range []string{"d", "e", "f"} {
		if got[id].Reason != catalog.ReasonContest {
			t.Errorf("two present %s = %+v", id, got[id])
		}
	}
	// Only gone items on one book: no progress to conflict, they all match.
	if got["g"].Tier != TierTitle || got["h"].Tier != TierTitle {
		t.Errorf("two gone = %+v, %+v", got["g"], got["h"])
	}
}

// TestMatchIndexPerPerson: one book index (a fetch builds it once) serves
// several people, each with their own access.
func TestMatchIndexPerPerson(t *testing.T) {
	idx := newBookIndex(matchBooks())
	pooh := Item{RelPath: "Kids/Winnie-the-Pooh", FullPath: "/abs/Kids/Winnie-the-Pooh"}
	discs := Item{RelPath: "Hollis Varga/The Cartographer's Daughter",
		FullPath: "/abs/Hollis Varga/The Cartographer's Daughter"}
	if got := idx.matcher(onlyLib1).match(pooh); got.Reason != catalog.ReasonNoAccess {
		t.Errorf("library 1 only: %+v", got)
	}
	everything := idx.matcher(func(catalog.Ref) bool { return true })
	if got := everything.match(pooh); got.Tier != TierPath || got.Ref.LibraryID != 2 {
		t.Errorf("everything: %+v", got)
	}
	// The disc folders' access is the person's too.
	if got := idx.matcher(func(r catalog.Ref) bool { return r.LibraryID == 2 }).match(discs); got.Reason == catalog.ReasonSplit {
		t.Errorf("split discs reported to a person who can't see them: %+v", got)
	}
	if got := everything.match(discs); got.Reason != catalog.ReasonSplit {
		t.Errorf("split discs: %+v", got)
	}
}

func TestPathScore(t *testing.T) {
	split := splitPath
	for _, tc := range []struct {
		a, full string
		rel     int
		want    int
	}{
		{"A/B", "/x/A/B", 2, 2},
		{"m/A/B", "/x/A/B", 2, 2}, // relPath A/B is a suffix of m/A/B
		{"A/B", "/x/y/A/B", 3, 2}, // A/B is a suffix of the absolute path
		{"z/A/B", "/x/y/A/B", 3, 0},
		{"A/C", "/x/A/B", 2, 0},
		{"B", "/x/A/B", 2, 1},
	} {
		if got := pathScore(split(tc.a), split(tc.full), tc.rel); got != tc.want {
			t.Errorf("pathScore(%q, %q, %d) = %d, want %d", tc.a, tc.full, tc.rel, got, tc.want)
		}
	}
}
