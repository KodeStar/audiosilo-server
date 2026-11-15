package api

import (
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Edited chapters (server#240): the console's chapter editor saves a book's own
// chapter list (catalog.SaveChapterEdit) and reverts it. Admin only, like the
// rest of the book page's writes; both answer the book page.

// maxChapterEditBody bounds a PUT of an edit: 2000 chapters with long titles
// and a basis of many files fit well inside it.
const maxChapterEditBody = 4 << 20

// chapterEditRequest is the body of PUT .../book/chapters: the basis the book
// page gave (AdminBookDetail.chapter_basis) and the chapters, each a start on
// the whole-book timeline in seconds and a title.
type chapterEditRequest struct {
	Basis    *catalog.ChapterBasis   `json:"basis"`
	Chapters []catalog.EditedChapter `json:"chapters"`
}

// handleAdminEditChapters serves PUT /admin/libraries/{id}/book/chapters?path=:
// store the book's own chapter list and answer the book page. 400
// chapters_invalid with a detail (catalog.ChapterEdit*) for chapters that
// don't hold on the book's files, 409 chapters_basis when the files are not
// the ones the basis names (the page is out of date), 409 not_book_library for
// a podcast episode, 404 for no book.
func (a *API) handleAdminEditChapters(w http.ResponseWriter, r *http.Request) {
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var req chapterEditRequest
	if err := decodeJSON(r, &req, maxChapterEditBody); err != nil || req.Basis == nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	err := a.cat.SaveChapterEdit(r.Context(), lib.ID, p, *req.Basis, req.Chapters, userFrom(r.Context()).ID)
	if err != nil {
		a.writeBookError(w, err, "edit chapters failed", "could not save the chapters", "library", lib.ID, "path", p)
		return
	}
	a.audit(r, "book.chapters_edited", lib.Name+": "+p, map[string]any{"chapter_count": len(req.Chapters)})
	a.writeBookDetail(w, r, lib, p)
}

// handleAdminRevertChapters serves DELETE
// /admin/libraries/{id}/book/chapters?path=: remove the book's own chapter
// list (its chapters come from the next source again) and answer the book
// page. A book with none is answered as it is (nothing is audited). 404 for no
// book.
func (a *API) handleAdminRevertChapters(w http.ResponseWriter, r *http.Request) {
	lib, p, status, msg := a.authorizedPath(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	removed, err := a.cat.RevertChapterEdit(r.Context(), lib.ID, p)
	if err != nil {
		a.writeBookError(w, err, "revert chapters failed", "could not revert the chapters", "library", lib.ID, "path", p)
		return
	}
	if removed {
		a.audit(r, "book.chapters_reverted", lib.Name+": "+p, nil)
	}
	a.writeBookDetail(w, r, lib, p)
}
