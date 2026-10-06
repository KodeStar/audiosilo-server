-- A person's listening goal (player redesign Phase 1b, GET/PUT/DELETE /me/goal):
-- how many books they mean to finish in a calendar year. One row per user, set by
-- that user; progress toward it is counted from progress.finished_at, so nothing
-- else is stored.
--
-- Durable user state, not part of the rebuildable index: it names no book, so it
-- has no library or path and nothing moves it. The FK to users purges it with
-- the account; backups (VACUUM INTO) carry it like every other table.
CREATE TABLE listening_goals (
    user_id        INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    books_per_year INTEGER NOT NULL CHECK (books_per_year BETWEEN 1 AND 1000),
    updated_at     TEXT NOT NULL
);
