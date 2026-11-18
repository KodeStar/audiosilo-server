package catalog

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"strings"

	unorm "golang.org/x/text/unicode/norm"
)

// Path spelling (server#210). A name with an accent has two Unicode spellings,
// precomposed (NFC: "É") and decomposed (NFD: "E" + a combining accent). Disks
// keep either (an HFS+ Mac wrote NFD; most tools write NFC), and a link typed by
// hand or shared from another system may carry the other one. The index keeps
// every path as the disk spells it (books.rel_path) and compares spellings by
// their NFC form (books.path_key), so a request in another spelling reaches the
// book while everything stored and answered stays the index's spelling.

// pathKeysKey is the server_state row holding the pathKeysVersion the stored
// books.path_key values were computed with.
const pathKeysKey = "path_keys"

// pathKeysVersion names what a stored key was computed with: the key's rule (the
// leading number) and x/text's Unicode tables, so an upgrade that composes a
// character another way keys every book again.
var pathKeysVersion = "1/" + unorm.Version

// pathKey is books.path_key for a book at rel: "" when rel is already NFC (every
// ASCII path), else its NFC form.
func pathKey(rel string) string {
	if unorm.NFC.IsNormalString(rel) {
		return ""
	}
	return unorm.NFC.String(rel)
}

// normSensitive reports whether p has another Unicode spelling: false when it
// is both NFC and NFD, as every ASCII path is, so Spelling needs no lookup.
func normSensitive(p string) bool {
	return !unorm.NFC.IsNormalString(p) || !unorm.NFD.IsNormalString(p)
}

