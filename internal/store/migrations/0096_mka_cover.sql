-- Older scans could not see Matroska attachments. Let the next scan check
-- these books once through its existing unknown-cover backfill.
UPDATE books SET has_cover = NULL WHERE format = 'mka' AND has_cover = 0;
