-- The release date a book's file tags give (and ffprobe's), YYYY[-MM[-DD]]: usually
-- the recording's, not the work's first publication, so it is not `published`
-- (an edit or a community match). The admin list's release-date sort falls back
-- to it for a book with no published date. A scanned value like codec, rewritten
-- by every index; never edited.
ALTER TABLE books ADD COLUMN released TEXT NOT NULL DEFAULT '';

-- A book indexed before this never had its date tags read, so it starts unchecked
-- (0) and the next scan reads its primary file once (tags and ffprobe, no
-- re-index), like the has_cover backfill; every index from now on writes 1. A
-- rebuildable index flag.
ALTER TABLE books ADD COLUMN released_checked INTEGER NOT NULL DEFAULT 0;
