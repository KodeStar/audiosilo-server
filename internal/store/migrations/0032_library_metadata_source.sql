-- Where a library's books take their title, author, series and position from first:
-- 'tags' (the audio files' tags, the path filling what they leave empty; the
-- default, and how every library read before this) or 'path' (the folder layout,
-- metadata.FromPathLayout, the tags filling what it leaves empty). It is applied
-- where a book's effective values are worked out (catalog bookLayers.resolve) from
-- the scan's stored snapshot, so changing it re-resolves the library's books
-- without reading a file.
ALTER TABLE libraries ADD COLUMN metadata_source TEXT NOT NULL DEFAULT 'tags';
