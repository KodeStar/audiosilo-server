-- The raw art identity a thumbnail was read from. NULL means no reading.
-- Older palettes, including the tag alone for undecodable art, hold only a
-- short hash. The catalog verifies those tags once before the colour pass.
ALTER TABLE books ADD COLUMN cover_color_art TEXT;
INSERT INTO server_state(key, value, updated_at)
SELECT 'backfill_cover_color_art_0094', 'true', strftime('%Y-%m-%dT%H:%M:%fZ','now')
WHERE EXISTS (SELECT 1 FROM books WHERE cover_color <> '');

CREATE INDEX idx_books_cover_color_due ON books(id) WHERE cover_color_art IS NOT cover_art;

-- Older servers also write cover_art and cover_color together when they read
-- a thumbnail. Keep their writes usable when the newer server runs again.
-- A cleared colour is no reading, so a write of '' puts the book back in the
-- colour pass rather than marking its art read.
CREATE TRIGGER books_legacy_cover_color AFTER UPDATE OF cover_color ON books
WHEN NEW.cover_color_art IS NOT (CASE WHEN NEW.cover_color = '' THEN NULL ELSE NULLIF(NEW.cover_art, '') END)
BEGIN
    UPDATE books SET cover_color_art = CASE WHEN NEW.cover_color = '' THEN NULL ELSE NULLIF(NEW.cover_art, '') END
     WHERE id = NEW.id;
END;

-- Names with FTS tokens already use books_fts. For other names, check only
-- nonempty narrator credits, within the selected library when there is one.
CREATE INDEX idx_books_narrator ON books(library_id, narrator) WHERE narrator > '';

-- A parked fetch frees its requester's slot. Terminal jobs can be retained.
CREATE INDEX idx_bridge_jobs_fetch_requester ON bridge_jobs(requested_by)
    WHERE kind = 'podcast.fetch' AND state IN ('queued', 'claimed', 'running');
