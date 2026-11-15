-- server#235: Sign in with OpenID Connect.
--
-- users.email: an address an admin typed (People) or a sign-in's verified email
-- copied onto an account it created. Not unique: two accounts may share one, and
-- a sign-in counts the accounts with its address when it decides (auth/identities.go).
-- The partial index serves that count; NOCASE is SQLite's ASCII case folding, the
-- compare the decision uses (emails are stored trimmed).
ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_users_email ON users(email COLLATE NOCASE) WHERE email <> '';

-- An account's identity at an OpenID provider: (issuer, subject) signs in as
-- user_id. One identity per provider per account. email and username are the
-- provider's as last seen (shown in the console, never matched on again);
-- link_method says how it was linked: auto_email (a verified email matching one
-- eligible account), admin (the console's Link) or created (a new account made
-- for it). groups_seen: a sign-in through it has carried the groups claim
-- (even an empty list), so from then on a sign-in without one means no groups
-- (auth.OIDCSignIn, oidc.Decide). Goes with its account; the admin who linked
-- it may go first.
CREATE TABLE user_identities (
    id              INTEGER PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    issuer          TEXT NOT NULL,
    subject         TEXT NOT NULL,
    email           TEXT NOT NULL DEFAULT '',
    username        TEXT NOT NULL DEFAULT '',
    link_method     TEXT NOT NULL CHECK (link_method IN ('auto_email', 'admin', 'created')),
    linked_by       INTEGER REFERENCES users(id) ON DELETE SET NULL,
    linked_at       TEXT NOT NULL,
    last_sign_in_at TEXT,
    groups_seen     INTEGER NOT NULL DEFAULT 0,
    UNIQUE (issuer, subject),
    UNIQUE (user_id, issuer)
);
-- Serves ON DELETE SET NULL when the linking admin's account goes.
CREATE INDEX idx_user_identities_linked_by ON user_identities(linked_by) WHERE linked_by IS NOT NULL;

-- Sign-ins the server could not place on an account, for an admin to link,
-- create or dismiss. One row per (issuer, subject), upserted on each attempt;
-- capped and pruned by code (auth.maxPendingSignIns, 30 days). groups is the
-- provider's list as JSON, capped. Never a token or a code. AUTOINCREMENT: a
-- dismissed row's id is never handed to another sign-in, so a console list
-- read before the dismissal can't link or create a different person.
CREATE TABLE oidc_pending (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    issuer         TEXT NOT NULL,
    subject        TEXT NOT NULL,
    email          TEXT NOT NULL DEFAULT '',
    email_verified INTEGER NOT NULL DEFAULT 0,
    username       TEXT NOT NULL DEFAULT '',
    name           TEXT NOT NULL DEFAULT '',
    groups         TEXT NOT NULL DEFAULT '[]',
    reason         TEXT NOT NULL,
    first_seen_at  TEXT NOT NULL,
    last_seen_at   TEXT NOT NULL,
    attempts       INTEGER NOT NULL DEFAULT 1,
    UNIQUE (issuer, subject)
);
CREATE INDEX idx_oidc_pending_seen ON oidc_pending(last_seen_at);
