package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// A book's .audiosilo/ attachments (metadata.AttachmentsDir) are the files the
// Bridge generates for it: characters, recaps, a description, chapters found in
// the narration and their provenance. They are not a metadata layer: nothing in
// the effective row reads them but the chapters, a chapter source of their own
// (applyChapterSource). The scan stores them checked in book_attachments
// (migrations 0048, 0050, 0051), with the files' signature on the book row
// (attachments_sig) and the indexed_at they were read with
// (attachments_indexed_at), and /meta serves the generated content beside the
// community data. Derived from the files, so a move never carries them and a
// re-index reads them again.

// AttachmentsReading is a book's attachments as a scan found them: their
// signature ("" for none) and what they say. Unread is a reading that couldn't
// be had right now (the folder couldn't be listed, or a file couldn't be read):
// UpsertBook then keeps what is stored and leaves the row unchecked, so the next
// scan reads them again rather than taking the book's generated content away.
type AttachmentsReading struct {
	Sig    string
	Files  metadata.AttachmentFiles
	Unread bool
	// Was, when set, is the row's state the reading was compared with (a scan's
	// Signatures, read when it began): SetAttachments records the reading only
	// while the row still holds it, so a scan never lays an older reading over
	// a fresher one recorded since (the Bridge's attachments refresh). Nil
	// records it whatever the row holds.
	Was *AttachmentsState
}

// AttachmentsState is a books row's attachments columns as a scan read them:
// the signature, and whether they needed reading anyway (Signature's
// AttachmentsSig and AttachmentsUnchecked).
type AttachmentsState struct {
	Sig       string
	Unchecked bool
}

// sigParts is which generated parts a book's attachments signature
// (attachments_sig) names: Book.GeneratedParts, read from the narrow column so
// a list never joins book_attachments. An allow-list of the three files that
// are generated content: bridge.json (provenance) is none, and so is any name
// a later server adds until it is listed here. A file that failed its checks
// still counts (its signature is recorded so it isn't read again): a /meta read
// then finds nothing, which costs a request and no more.
func sigParts(sig string) metadata.AttachmentParts {
	var p metadata.AttachmentParts
	for _, name := range metadata.AttachmentSigNames(sig) {
		switch name {
		case metadata.AttachmentCharacters:
			p.Characters = true
		case metadata.AttachmentRecaps:
			p.Recaps = true
		case metadata.AttachmentDescription:
			p.Description = true
		}
	}
	return p
}

// attachmentsUncheckedExpr is a books row's Signature.AttachmentsUnchecked: a
// book whose attachments weren't read since the columns existed (NULL), or were
// read for another index of the row (an older server re-indexed it).
const attachmentsUncheckedExpr = `(attachments_indexed_at IS NOT indexed_at)`

// writeAttachments replaces book id's book_attachments row with files, inside
// tx: none when there is nothing to store.
func writeAttachments(ctx context.Context, tx *sql.Tx, id int64, files metadata.AttachmentFiles, now string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM book_attachments WHERE book_id = ?`, id); err != nil {
		return err
	}
	if files.Empty() {
		return nil
	}
	chars, err := encodeAttachment(files.Characters)
	if err != nil {
		return err
	}
	recaps, err := encodeAttachment(files.Recaps)
	if err != nil {
		return err
	}
	desc, err := encodeAttachment(files.Description)
	if err != nil {
		return err
	}
	chapters, err := encodeAttachment(files.Chapters)
	if err != nil {
		return err
	}
	prov, err := encodeAttachment(files.Provenance)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO book_attachments(book_id, characters, recaps, description, chapters, provenance, updated_at) VALUES(?,?,?,?,?,?,?)`,
		id, chars, recaps, desc, chapters, prov, now)
	return err
}

