package catalog

import (
	"context"

	"github.com/kodestar/audiosilo-meta/pkg/model"
)

// Integration stub: replaced by the attachments builder's catalog code.

type Attachments struct {
	Characters *model.Characters
	Recaps     *model.Recaps
	Provenance *AttachmentProvenance
	UpdatedAt  string
}

type AttachmentProvenance struct {
	Generator, Version, Kind, GeneratedAt, Source, WorkID string
	JobID                                                 int64
}

func (c *Catalog) BookAttachments(ctx context.Context, libraryID int64, relPath string) (*Attachments, error) {
	return nil, nil
}
