-- Small server-wide facts the server keeps for itself, one row per key (the admin
-- console's support card: catalog/support.go, key 'support_card'). Not settings:
-- nobody types these in config.yaml, and an AUDIOSILO_* variable never sets them.
--
-- Durable server state, not part of the rebuildable index: it names no book.
-- Backups (VACUUM INTO) carry it like every other table. value is the key's own
-- JSON; updated_at is fixed-width millisecond UTC.
CREATE TABLE server_state (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
