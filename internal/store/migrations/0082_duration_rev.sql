-- The revision of the scan's reading of a book's lengths (catalog.durationRev).
-- Revision 1 counts the frames of a VBR MP3 with no Xing or VBRI header, whose
-- length ffprobe and the in-process probe used to estimate from a bitrate, up
-- to ~7% off the audio (SRV-16). A book indexed before this starts at 0, so the
-- next scan indexes a book holding an MP3 once more though its files are
-- unchanged; every index from now on writes the current revision.
-- duration_rev_indexed_at is the indexed_at the revision was written with:
-- NULL or another index's (an older server re-indexed the row, storing its own
-- estimate and leaving the revision) counts as an older revision too, as
-- file_order_indexed_at does for the file order (0055). A rebuildable index
-- value, never on the wire.
ALTER TABLE books ADD COLUMN duration_rev INTEGER NOT NULL DEFAULT 0;
ALTER TABLE books ADD COLUMN duration_rev_indexed_at TEXT;
