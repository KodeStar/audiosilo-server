-- A book split across disc folders (a CD rip: "Book/CD1", "Book/CD2", the tracks in
-- each, none in "Book") reads as one book per disc until an admin sets "Book" to one
-- book, which joins them (library.joinedBook). The scanner tells such a disc during
-- discovery (library.markSplitDiscs): split_parent is the folder holding it when that
-- folder is a disc set (library.discSets: no audio of its own, and every folder with
-- audio under it a disc-named folder directly in it, at least two; the only folders
-- a `book` override joins); '' otherwise. The Health page's split_discs issue, the
-- duplicate groups and the console's folder listing (split_discs) read it.
--
-- Every existing row starts as '': the next scan compares what it discovers with
-- the stored value (catalog.Signature.SplitParent) and records the difference
-- without re-indexing the book, so a rebuilt index and an upgraded one agree.
ALTER TABLE books ADD COLUMN split_parent TEXT NOT NULL DEFAULT '';

-- The issue lists one disc per split book, its first: the discs of one folder are
-- found by (library_id, split_parent), in rel_path order.
CREATE INDEX idx_books_split_parent ON books(library_id, split_parent, rel_path) WHERE split_parent <> '';
