-- Listening in the house (player redesign Phase 8, capability household): whether
-- a person shares what they are listening to with the other people on this server
-- (GET/PUT /me/household). Off by default: no row reads as off. Reciprocal: only
-- a person who shares sees the others who share (catalog.HouseListening).
--
-- Durable user state, not part of the rebuildable index: it names no book, so
-- nothing moves it. The FK to users purges it with the account; backups (VACUUM
-- INTO) carry it like every other table. updated_at is fixed-width millisecond UTC.
CREATE TABLE household_sharing (
    user_id         INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    share_listening INTEGER NOT NULL CHECK (share_listening IN (0, 1)),
    updated_at      TEXT NOT NULL
);
