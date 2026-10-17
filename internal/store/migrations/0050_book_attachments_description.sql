-- A book's generated description (the Bridge's .audiosilo/description.json,
-- Phase 1c; catalog: attachments.go), checked against audiosilo-meta's
-- model.Description and stored as JSON like the other files ('' for none or a
-- file that can't be used). Derived from the files like the rest of the row,
-- so rebuildable. A book read before this has no description column to fill:
-- its signature never named description.json (the name was unknown), so a book
-- that has one now is read again by the next scan.
ALTER TABLE book_attachments ADD COLUMN description TEXT NOT NULL DEFAULT '';
