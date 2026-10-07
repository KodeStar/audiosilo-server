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
