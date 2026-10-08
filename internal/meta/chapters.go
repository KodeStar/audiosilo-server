package meta

import (
	"context"
	"errors"

	"github.com/kodestar/audiosilo-server/pkg/match"
)

// Chapter is one of a community recording's chapters, timed on that recording's
// audio (milliseconds), as metaserve sends it.
type Chapter struct {
	Title    string `json:"title"`
	StartMS  int64  `json:"start_ms"`
	LengthMS int64  `json:"length_ms"`
}

// RecordingChapters is the chapter list of the recording an identifier names.
type RecordingChapters struct {
	WorkID      string
	RecordingID string
	// Chapters is empty when the community has none for the recording (or the
	// metaserve predates the chapters route).
	Chapters []Chapter
}

// RecordingChapters looks an identifier (an asin, else an isbn) up and fetches
// the chapters of the exact recording it names. ErrNotFound when there is no
// match, or the match names no recording: a work's chapters belong to one
// recording, and the first recording (Enrich's fallback) may be another edition
// altogether. Any other error is the upstream's.
//
// Nothing is cached here: the caller (the community chapters worker) keeps what
// it makes of the answer, and asks again only when that is stale.
func (s *Service) RecordingChapters(ctx context.Context, asin, isbn string) (*RecordingChapters, error) {
	asin, isbn = match.NormalizeASIN(asin), match.NormalizeISBN(isbn)
	if asin == "" && isbn == "" {
		return nil, ErrNotFound
	}
	l, err := s.lookup(ctx, asin, isbn)
	if err != nil {
		return nil, err
	}
	if l.RecordingID == "" {
		return nil, ErrNotFound
	}
	out := &RecordingChapters{WorkID: l.Work.ID, RecordingID: l.RecordingID}
	chs, err := s.client.recordingChapters(ctx, l.Work.ID, l.RecordingID)
	switch {
	case errors.Is(err, ErrNotFound):
		return out, nil // a metaserve without the route: no chapters known
	case err != nil:
		return nil, err
	}
	out.Chapters = chs
	return out, nil
}
