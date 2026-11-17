-- A share is marked as a library grant (whole_library_id, 0015) only while it
-- holds that library's whole-library rule. Older versions let the rule go and
-- left the mark: a path edit on a grant (refused since server#254), an
-- interrupted grant, or deleting the library of a grant that also held another
-- library's folders. Such a share gives the folders it holds, yet the mark kept
-- its folders closed to any change (409 library_grant) and the console read
-- only. It is an ordinary share again: catalog.DeleteLibrary now clears the
-- mark itself.
UPDATE shares
   SET whole_library_id = NULL
 WHERE whole_library_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM share_paths sp
                    WHERE sp.share_id = shares.id
                      AND sp.library_id = shares.whole_library_id
                      AND sp.path = '');
