-- Player redesign Phase 4: the caller's bookmarks, notes and listening history
-- across books (GET /me/bookmarks, /me/notes, /me/history) read one user's rows
-- newest first and page by keyset: user_id = ? ORDER BY created_at (ended_at)
-- DESC, id DESC. The existing indexes lead with (user_id, library_id, rel_path),
-- so they can only seek the user and then sort all of their rows; these seek the
-- user and walk the order. The id tiebreaker is the rowid, which every index
-- entry already ends with.
CREATE INDEX idx_bookmarks_user_created ON bookmarks(user_id, created_at);
CREATE INDEX idx_notes_user_created ON notes(user_id, created_at);
CREATE INDEX idx_history_user_ended ON listening_history(user_id, ended_at);

-- The lists order by those timestamps as text, so the rows stored before this
-- release are rewritten to the fixed-width UTC millisecond form new rows take
-- (catalog's c.stamp): the server wrote created_at as RFC3339Nano, which trims
-- trailing zeros ("...:00Z" sorts after "...:00.1Z"), and a span's times were the
-- client's text verbatim (an offset sorts by local time). Only a value that reads
-- as a date is touched (the GLOB keeps strftime from taking a bare number as a
-- Julian day); anything else stays as it was.
UPDATE bookmarks SET created_at = strftime('%Y-%m-%dT%H:%M:%fZ', created_at)
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-*' AND strftime('%Y-%m-%dT%H:%M:%fZ', created_at) IS NOT NULL;
UPDATE notes SET created_at = strftime('%Y-%m-%dT%H:%M:%fZ', created_at)
 WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-*' AND strftime('%Y-%m-%dT%H:%M:%fZ', created_at) IS NOT NULL;
UPDATE listening_history SET ended_at = strftime('%Y-%m-%dT%H:%M:%fZ', ended_at)
 WHERE ended_at GLOB '[0-9][0-9][0-9][0-9]-*' AND strftime('%Y-%m-%dT%H:%M:%fZ', ended_at) IS NOT NULL;
UPDATE listening_history SET started_at = strftime('%Y-%m-%dT%H:%M:%fZ', started_at)
 WHERE started_at GLOB '[0-9][0-9][0-9][0-9]-*' AND strftime('%Y-%m-%dT%H:%M:%fZ', started_at) IS NOT NULL;
