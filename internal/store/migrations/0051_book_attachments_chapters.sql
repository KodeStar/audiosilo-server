-- A book's generated chapters (the Bridge's .audiosilo/chapters.json, Phase 1d;
-- catalog: attachments.go, communitychapters.go), checked by
-- metadata.ParseChapterList and stored as JSON like the other files ('' for none
-- or a file that can't be used). Derived from the files like the rest of the
-- row, so rebuildable. A book read before this has no chapters column to fill:
-- its signature never named chapters.json (the name was unknown), so a book
-- that has one now is read again by the next scan.
ALTER TABLE book_attachments ADD COLUMN chapters TEXT NOT NULL DEFAULT '';
