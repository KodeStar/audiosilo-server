-- Record each book's audio codec (from ffprobe) so the API can tell clients
-- whether a file plays natively or needs on-the-fly transcoding. Empty when
-- ffprobe is unavailable or the book predates this column (treated as playable;
-- the client falls back to ?transcode=1 if direct playback fails).
ALTER TABLE books ADD COLUMN codec TEXT NOT NULL DEFAULT '';