// storedChapters is book id's stored chapters.json (its book_attachments
// column), "" for none.
func storedChapters(ctx context.Context, q querier, id int64) (string, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT chapters FROM book_attachments WHERE book_id = ?), '')`, id).Scan(&raw)
	return raw, err
}

// encodeAttachment is a book_attachments column's value: v as JSON, "" for nil.
func encodeAttachment[T any](v *T) (string, error) {
	if v == nil {
		return "", nil
	}
	raw, err := json.Marshal(v)
	return string(raw), err
}

// decodeAttachment reads a book_attachments column's value back: nil for "" or
// one that no longer decodes (written by a later server, say).
func decodeAttachment[T any](raw string) *T {
	if raw == "" {
		return nil
	}
	var v T
	if json.Unmarshal([]byte(raw), &v) != nil {
		return nil
	}
	return &v
}

// SetAttachments records the attachments of the books at these paths (a scan's
// read of a book whose audio is unchanged but whose attachments are new,
// changed, gone or not read for this index, and the Bridge's attachments-only
// rescan). A reading with Was is recorded only while the row still holds that
// state (compare and set), else skipped: something recorded a newer reading
// since. It commits every pathReadingBatch books. A book whose chapters.json
// changed (came, went or says something else) is re-resolved (refreshEffective:
// the generated chapters are a chapter source) and announced (c.changed); the
// rest of the files are read by nothing in the effective row.
func (c *Catalog) SetAttachments(ctx context.Context, libraryID int64, readings map[string]AttachmentsReading) error {
	now := c.ts()
	return c.eachPathBatch(ctx, "SetAttachments", slices.Collect(maps.Keys(readings)),
		func(tx *sql.Tx, relPath string) (bool, error) {
			id, err := bookIDByPath(ctx, tx, libraryID, relPath)
			if errors.Is(err, ErrNotFound) {
				return false, nil // gone since it was read
			}
			if err != nil {
				return false, err
			}
			r := readings[relPath]
			q, args := `UPDATE books SET attachments_sig = ?, attachments_indexed_at = indexed_at WHERE id = ?`, []any{r.Sig, id}
			if r.Was != nil {
				unchecked := 0
				if r.Was.Unchecked {
					unchecked = 1
				}
				q += ` AND attachments_sig = ? AND ` + attachmentsUncheckedExpr + ` = ?`
				args = append(args, r.Was.Sig, unchecked)
			}
			res, err := tx.ExecContext(ctx, q, args...)
			if err != nil {
				return false, fmt.Errorf("%s: %w", relPath, err)
			}
			if n, err := res.RowsAffected(); err != nil || n == 0 {
				return false, err // a newer reading was recorded since: it stays
			}
			was, err := storedChapters(ctx, tx, id)
			if err != nil {
				return false, err
			}
			chapters, err := encodeAttachment(r.Files.Chapters)
			if err != nil {
				return false, err
			}
			if err := writeAttachments(ctx, tx, id, r.Files, now); err != nil {
				return false, fmt.Errorf("%s: %w", relPath, err)
			}
			if chapters == was {
				return false, nil
			}
			if err := refreshEffective(ctx, tx, id); err != nil {
				return false, fmt.Errorf("%s: %w", relPath, err)
			}
			return true, nil
		})
}

// BookAttachments returns the stored attachments of book id (books.id, read
// inside the server only) with the generated parts want names, and its
// provenance, or nil, nil when that leaves nothing. A part want leaves out is
// never copied or decoded (characters and recaps run to tens of KB), though
// SQLite still pages past it to the provenance and description stored after
// it. A column that no longer decodes reads as a file the book doesn't have.
func (c *Catalog) BookAttachments(ctx context.Context, bookID int64, want metadata.AttachmentParts) (*metadata.AttachmentFiles, error) {
	// A part left out reads as none; the column names are constants.
	col := func(name string, on bool) string {
		if on {
			return name
		}
		return "''"
	}
	var chars, recaps, desc, prov string
	err := c.db.QueryRowContext(ctx,
		`SELECT `+col("characters", want.Characters)+`, `+col("recaps", want.Recaps)+`, `+
			col("description", want.Description)+`, provenance FROM book_attachments WHERE book_id = ?`, bookID).
		Scan(&chars, &recaps, &desc, &prov)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a := &metadata.AttachmentFiles{
		Characters:  decodeAttachment[model.Characters](chars),
		Recaps:      decodeAttachment[model.Recaps](recaps),
		Description: decodeAttachment[model.Description](desc),
		Provenance:  decodeAttachment[metadata.AttachmentProvenance](prov),
	}
	if a.Empty() {
		return nil, nil
	}
	return a, nil
}

// BookAttachmentParts reports which generated parts book id has stored, without
// reading them (the Bridge estimate's has.local). A stored column is a checked
// file the server wrote, so a non-empty one is a part the book has.
// octet_length of a bare TEXT column comes from the record header, so the
// values themselves (and their overflow pages) are never loaded, as comparing
// each with an empty string would; the columns are NOT NULL text defaulting to
// an empty string, so the two agree.
func (c *Catalog) BookAttachmentParts(ctx context.Context, bookID int64) (metadata.AttachmentParts, error) {
	var p metadata.AttachmentParts
	err := c.db.QueryRowContext(ctx,
		`SELECT octet_length(characters) > 0, octet_length(recaps) > 0, octet_length(description) > 0
		   FROM book_attachments WHERE book_id = ?`, bookID).
		Scan(&p.Characters, &p.Recaps, &p.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return metadata.AttachmentParts{}, nil
	}
	return p, err
}
