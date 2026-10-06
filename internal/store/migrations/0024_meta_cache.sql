-- The community metadata cache's persistent second level (internal/meta.Store,
-- catalog/metacache.go): the answers the in-memory cache holds - a book's composed
-- enrichment keyed by its asin/isbn ("a:<asin>" / "i:<isbn>") and a work fetched by
-- id ("w:<id>") - kept so a restart serves them warm and a positive one outlives a
-- metaserve outage.
--
-- DERIVED AND REBUILDABLE, NOT USER STATE: every row is an upstream answer the
-- server can ask for again, keyed by an identifier rather than a book or a user, so
-- nothing moves it with a book (no MoveDurableState) or purges it with a user, and
-- dropping the table costs one upstream lookup per book. Bounded by the launcher's
-- daily retention (catalog.PruneMetaCache keeps the newest catalog.MetaCacheRows).
--
-- version is the payload's format (meta.storeVersion) and source the metaserve base
-- URL it came from; a row of another version or source is ignored, never served.
-- payload is the JSON of the answer, '' for a cached "no match". expires_at is when
-- the answer stops being fresh and stored_at when it was written (both unix
-- milliseconds); retention keeps the newest by stored_at.
CREATE TABLE meta_cache (
    key        TEXT PRIMARY KEY,
    version    INTEGER NOT NULL,
    source     TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '',
    expires_at INTEGER NOT NULL,
    stored_at  INTEGER NOT NULL
);
CREATE INDEX idx_meta_cache_stored ON meta_cache(stored_at);
