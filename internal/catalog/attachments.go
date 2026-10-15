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
// Bridge generates for it: characters, recaps and their provenance. They are not
// a metadata layer: nothing in the effective row reads them. The scan stores
// them checked in book_attachments (migration 0048), with the files' signature
// on the book row (attachments_sig) and the indexed_at they were read with
// (attachments_indexed_at), and /meta serves them beside the community data.
// Derived from the files, so a move never carries them and a re-index reads
// them again.

// AttachmentsReading is a book's attachments as a scan found them: their
// signature ("" for none) and what they say.
type AttachmentsReading struct {
	Sig   string
	Files metadata.AttachmentFiles
}

// generatedSig reports whether a book's attachments signature (attachments_sig)
// names a characters or recaps file: Book.Generated, read from the narrow column
// so a list never joins book_attachments. A file that failed its checks still
// counts (its signature is recorded so it isn't read again): a /meta read then
// finds nothing, which costs a request and no more.
func generatedSig(sig string) bool {
	for _, name := range metadata.AttachmentSigNames(sig) {
		if name == metadata.AttachmentCharacters || name == metadata.AttachmentRecaps {
			return true
		}
	}
	return false
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
	prov, err := encodeAttachment(files.Provenance)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO book_attachments(book_id, characters, recaps, provenance, updated_at) VALUES(?,?,?,?,?)`,
		id, chars, recaps, prov, now)
	return err
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
// rescan). It commits every pathReadingBatch books. Nothing in a book's
// effective row reads them, so no book is re-resolved.
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
			if _, err := tx.ExecContext(ctx,
				`UPDATE books SET attachments_sig = ?, attachments_indexed_at = indexed_at WHERE id = ?`,
				r.Sig, id); err != nil {
				return false, fmt.Errorf("%s: %w", relPath, err)
			}
			if err := writeAttachments(ctx, tx, id, r.Files, now); err != nil {
				return false, fmt.Errorf("%s: %w", relPath, err)
			}
			return false, nil
		})
}

// BookAttachments returns the stored attachments of book id (books.id, read
// inside the server only), or nil, nil when it has none. A column that no longer
// decodes reads as a file the book doesn't have.
func (c *Catalog) BookAttachments(ctx context.Context, bookID int64) (*metadata.AttachmentFiles, error) {
	var chars, recaps, prov string
	err := c.db.QueryRowContext(ctx,
		`SELECT characters, recaps, provenance FROM book_attachments WHERE book_id = ?`, bookID).Scan(&chars, &recaps, &prov)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a := &metadata.AttachmentFiles{
		Characters: decodeAttachment[model.Characters](chars),
		Recaps:     decodeAttachment[model.Recaps](recaps),
		Provenance: decodeAttachment[metadata.AttachmentProvenance](prov),
	}
	if a.Empty() {
		return nil, nil
	}
	return a, nil
}
