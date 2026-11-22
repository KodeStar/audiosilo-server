-- A split set belongs to its parent folder. The old request named a disc
-- immediately below it, so even an unindexed disc can usually be placed.
-- Keep an unplaceable path under the old kind for the compatibility lookup.
CREATE TEMP TABLE old_disc_parents AS
WITH RECURSIVE parents(library_id, path, parent, rest) AS (
    SELECT library_id, path, '', path FROM issue_ignores WHERE kind = 'split_discs'
    UNION ALL
    SELECT library_id, path, parent || substr(rest, 1, instr(rest, '/')),
           substr(rest, instr(rest, '/') + 1) FROM parents WHERE instr(rest, '/') > 0
), leaves AS (
    SELECT library_id, path, rtrim(parent, '/') AS parent,
           replace(replace(replace(replace(lower(trim(rest)), ' ', ''), '-', ''), '.', ''), '_', '') AS name
    FROM parents WHERE instr(rest, '/') = 0
)
SELECT l.library_id, l.path, coalesce(nullif(b.split_parent, ''), l.parent) AS parent
FROM leaves l LEFT JOIN books b ON b.library_id = l.library_id AND b.rel_path = l.path
WHERE b.split_parent <> '' OR (l.parent <> '' AND CASE
    WHEN substr(name, 1, 2) = 'cd' THEN substr(name, 3) NOT GLOB '*[^0-9]*'
    WHEN substr(name, 1, 4) IN ('disc','disk') THEN substr(name, 5) NOT GLOB '*[^0-9]*'
    ELSE 0 END);
INSERT OR IGNORE INTO issue_ignores(library_id, path, kind, created_by, created_at)
SELECT i.library_id, p.parent, 'split_disc_set', i.created_by, i.created_at
FROM old_disc_parents p JOIN issue_ignores i USING(library_id, path)
WHERE i.kind = 'split_discs' ORDER BY i.created_at, i.path;
DELETE FROM issue_ignores WHERE kind = 'split_discs' AND (library_id, path) IN
    (SELECT library_id, path FROM old_disc_parents);
DROP TABLE old_disc_parents;

-- One decision per exact comparison. Within a library, paths is a JSON array
-- sorted by path bytes. Across libraries it holds two paths in library order.
CREATE TABLE duplicate_decisions (
    library_id       INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    other_library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    paths            TEXT NOT NULL,
    ignored          INTEGER NOT NULL DEFAULT 1,
    created_by       INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at       TEXT NOT NULL,
    PRIMARY KEY (library_id, other_library_id, paths),
    CHECK (library_id <= other_library_id AND json_array_length(paths) >= 2)
);
CREATE INDEX idx_duplicate_decisions_other ON duplicate_decisions(other_library_id);

-- IgnoreIssue stamped every new member of one request with the same instant
-- and admin. Recover that selection even when none of its books is indexed.
CREATE TEMP TABLE old_duplicate_ignores AS
    SELECT library_id, path, created_by, created_at FROM issue_ignores WHERE kind = 'duplicate';
CREATE INDEX temp.idx_old_duplicate_action ON old_duplicate_ignores(created_at, created_by, library_id);
INSERT INTO duplicate_decisions(library_id, other_library_id, paths, created_by, created_at)
SELECT library_id, library_id, json_group_array(path ORDER BY path), created_by, created_at
FROM old_duplicate_ignores GROUP BY created_at, created_by, library_id HAVING count(*) >= 2;
INSERT INTO duplicate_decisions(library_id, other_library_id, paths, created_by, created_at)
SELECT a.library_id, b.library_id, json_array(a.path, b.path), a.created_by, a.created_at
FROM old_duplicate_ignores a JOIN old_duplicate_ignores b ON a.created_at = b.created_at
    AND a.created_by IS b.created_by AND a.library_id < b.library_id;

-- Keep the source rows. An old write skipped conflicts, so an action's
-- timestamp alone cannot recover every member. Only the catalogue's pass over
-- the upgrade index marks confirmed members as mapped. Missing members keep
-- the old all-members rule until an admin makes an exact decision.
-- The catalogue completes the indexed part at startup, before the scanner
-- changes any membership. A previous console could split a large group into
-- several requests, or two overlapping requests could have different stamps.
INSERT INTO server_state(key, value, updated_at)
SELECT 'map_duplicate_ignores_0092', 'true', strftime('%Y-%m-%dT%H:%M:%fZ','now')
WHERE EXISTS (SELECT 1 FROM old_duplicate_ignores);
DROP TABLE old_duplicate_ignores;
