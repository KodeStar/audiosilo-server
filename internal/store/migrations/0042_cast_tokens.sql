-- Cast tokens (player redesign Phase 8, capability cast): a short-lived credential
-- a player mints (POST /cast/token) and hands to a Chromecast receiver instead of
-- its own session token. It works only as the ?token= of GET /libraries/{id}/cover
-- and /stream, only for the one book it was minted for (that path or a file
-- inside it), only while it has not expired and its user still exists, is enabled
-- and may see the book (auth.ResolveCastToken, api.castAuthenticate).
--
-- Only the SHA-256 hash of the secret is stored, like tokens. Path-keyed with no
-- FK to the rebuildable books index; the FK to users purges a person's tokens
-- with the account. expires_at and created_at are fixed-width millisecond UTC, so
-- they compare as text: a mint deletes the expired rows and keeps a person's
-- newest 20.
CREATE TABLE cast_tokens (
    id         INTEGER PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id INTEGER NOT NULL,
    rel_path   TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX idx_cast_tokens_user ON cast_tokens(user_id, id);
CREATE INDEX idx_cast_tokens_expires ON cast_tokens(expires_at);
