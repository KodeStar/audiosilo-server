-- Bridge Phase 3c-1: read-along. The Bridge writes a book's read-along map
-- (.audiosilo/readalong.json: where each sentence of its ebook is spoken in its
-- audio) beside its other attachments; the server stores the map's head (format,
-- ebook, audio, generator, generated_at, coverage; never the sections, which are
-- streamed from the file) in book_attachments.readalong ('' for none or a file
-- that can't be used), checked by metadata.ParseReadalongHead. Derived from the
-- files like the rest of the row, so rebuildable. A book read before this has
-- no readalong column to fill: its signature never named readalong.json (the
-- name was unknown), so a book that has one now is read again by the next scan.
ALTER TABLE book_attachments ADD COLUMN readalong TEXT NOT NULL DEFAULT '';

-- A map names the ebook it was made from by its sha256, so the server tells a
-- stale map from a fresh one by hashing the ebook. Hashing every EPUB of a
-- library would read whole libraries over a network share, so only a book with
-- a map is hashed (by the scan, the Bridge's attachments rescan, or on demand
-- by GET /readalong): ebook_sha256 is the lowercase hex digest ('' for none) and
-- ebook_hashed_sig the ebook_sig of the file it was computed for, so a digest
-- whose file changed since (ebook_hashed_sig != ebook_sig) is known stale.
ALTER TABLE books ADD COLUMN ebook_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN ebook_hashed_sig TEXT NOT NULL DEFAULT '';

-- The ebook's size in bytes when last scanned (Book.ebook.size), its own column
-- rather than read back out of ebook_sig (server#228). Filled here from the
-- signature, whose size leads ("<size>:<mtime ns>:<name>"), for every book
-- whose ebook was recorded before it.
ALTER TABLE books ADD COLUMN ebook_size INTEGER NOT NULL DEFAULT 0;
UPDATE books SET ebook_size = CAST(substr(ebook_sig, 1, instr(ebook_sig, ':') - 1) AS INTEGER)
 WHERE ebook_path != '' AND instr(ebook_sig, ':') > 1;
