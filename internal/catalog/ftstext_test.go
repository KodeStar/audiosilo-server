package catalog

import (
	"testing"
)

// TestFTSText: Han, Hiragana and Katakana are set apart a character a token,
// everything else is left as it is, and a second pass changes nothing.
func TestFTSText(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                 "",
		"The Final Empire": "The Final Empire",
		"Война и мир":      "Война и мир",
		"Les Misérables":   "Les Misérables",
		"사랑의 기술":           "사랑의 기술",
		"吾輩は猫である":          "吾 輩 は 猫 で あ る",
		"夏目 漱石":            "夏 目 漱 石",
		"ソードアート・オンライン":     "ソ ー ド ア ー ト ・ オ ン ラ イ ン",
		"人々":               "人 々",
		"Book 1: 三体":       "Book 1: 三 体",
		"三体II":             "三 体 II",
		"Series\n三体":       "Series\n三 体",
		"ｱｰｸ":              "ｱ ｰ ｸ",
	}
	for in, want := range cases {
		got := ftsText(in)
		if got != want {
			t.Errorf("ftsText(%q) = %q, want %q", in, got, want)
		}
		if again := ftsText(got); again != got {
			t.Errorf("ftsText(%q) again = %q, want it unchanged", got, again)
		}
	}
}

// TestSearchCJK (server#178): a word inside a Chinese or Japanese title is
// found, as is its start; the author filter's phrase finds a CJK credit; Latin
// and Cyrillic search is as it was.
func TestSearchCJK(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	for _, b := range []*Book{
		{LibraryID: lib.ID, RelPath: "1.m4b", Title: "吾輩は猫である", Author: "夏目漱石", Narrator: "朗読者"},
		{LibraryID: lib.ID, RelPath: "2.m4b", Title: "三体", Author: "刘慈欣", Series: "地球往事"},
		{LibraryID: lib.ID, RelPath: "3.m4b", Title: "ソードアート・オンライン", Author: "川原礫"},
		{LibraryID: lib.ID, RelPath: "4.m4b", Title: "Soulsmith", Author: "Will Wight"},
		{LibraryID: lib.ID, RelPath: "5.m4b", Title: "Война и мир", Author: "Толстой"},
	} {
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	all := []Scope{{LibraryID: lib.ID, AllowAll: true}}
	search := func(q string) []string {
		t.Helper()
		res, err := c.Search(ctx, q, all, 10)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		var paths []string
		for _, b := range res {
			paths = append(paths, b.RelPath)
		}
		return paths
	}
	for q, want := range map[string]string{
		"猫": "1.m4b", "吾輩": "1.m4b", "猫である": "1.m4b", "吾輩は猫である": "1.m4b", "漱石": "1.m4b", "朗読": "1.m4b",
		"体": "2.m4b", "往事": "2.m4b", "オンライン": "3.m4b", "soul": "4.m4b", "Война": "5.m4b",
	} {
		if got := search(q); len(got) != 1 || got[0] != want {
			t.Errorf("search %q = %v, want [%s]", q, got, want)
		}
	}
	if got := search("猫犬"); len(got) != 0 {
		t.Errorf("search 猫犬 = %v, want none (the characters in that order)", got)
	}

	for author, want := range map[string]int{"夏目漱石": 1, "刘慈欣": 1, "夏目": 0, "Will Wight": 1} {
		page, err := c.ListBooks(ctx, ListOptions{LibraryID: lib.ID, Author: author, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Books) != want {
			t.Errorf("author=%s: %d books, want %d", author, len(page.Books), want)
		}
	}
}

// TestIndexSearchText: the rows an older server wrote, CJK text whole, are
// rewritten once (only those), after which a word inside them is found; a
// second start rewrites nothing.
func TestIndexSearchText(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	for _, b := range []*Book{
		{LibraryID: lib.ID, RelPath: "1.m4b", Title: "吾輩は猫である", Author: "夏目漱石"},
		{LibraryID: lib.ID, RelPath: "2.m4b", Title: "Soulsmith", Author: "Will Wight"},
	} {
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// As a server before it left the index: the CJK text whole, no version.
	if _, err := c.db.ExecContext(ctx, `UPDATE books_fts SET title = '吾輩は猫である', author = '夏目漱石' WHERE title = ?`,
		ftsText("吾輩は猫である")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `DELETE FROM server_state WHERE key = ?`, searchTextKey); err != nil {
		t.Fatal(err)
	}
	all := []Scope{{LibraryID: lib.ID, AllowAll: true}}
	if res, _ := c.Search(ctx, "猫", all, 10); len(res) != 0 {
		t.Fatalf("before the rewrite: search 猫 = %d books, want the old index's none", len(res))
	}

	n, err := c.IndexSearchText(ctx)
	if err != nil || n != 1 {
		t.Fatalf("IndexSearchText = %d (err %v), want the 1 CJK row", n, err)
	}
	for _, q := range []string{"猫", "吾輩", "漱石", "soul"} {
		if res, _ := c.Search(ctx, q, all, 10); len(res) != 1 {
			t.Errorf("after the rewrite: search %q = %d books, want 1", q, len(res))
		}
	}
	if n, err := c.IndexSearchText(ctx); err != nil || n != 0 {
		t.Errorf("second IndexSearchText = %d (err %v), want 0", n, err)
	}
	// A new database (every row written in ftsText's form) records the version
	// and rewrites nothing.
	fresh, ctx2 := newTestCatalog(t)
	if n, err := fresh.IndexSearchText(ctx2); err != nil || n != 0 {
		t.Errorf("fresh IndexSearchText = %d (err %v), want 0", n, err)
	}
	if v, err := getServerState[string](ctx2, fresh.db, searchTextKey); err != nil || v != searchTextVersion {
		t.Errorf("fresh search_text = %q (err %v), want %q", v, err, searchTextVersion)
	}
}
