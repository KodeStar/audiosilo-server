-- Where a custom cover came from, as book_overrides.source records it for a field:
-- 'edited' (uploaded by an admin) or 'community' (taken from a community match, in
-- the match dialog or by a bulk match run). Clearing community matches
-- (catalog.ClearCommunityMatches) removes the community ones with the community
-- overrides. The column goes after the blob, so only a read of it reads past the
-- image; the partial index lists the community covers without touching any blob.
ALTER TABLE book_covers ADD COLUMN source TEXT NOT NULL DEFAULT 'edited';
CREATE INDEX idx_book_covers_community ON book_covers(library_id, path) WHERE source = 'community';

-- The covers kept before this column existed, as far as the server's own records
-- tell (conservative: a cover it can't place stays 'edited', which a clear keeps).
-- The match dialog audits each cover it saves (book.cover_set, target
-- "<library name>: <path>", details.source "community"); a cover whose newest such
-- record is a community one, made when this cover was saved or since (so not one
-- left by an earlier cover at the path, before an upload was moved there), is one.
-- The times compare as instants (julianday), as the audit's milliseconds and a
-- cover's nanoseconds don't compare as text. A bulk run saves a cover only for a
-- book with none and audits no book: a cover saved since a match run that applied
-- this book a cover to take (match mode, a scope that takes covers, no
-- cover_failed), with no audited cover save for the book since that run began, is
-- one too. The two indexes exist only for this pass (neither table has one on these
-- columns), so each cover's lookups seek instead of scanning up to 100k audit rows.
CREATE INDEX tmp_audit_cover_set ON audit_events(action, target);
CREATE INDEX tmp_match_items_path ON match_run_items(library_id, path);
UPDATE book_covers SET source = 'community'
 WHERE (SELECT json_extract(e.details, '$.source')
          FROM audit_events e JOIN libraries l ON l.id = book_covers.library_id
         WHERE e.action = 'book.cover_set' AND e.target = l.name || ': ' || book_covers.path
           AND julianday(e.at) >= julianday(book_covers.updated_at)
         ORDER BY e.id DESC LIMIT 1) = 'community'
    OR EXISTS(SELECT 1 FROM match_run_items i JOIN match_runs r ON r.id = i.run_id
               WHERE i.library_id = book_covers.library_id AND i.path = book_covers.path
                 AND i.applied = 'applied' AND i.detail <> 'cover_failed'
                 AND r.mode = 'match' AND r.scope IN ('fill', 'overwrite')
                 AND json_extract(i.proposal, '$.cover_url') <> ''
                 AND book_covers.updated_at >= r.started_at
                 AND NOT EXISTS(SELECT 1 FROM audit_events e JOIN libraries l ON l.id = i.library_id
                                 WHERE e.action = 'book.cover_set'
                                   AND e.target = l.name || ': ' || i.path
                                   AND e.at >= r.started_at));
DROP INDEX tmp_audit_cover_set;
DROP INDEX tmp_match_items_path;
