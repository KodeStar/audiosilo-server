-- The revision of the scan's reading of a book's lengths (catalog.durationRev).
-- Revision 1 counts the frames of a VBR MP3 with no Xing or VBRI header, whose
-- length ffprobe and the in-process probe used to estimate from a bitrate, up
-- to ~7% off the audio (SRV-16). A book indexed before this starts at 0, so the
-- next scan indexes a book holding an MP3 once more though its files are
-- unchanged; every index from now on writes the current revision, unless one
-- of the book's header-less VBR files could not be counted.
-- duration_rev_indexed_at is the indexed_at the revision was written with:
-- NULL or another index's (an older server re-indexed the row, storing its own
-- estimate and leaving the revision) counts as an older revision too, as
-- file_order_indexed_at does for the file order (0055). A rebuildable index
-- value, never on the wire.
ALTER TABLE books ADD COLUMN duration_rev INTEGER NOT NULL DEFAULT 0;
ALTER TABLE books ADD COLUMN duration_rev_indexed_at TEXT;
-- A book with no MP3 (its own format, or a part's) reads the same by revision
-- 1, so it starts there, with its own indexed_at: the scan's staleness check
-- (catalog.durationStaleExpr) then never looks for its MP3 parts.
UPDATE books SET duration_rev = 1, duration_rev_indexed_at = indexed_at
 WHERE format <> 'mp3'
   AND NOT EXISTS (SELECT 1 FROM book_files f WHERE f.book_id = books.id AND f.format = 'mp3');
