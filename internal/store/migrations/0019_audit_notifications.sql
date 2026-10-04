-- Admin console Phase 5b: the audit log, notification destinations and the server's
-- event feed. Backups need no table: they are files (see internal/backup).

-- One row per admin action (catalog.RecordAudit), newest kept for a year. The actor
-- is copied (id and name, no FK), so deleting an account keeps its history, and
-- "via" says how it was signed in: "session" (the console or another signed-in app),
-- "api" (a personal API key) or "system" (the server itself, e.g. a restore it
-- applied at start). action is a code like "user.update" the console words; target
-- is a short human label (a username, a library name, a book path); details is a
-- small JSON object of what changed. No IP address is kept (Phase 4a: an address is
-- only "last seen from" on a device), and no secret ever goes into details.
CREATE TABLE audit_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    at         TEXT NOT NULL,
    actor_id   INTEGER,
    actor_name TEXT NOT NULL DEFAULT '',
    via        TEXT NOT NULL DEFAULT 'session',
    action     TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    details    TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_audit_at ON audit_events(at);
CREATE INDEX idx_audit_actor ON audit_events(actor_id, id);

-- Where the server sends notifications: a webhook, an ntfy topic or a Discord
-- webhook. url and secret are credentials (a Discord webhook URL carries its token,
-- an ntfy topic is its own password, secret signs webhook bodies or is the ntfy
-- access token): the API never returns either, only a redacted address. events is
-- a JSON array of event kinds (notify.Kinds). last_* is the newest delivery's
-- outcome, for the console.
CREATE TABLE notification_targets (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT NOT NULL,
    name         TEXT NOT NULL,
    url          TEXT NOT NULL,
    secret       TEXT NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    events       TEXT NOT NULL DEFAULT '[]',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    last_at      TEXT,
    last_ok      INTEGER,
    last_error   TEXT NOT NULL DEFAULT ''
);

-- What happened that an admin may want to hear about (a book added, a library gone
-- offline, a new device): the console's bell, and the record notifications are sent
-- from. data is the event's facts as JSON (never a secret, never an IP); dedup_key
-- lets an event be announced once (the release version an update was announced for).
-- Kept for 90 days.
CREATE TABLE server_events (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    at        TEXT NOT NULL,
    kind      TEXT NOT NULL,
    data      TEXT NOT NULL DEFAULT '{}',
    dedup_key TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_server_events_at ON server_events(at);
CREATE INDEX idx_server_events_dedup ON server_events(kind, dedup_key);
