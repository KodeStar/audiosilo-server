package catalog

import "context"

// Favourite is a user-hearted item, addressed by the path identity (Ref). It may
// be a navigation folder, a book folder, or a single-file book. The Book* fields
// are enriched from the index via a LEFT JOIN when one exists at the path;
// IsBook reports whether that join matched (a plain navigation folder won't, so a
// client renders it by its path leaf). Like all durable user state, favourites are
// path-keyed and not FK'd to the rebuildable index.
type Favourite struct {
	Ref
	IsBook      bool    `json:"is_book"`
	Title       string  `json:"title"`
	Author      string  `json:"author"`
	Series      string  `json:"series"`
	SeriesIndex float64 `json:"series_index"`
	Duration    float64 `json:"duration"`
	CreatedAt   string  `json:"created_at"`
}

// AddFavourite marks a path as a favourite for a user. Idempotent: re-favouriting
// an existing path is a no-op (the original created_at is kept).
func (c *Catalog) AddFavourite(ctx context.Context, userID int64, ref Ref) error {
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO favourites(user_id, library_id, rel_path, created_at)
		 VALUES(?,?,?,?)
		 ON CONFLICT(user_id, library_id, rel_path) DO NOTHING`,
		userID, ref.LibraryID, ref.Path, c.ts())
	return err
}

// RemoveFavourite clears a user's favourite for a path. Idempotent: removing a
// path that isn't favourited is a no-op.
func (c *Catalog) RemoveFavourite(ctx context.Context, userID int64, ref Ref) error {
	_, err := c.db.ExecContext(ctx,
		`DELETE FROM favourites WHERE user_id = ? AND library_id = ? AND rel_path = ?`,
		userID, ref.LibraryID, ref.Path)
	return err
}

// ListAllFavourites returns a user's favourites across every library they can
// still reach, newest first. The scope filter is applied in the query (see
// UserScopes), so favourites under a since-revoked share are not returned and an
// empty scope yields none. Books are LEFT-joined on the path identity to enrich
// each row with title/author/etc. — left empty for a plain navigation folder.
func (c *Catalog) ListAllFavourites(ctx context.Context, userID int64, scopes []Scope) ([]Favourite, error) {
	filter, fargs := scopesFilterSQL("f.library_id", "f.rel_path", scopes)
	args := append([]any{userID}, fargs...)
	rows, err := c.db.QueryContext(ctx,
		`SELECT f.library_id, f.rel_path,
		        b.id IS NOT NULL,
		        COALESCE(b.title, ''), COALESCE(b.author, ''),
		        COALESCE(b.series, ''), COALESCE(b.series_index, 0),
		        COALESCE(b.duration, 0), f.created_at
		   FROM favourites f
		   LEFT JOIN books b ON b.library_id = f.library_id AND b.rel_path = f.rel_path
		  WHERE f.user_id = ? AND `+filter+`
		  ORDER BY f.created_at DESC, f.rel_path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Favourite
	for rows.Next() {
		var fav Favourite
		if err := rows.Scan(&fav.LibraryID, &fav.Path, &fav.IsBook,
			&fav.Title, &fav.Author, &fav.Series, &fav.SeriesIndex,
			&fav.Duration, &fav.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, fav)
	}
	return out, rows.Err()
}