// KeyPaths writes books.path_key for every book that has none (migration 0083
// left every row NULL; a book an older server inserted after a rollback has
// none either), and for every book whenever pathKeysVersion changes. One
// transaction at the start, before the server answers (Spelling reads the keys).
// It returns how many books it keyed.
func (c *Catalog) KeyPaths(ctx context.Context) (int, error) {
	v, err := getServerState[string](ctx, c.db, pathKeysKey)
	if err != nil {
		return 0, err
	}
	where := `WHERE path_key IS NULL`
	if v != pathKeysVersion {
		where = ``
	} else {
		var unkeyed bool
		if err := c.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM books WHERE path_key IS NULL)`).Scan(&unkeyed); err != nil || !unkeyed {
			return 0, err
		}
	}
	keyed := 0
	err = c.db.WithTx(ctx, "KeyPaths", func(tx *sql.Tx) error {
		type book struct {
			id     int64
			rel    string
			stored sql.NullString
		}
		books, err := queryRows(ctx, tx, func(r *sql.Rows, b *book) error { return r.Scan(&b.id, &b.rel, &b.stored) },
			`SELECT id, rel_path, path_key FROM books `+where)
		if err != nil {
			return err
		}
		set, err := tx.PrepareContext(ctx, `UPDATE books SET path_key = ? WHERE id = ?`)
		if err != nil {
			return err
		}
		defer set.Close()
		for _, b := range books {
			if k := pathKey(b.rel); !b.stored.Valid || b.stored.String != k {
				if _, err := set.ExecContext(ctx, k, b.id); err != nil {
					return err
				}
			}
		}
		keyed = len(books)
		return putServerState(ctx, tx, pathKeysKey, pathKeysVersion, c.stamp())
	})
	return keyed, err
}

// Spelling is the index's spelling of p, a cleaned library-relative path a
// client sent: p itself when the index holds it as given or holds nothing it
// could mean, else the one indexed path whose NFC form is p's. From the index
// alone, never the disk:
//
//  1. a path with no other Unicode spelling (every ASCII path) is p, no lookup;
//  2. a book at p exactly is p;
//  3. the one book whose path is p in NFC (rel_path or path_key);
//  4. a file of the innermost folder book above p, or a folder inside one (a
//     disc folder of a joined book), whose path is p in NFC: that file's
//     spelling, cut to p's depth for a folder;
//  5. a folder holding books (a navigation folder) whose path is p with each
//     name in its given, NFC or NFD spelling: that spelling.
//
// Two indexed paths that both read as p (an NFC and an NFD folder side by side
// on a disk that tells them apart) leave p as given: it never guesses. The
// result is an index row's spelling or p, so it reaches nothing p's canonical
// form could not; a caller still checks scope and hidden paths on it, and reads
// the disk only through SafeJoin.
func (c *Catalog) Spelling(ctx context.Context, libraryID int64, p string) (string, error) {
	if !normSensitive(p) {
		return p, nil
	}
	if _, err := bookIDByPath(ctx, c.db, libraryID, p); !errors.Is(err, ErrNotFound) {
		return p, err // the book at p, or a failed lookup
	}
	k := unorm.NFC.String(p)
	books, err := c.booksKeyed(ctx, libraryID, []string{k}, false)
	if err != nil {
		return "", err
	}
	switch len(books) {
	case 0:
	case 1:
		return books[0].rel, nil
	default:
		return p, nil // two books read as p
	}
	if s, found, err := c.spellingInBook(ctx, libraryID, p, k); found || err != nil {
		return s, err
	}
	return c.folderSpelling(ctx, libraryID, p)
}

// keyedBook is a book booksKeyed found: its stored path and NFC key.
type keyedBook struct {
	id       int64
	rel, key string
}

// booksKeyed reads the books whose path in NFC is one of keys (each an NFC
// path): those stored in NFC by rel_path, the others by path_key. With folders,
// folder books only.
func (c *Catalog) booksKeyed(ctx context.Context, libraryID int64, keys []string, folders bool) ([]keyedBook, error) {
	args := make([]any, 0, 2*len(keys)+2)
	args = append(args, libraryID)
	for _, k := range keys {
		args = append(args, k)
	}
	args = append(args, libraryID)
	for _, k := range keys {
		args = append(args, k)
	}
	folder := ``
	if folders {
		folder = ` AND is_folder = 1`
	}
	in := placeholders(len(keys))
	// path_key != '' names the partial index's own condition, so it is used.
	return queryRows(ctx, c.db, func(r *sql.Rows, b *keyedBook) error {
		if err := r.Scan(&b.id, &b.rel); err != nil {
			return err
		}
		b.key = unorm.NFC.String(b.rel)
		return nil
	}, `SELECT id, rel_path FROM books WHERE library_id = ? AND rel_path IN (`+in+`)`+folder+`
	    UNION ALL
	    SELECT id, rel_path FROM books WHERE library_id = ? AND path_key IN (`+in+`) AND path_key != ''`+folder,
		args...)
}

// spellingInBook is Spelling's step 4: p (key k) as a file of, or a folder
// inside, the innermost folder book above it that holds one. found is false
// when no book above p holds a path reading as p.
func (c *Catalog) spellingInBook(ctx context.Context, libraryID int64, p, k string) (string, bool, error) {
	var keys []string
	for d := path.Dir(k); d != "." && d != "/"; d = path.Dir(d) {
		keys = append(keys, d)
	}
	if len(keys) == 0 {
		return "", false, nil
	}
	books, err := c.booksKeyed(ctx, libraryID, keys, true)
	if err != nil || len(books) == 0 {
		return "", false, err
	}
	byKey := map[string][]keyedBook{}
	for _, b := range books {
		byKey[b.key] = append(byKey[b.key], b)
	}
	depth := strings.Count(p, "/") + 1
	for _, key := range keys { // innermost first
		at := byKey[key]
		switch {
		case len(at) == 0:
			continue
		case len(at) > 1:
			return p, true, nil // two books read as this folder
		}
		files, err := queryRows(ctx, c.db, func(r *sql.Rows, f *string) error { return r.Scan(f) },
			`SELECT rel_path FROM book_files WHERE book_id = ?`, at[0].id)
		if err != nil {
			return "", false, err
		}
		match := ""
		for _, f := range files {
			if f == p || strings.HasPrefix(f, p+"/") {
				return p, true, nil // the index has p as given
			}
			var s string
			switch fk := unorm.NFC.String(f); {
			case fk == k:
				s = f
			case strings.HasPrefix(fk, k+"/"):
				s = strings.Join(strings.SplitN(f, "/", depth+1)[:depth], "/")
			default:
				continue
			}
			if match != "" && match != s {
				return p, true, nil // two spellings read as p
			}
			match = s
		}
		if match != "" {
			return match, true, nil
		}
	}
	return "", false, nil
}

// maxFolderSpellings bounds the spellings folderSpelling tries: a path whose
// names have more combinations tries only p, its NFC and its NFD form.
const maxFolderSpellings = 32

// folderSpelling is Spelling's step 5: p as a folder holding indexed books,
// each of its names in the spelling given, NFC or NFD. p when the index holds
// books below p as given, or below none or several of the other spellings.
func (c *Catalog) folderSpelling(ctx context.Context, libraryID int64, p string) (string, error) {
	holds := func(s string) (bool, error) {
		var ok bool
		err := c.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM books WHERE library_id = ? AND rel_path >= ? AND rel_path < ?)`,
			libraryID, s+"/", s+"0").Scan(&ok) // '0' sorts just after '/'
		return ok, err
	}
	if ok, err := holds(p); ok || err != nil {
		return p, err
	}
	found := ""
	for _, s := range folderSpellings(p) {
		if s == p {
			continue
		}
		ok, err := holds(s)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		if found != "" {
			return p, nil // two folders read as p
		}
		found = s
	}
	if found == "" {
		return p, nil
	}
	return found, nil
}

// folderSpellings is every spelling of p whose names are each as given, NFC or
// NFD (p first), or only p, NFC(p) and NFD(p) past maxFolderSpellings.
func folderSpellings(p string) []string {
	names := strings.Split(p, "/")
	forms := make([][]string, len(names))
	total := 1
	for i, n := range names {
		forms[i] = distinct(n, unorm.NFC.String(n), unorm.NFD.String(n))
		total *= len(forms[i])
		if total > maxFolderSpellings {
			return distinct(p, unorm.NFC.String(p), unorm.NFD.String(p))
		}
	}
	out := []string{""}
	for i, f := range forms {
		next := make([]string, 0, len(out)*len(f))
		for _, prefix := range out {
			for _, n := range f {
				if i > 0 {
					n = prefix + "/" + n
				}
				next = append(next, n)
			}
		}
		out = next
	}
	return out
}

// distinct is ss without repeats, in order.
func distinct(ss ...string) []string {
	out := ss[:0:0]
	for _, s := range ss {
		dup := false
		for _, o := range out {
			dup = dup || o == s
		}
		if !dup {
			out = append(out, s)
		}
	}
	return out
}
