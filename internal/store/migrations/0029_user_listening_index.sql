-- Player redesign Phase 1b: a person's own listening stats (GET /me/stats,
-- /me/listening) read one user's sessions overlapping a period: user_id = ? AND
-- last_at >= ? (and started_at < ?). idx_sessions_user (user_id, id) can only
-- seek the user and then scan all of their history; this index seeks the user
-- and the period's start, as idx_sessions_last does for everyone's.
CREATE INDEX idx_sessions_user_last ON listening_sessions(user_id, last_at);
