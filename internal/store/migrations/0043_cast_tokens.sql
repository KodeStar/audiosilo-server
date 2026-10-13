-- Cast tokens (player redesign Phase 8, capability cast): a short-lived credential
-- a player mints (POST /cast/token) and hands to a Chromecast receiver instead of
-- its own session token. A cast token is a tokens row of kind 'cast' (hashed like
-- every token, expires_at set, 24 h), so auth.lookupToken stays the one
-- definition of a valid token; cast_grants says what it may read. It works only
-- as the ?token= of GET /libraries/{id}/cover and /stream, only for the one book
-- it was minted for (that path or a file inside it), only while the token that
-- minted it (parent_token_id, the sender's session) is still signed in
-- (auth.ResolveCastRequest).
--
-- Path-keyed with no FK to the rebuildable books index. The FKs to tokens drop
-- the grant with its token or its parent; a cast token without a grant is never
-- valid.
CREATE TABLE cast_grants (
    token_id        INTEGER PRIMARY KEY REFERENCES tokens(id) ON DELETE CASCADE,
    library_id      INTEGER NOT NULL,
    rel_path        TEXT NOT NULL,
    parent_token_id INTEGER NOT NULL REFERENCES tokens(id) ON DELETE CASCADE
);
-- The parent FK's cascade (a deleted token finds its cast tokens' grants).
CREATE INDEX idx_cast_grants_parent ON cast_grants(parent_token_id);
-- A mint deletes the expired cast tokens: expires_at is RFC3339 UTC, so it
-- compares as text.
CREATE INDEX idx_tokens_cast_expiry ON tokens(expires_at) WHERE kind = 'cast';
