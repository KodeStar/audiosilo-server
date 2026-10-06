-- A book's cover colours and art version, for the player's themed screens and its
-- cover URLs. Both are derived, rebuildable and never durable user state: they are
-- set when a cover thumbnail is made (GET /libraries/{id}/cover?size= or the
-- console's POST /admin/covers, catalog.RecordCoverColors) from the art it was made
-- of, so a rebuilt index simply starts without them.
--
-- cover_color is "bg" or "bg accent on_accent" (lowercase #rrggbb, space-separated;
-- see media.CoverPalette); '' = not computed yet. cover_version is a short hash of
-- the art's version (catalog.CoverVersion: the custom cover's stamp, or the sidecar
-- or embedded art's file size and mtime); '' = not known yet. A custom cover upload
-- sets cover_version at once and a removal clears both (catalog.SetCover,
-- DeleteCover); a re-index that changes the book's mtime or sidecar clears both
-- (catalog.UpsertBook), so neither outlives the art it describes.
ALTER TABLE books ADD COLUMN cover_color TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN cover_version TEXT NOT NULL DEFAULT '';
