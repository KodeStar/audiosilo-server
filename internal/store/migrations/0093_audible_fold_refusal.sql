-- A completed backup can place files without matching its Audible title.
ALTER TABLE audible_titles ADD COLUMN fold_refusal_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE audible_titles ADD COLUMN fold_refused_at TEXT;

-- A successful import, a poll's accepted match, or undoing "Not this book"
-- resolves the refusal in the same write that accepts the match.
CREATE TRIGGER audible_title_match_clears_refusal
AFTER UPDATE OF match_library_id, match_path, match_how, rejected ON audible_titles
WHEN NEW.fold_refused_at IS NOT NULL AND NEW.match_path IS NOT NULL AND NEW.rejected = 0
BEGIN
    UPDATE audible_titles SET fold_refusal_reason = '', fold_refused_at = NULL
    WHERE account = NEW.account AND asin = NEW.asin;
END;
