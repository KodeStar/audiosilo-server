-- The revision of the scan's reading of a book's lengths (catalog.durationRev).
-- Revision 1 counts the frames of a VBR MP3 with no Xing or VBRI header, whose
-- length ffprobe and the in-process probe used to estimate from a bitrate, up
-- to ~7% off the audio (SRV-16). A book indexed before this starts at 0, so the
-- next scan indexes a book holding an MP3 once more though its files are
-- unchanged; every index from now on writes the current revision. A
-- rebuildable index value, never on the wire.
ALTER TABLE books ADD COLUMN duration_rev INTEGER NOT NULL DEFAULT 0;
