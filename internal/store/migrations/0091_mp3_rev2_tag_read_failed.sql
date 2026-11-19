-- server#452: revision 2 of the scan's reading of a book's lengths
-- (catalog.durationRev, 0082's books.duration_rev) counts the frames of more
-- MP3s: a header-less one whose sampled frames share its MPEG version's lowest
-- bitrate (a VBR encoder writes silence at it) with another bitrate in its
-- first 2 MiB, or vary where the denser sampling looks, one whose Xing header gives no frame count, and one whose
-- sample rate changes, each frame at its own rate. A book holding an MP3 is
-- indexed once more by the next scan, as for revision 1. A book with no MP3
-- reads the same by both, so one 0082 or an index since started at revision 1
-- moves to 2 here, with its indexed_at kept, so the scan's staleness check
-- (catalog.durationStaleExpr) never looks for its MP3 parts.
UPDATE books SET duration_rev = 2
 WHERE duration_rev = 1 AND duration_rev_indexed_at IS indexed_at
   AND format <> 'mp3'
   AND NOT EXISTS (SELECT 1 FROM book_files f WHERE f.book_id = books.id AND f.format = 'mp3');

-- server#318: the indexed_at of a book's row when a scan's backfill read of
-- its primary file (its names or tag hints, catalog.Signature.NamesCheck,
-- PathRead, hints_checked) found ffprobe rejecting the file where the index
-- had not. While it is the row's own indexed_at the file's bytes are the ones
-- that read failed on, so the scan does not read it again for them on every
-- run (catalog.Signature.TagReadFailed); any index of the book (its files
-- changed, an admin's Re-read) moves indexed_at on and clears it. NULL for
-- every row before. A rebuildable index value, never on the wire.
ALTER TABLE books ADD COLUMN tag_read_failed_at TEXT;
