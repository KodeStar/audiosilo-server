package meta

import (
	"context"
	"strings"
)

// LibriVox L6: adding a LibriVox recording to a library. The admin searches the
// community metadata database's LibriVox works (never librivox.org), opens one,
// picks one of its LibriVox recordings, and the server queues the Bridge's
// librivox.fetch with the recording's identity resolved here
// (LibrivoxRecordingOf). None of these reads is cached: they are the console's,
// one admin at a time.

// SourceLibrivox is metaserve's source type of a LibriVox recording (its
// ?source= filter).
const SourceLibrivox = "librivox"

// LibrivoxHit is one work of a LibriVox search: a work with at least one
// LibriVox recording.
type LibrivoxHit struct {
	WorkID  string          `json:"work_id"`
	Title   string          `json:"title"`
	Authors []MetaPersonRef `json:"authors"`
	// Language is the work's language tag ("en").
	Language string `json:"language"`
	// ReleaseDate is the earliest release date of the work's recordings
	// (YYYY, YYYY-MM or YYYY-MM-DD), "" when none states one.
	ReleaseDate string `json:"release_date"`
}

// LibrivoxSearch runs a work search narrowed to the works with a LibriVox
// recording (and to lang, a language tag, when not ""), at most limit hits,
// best first. The caller checks q, lang and limit (metaserve answers a
// malformed one with a 400, an outage here).
func (s *Service) LibrivoxSearch(ctx context.Context, q, lang string, limit int) ([]LibrivoxHit, error) {
	res, err := s.client.searchWorks(ctx, q, limit, searchFilter{Source: SourceLibrivox, Lang: lang})
	if err != nil {
		return nil, err
	}
	out := make([]LibrivoxHit, 0, len(res.Results))
	for _, r := range res.Results {
		if r.ID == "" {
			continue
		}
		out = append(out, LibrivoxHit{WorkID: r.ID, Title: r.Title, Authors: toPersonRefs(r.Authors),
			Language: r.Language, ReleaseDate: r.ReleaseDate})
	}
	return out, nil
}

// LibrivoxWorkHead is the work a LibrivoxWork lists the recordings of.
type LibrivoxWorkHead struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	Authors  []MetaPersonRef `json:"authors"`
	Language string          `json:"language"`
}

// LibrivoxRecording is one LibriVox recording of a work: its community
// recording id and the LibriVox project it IS, its readers, length, sections,
// cover and catalogue page (each URL only when it is an absolute http(s) one).
type LibrivoxRecording struct {
	RecordingID  string          `json:"recording_id"`
	LibrivoxID   int             `json:"librivox_id"`
	Narrators    []MetaPersonRef `json:"narrators"`
	RuntimeMin   int             `json:"runtime_min"`
	ChapterCount int             `json:"chapter_count"`
	CoverURL     string          `json:"cover_url"`
	URL          string          `json:"url"`
}

// LibrivoxWork is a work and its LibriVox recordings only (a store recording
// of the same work is left out: nothing here can add one).
type LibrivoxWork struct {
	Work       LibrivoxWorkHead    `json:"work"`
	Recordings []LibrivoxRecording `json:"recordings"`
}

// LibrivoxWork fetches the work id (caller-chosen: bounded by the uncached
// work fetches' slots) and its LibriVox recordings, in the work's order: those
// that name a LibriVox project (librivox_id), never one picked as a work's
// first recording. ErrNotFound for an unknown work.
func (s *Service) LibrivoxWork(ctx context.Context, id string) (*LibrivoxWork, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, ErrNotFound
	}
	d, err := s.fetchWork(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &LibrivoxWork{
		Work:       LibrivoxWorkHead{ID: d.ID, Title: d.Title, Authors: toPersonRefs(d.Authors), Language: d.Language},
		Recordings: []LibrivoxRecording{},
	}
	for i := range d.Recordings {
		r := &d.Recordings[i]
		lv := librivoxOf(r)
		if lv == nil || r.ID == "" {
			continue
		}
		out.Recordings = append(out.Recordings, LibrivoxRecording{
			RecordingID:  r.ID,
			LibrivoxID:   lv.ID,
			Narrators:    toPersonRefs(r.Narrators),
			RuntimeMin:   r.RuntimeMin,
			ChapterCount: r.ChapterCount,
			CoverURL:     absoluteHTTPURL(r.CoverURL),
			URL:          lv.URL,
		})
	}
	return out, nil
}

// LibrivoxProject is what a LibriVox project id resolves to: the community
// work and the recording that IS that project, and the work's title.
type LibrivoxProject struct {
	WorkID      string
	RecordingID string
	Title       string
}

// LibrivoxRecordingOf resolves LibriVox project id to the community work and
// the recording that IS that project (lookup?librivox_id=): the identity a
// librivox.fetch records for the book it places, with the work's title (what
// the job is called until it places a book). ErrNotFound when the catalogue
// holds no such project, or answers without a work or a recording.
func (s *Service) LibrivoxRecordingOf(ctx context.Context, id int) (LibrivoxProject, error) {
	l, err := s.client.lookupLibrivox(ctx, id)
	if err != nil {
		return LibrivoxProject{}, err
	}
	if l.Work == nil || strings.TrimSpace(l.Work.ID) == "" || strings.TrimSpace(l.RecordingID) == "" {
		return LibrivoxProject{}, ErrNotFound
	}
	return LibrivoxProject{WorkID: l.Work.ID, RecordingID: l.RecordingID, Title: l.Work.Title}, nil
}
