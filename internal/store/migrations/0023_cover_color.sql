-- A book's cover art identity and colours, for the player's cover URLs and themed
-- screens. Both are derived and rebuildable, never durable user state.
--
-- cover_art identifies the art the book's cover is read from, from index data
-- alone: 'c' and its custom cover's updated_at when it has one, else 'f' and its
-- mtime, size and sidecar path (catalog.coverArtSQL, the expression below). Every
-- writer of those keeps it current (catalog.UpsertBook, SetCover, DeleteCover, a
-- move or join carrying a custom cover). The wire's cover_version is a short hash
-- of it (catalog.CoverVersion).
--
-- cover_color is the palette read from a cover thumbnail, tagged with the
-- cover_version it was read for: "version bg" or "version bg accent on_accent"
-- (lowercase #rrggbb, space-separated; see media.Palette); '' = none. It is
-- exposed only while its tag is the book's current cover_version, so new art
-- needs nothing cleared: the old colour simply stops counting. It is recorded
-- compare-and-set on cover_art (catalog.RecordCoverColors).
ALTER TABLE books ADD COLUMN cover_art TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN cover_color TEXT NOT NULL DEFAULT '';
UPDATE books SET cover_art = COALESCE(
	(SELECT 'c' || cv.updated_at FROM book_covers cv
	  WHERE cv.library_id = books.library_id AND cv.path = books.rel_path),
	'f' || books.mtime || ' ' || books.size || ' ' || books.cover_path);
