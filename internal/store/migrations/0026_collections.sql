-- Player redesign Phase 1b: collections, a listener's own named, ordered lists of
-- books, which the owner may share READ-ONLY with named users on the same server.

-- One row per collection. Owned by one user (deleted with them). name is 1-100
-- characters, description at most 1000 (catalog validates). updated_at moves on a
-- rename, a description change and any change to the items; created_at/updated_at
-- are RFC3339 UTC. At most catalog.MaxCollections per owner.
CREATE TABLE collections (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
CREATE INDEX idx_collections_user ON collections(user_id);

-- A collection's books, in order. Durable, path-keyed like up_next (no FK to the
-- rebuildable books index): rel_path is a book's path, carried by a move or a
-- join (catalog.carryListeningState). Each reader sees only the items THEIR
-- current access allows; the rows themselves are kept (until an add needs the
-- room, as up_next). position orders them
-- (gaps allowed, as up_next). At most catalog.MaxCollectionItems per collection.
CREATE TABLE collection_items (
    collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    library_id    INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    rel_path      TEXT NOT NULL,
    position      INTEGER NOT NULL,
    added_at      TEXT NOT NULL,
    PRIMARY KEY (collection_id, library_id, rel_path)
);
-- The scanner's carry on a move/join looks rows up by path, across collections.
CREATE INDEX idx_collection_items_path ON collection_items(library_id, rel_path);
-- A collection's items in their order (detail reads, the previews of
-- GET /me/collections, the range shift of an insert at a position).
CREATE INDEX idx_collection_items_order ON collection_items(collection_id, position);

-- Who a collection is shared with (read-only viewers). Removed with the
-- collection or the viewer; a viewer leaving deletes only their own row. At most
-- catalog.MaxCollectionShares per collection.
CREATE TABLE collection_shares (
    collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (collection_id, user_id)
);
-- "Collections shared with me".
CREATE INDEX idx_collection_shares_user ON collection_shares(user_id);
