// Package catalog stores and queries the audiobook index: libraries, per-user
// access grants and books (with files and chapters). It keeps the FTS5 search
// table in sync on every book upsert/delete. The index is derived from a
// filesystem scan and can be rebuilt at any time.
package catalog

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// ViewHybrid is the default library view (filesystem browse + indexed metadata).
const ViewHybrid = "hybrid"

// Library is a configured root of books or podcast shows. For books, its shape (single-file books,
// folder-per-book, multi-file parts) is auto-detected per folder by the scanner;
// there is no library-wide layout setting. Folders the detector gets wrong can be
// corrected with a per-folder override (see folder_overrides.go).
type Library struct {
	Kind        string `json:"kind"`
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Root        string `json:"root"`
	DefaultView string `json:"default_view"`
	// SortOrder is the library's display order (lower first). It also breaks ties
	// when de-duplicating identical copies of a book across libraries.
	SortOrder int `json:"sort_order"`
	// ScanSchedule ("" = none) and IgnorePatterns are admin settings (see
	// library.ParseSchedule and library.ParseIgnore), kept off the player wire; the
	// admin library list carries them.
	ScanSchedule   string   `json:"-"`
	IgnorePatterns []string `json:"-"`
	// MetadataSource is where its books' title, author, series and position come
	// from first (MetadataFromTags or MetadataFromPath); an admin setting too.
	MetadataSource string `json:"-"`
}

// A library's MetadataSource: the files' tags first (the default), or the folder
// layout first (metadata.FromPathLayout). The other fills what the first leaves
// empty.
const (
	MetadataFromTags = "tags"
	MetadataFromPath = "path"
)

// ErrInvalidMetadataSource marks a library metadata source other than
// MetadataFromTags or MetadataFromPath.
var ErrInvalidMetadataSource = errors.New(`metadata_source must be "tags" or "path"`)

// checkMetadataSource defaults an unset metadata source to MetadataFromTags and
// refuses one that is neither.
func checkMetadataSource(l *Library) error {
	switch l.MetadataSource {
	case "":
		l.MetadataSource = MetadataFromTags
	case MetadataFromTags, MetadataFromPath:
	default:
		return ErrInvalidMetadataSource
	}
	return nil
}

// Book is an indexed audiobook (single file or a folder of files).
type Book struct {
	Episode     *PodcastEpisodeInfo `json:"episode,omitempty"`
	ID          int64               `json:"id"`
	LibraryID   int64               `json:"library_id"`
	RelPath     string              `json:"rel_path"`
	IsFolder    bool                `json:"is_folder"`
	Title       string              `json:"title"`
	Author      string              `json:"author"`
	Series      string              `json:"series"`
	SeriesIndex float64             `json:"series_index"`
	// SeriesIndexEnd is the end of a main series position that is a range (an
	// omnibus of books 1-3: SeriesIndex 1, SeriesIndexEnd 3); 0, and omitted,
	// when it isn't one, so a client that knows only SeriesIndex reads the start.
	SeriesIndexEnd float64 `json:"series_index_end,omitempty"`
	// SeriesList is every series the book is in, its main one (Series) first,
	// with its position in each - present only for a book in more than one (see
	// FieldMoreSeries); a client otherwise reads Series/SeriesIndex.
	SeriesList  []SeriesRef `json:"series_list,omitempty"`
	Narrator    string      `json:"narrator"`
	Duration    float64     `json:"duration"`
	ASIN        string      `json:"asin,omitempty"`
	ISBN        string      `json:"isbn,omitempty"`
	CoverPath   string      `json:"-"`
	Format      string      `json:"format"`
	Codec       string      `json:"codec,omitempty"` // audio codec (ffprobe); "" when unknown
	Size        int64       `json:"size"`
	MTime       int64       `json:"-"`
	AddedAt     string      `json:"added_at,omitempty"` // RFC3339; filesystem birth time (scanner)
	ContentHash string      `json:"-"`
	// Published (YYYY[-MM[-DD]]) and Description come only from an Audiobookshelf
	// metadata.json, an edit or a community match today (never a tag: a tag's date
	// is Released). Published rides on every player book; Description can
	// be long, so only GetBook (the single-book read behind the item endpoint)
	// loads it, leaving list, search and recent pages small.
	Published   string `json:"published,omitempty"`
	Description string `json:"description,omitempty"`
	// Subtitle, Publisher and Language come from an Audiobookshelf metadata.json,
	// an edit or a community match. Abridged likewise: nil (omitted) when nothing
	// says, so a client never reads "unknown" as "unabridged". Genres come from
	// the genre tag too (metadata.SplitGenres), Tags only from the ABS file or an
	// edit (see labels.go); both ride on every player book, list pages included,
	// so a client can filter a library it has loaded by them.
	Subtitle  string   `json:"subtitle,omitempty"`
	Publisher string   `json:"publisher,omitempty"`
	Language  string   `json:"language,omitempty"`
	Abridged  *bool    `json:"abridged,omitempty"`
	Genres    []string `json:"genres,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	// Released is the date the file's tags give (metadata.ReleaseDate), usually
	// the recording's: scanned, never edited, and only the admin list's
	// release-date sort reads it (where a book has no Published).
	Released string `json:"-"`
	// HasCover reports cover art (a sibling image or embedded art); nil until a
	// scan has checked. Set by the scanner; read by the admin catalog.
	HasCover *bool `json:"-"`
	// CoverVersion is a short opaque token that changes when the cover art does
	// (a client appends it to cover URLs as a cache-buster): the hash of
	// books.cover_art (CoverVersion), set whenever the book is indexed and when a
	// custom cover is set or removed, and moved to the art's own version by a
	// thumbnail of it. CoverColor is the cover's palette for the player's themed
	// screens, read from a thumbnail of the art (RecordCoverColors); absent until
	// one has been made for this version.
	CoverColor   *CoverColor `json:"cover_color,omitempty"`
	CoverVersion string      `json:"cover_version,omitempty"`
	// Generated is set (and true) when the book has characters, recaps or a
	// description the Bridge generated (.audiosilo/ attachments, read from
	// books.attachments_sig: GeneratedParts.Any()), so a player asks /meta only
	// for a book with an ASIN/ISBN or this; omitted otherwise.
	Generated bool `json:"generated,omitempty"`
	// GeneratedParts is which of those parts its signature names (sigParts), so
	// /meta reads only parts the book has.
	GeneratedParts metadata.AttachmentParts `json:"-"`
	// ScanError is what went wrong reading the book's files when it was last indexed
	// (a code; "" = nothing), in which file (library-relative) and the tool's
	// message. SuspectParts is how many separate books its parts look like (0 = one;
	// nil = not checked). SplitParent is the folder holding it when it is one disc of
	// a book split across disc folders ("" = not; see books.split_parent). Set by the
	// scanner; read by the Health page.
	ScanError       string `json:"-"`
	ScanErrorFile   string `json:"-"`
	ScanErrorDetail string `json:"-"`
	SuspectParts    *int   `json:"-"`
	SplitParent     string `json:"-"`
	// ABS is the Audiobookshelf metadata.json in a folder book's folder: its
	// signature from the scan's walk, then what it says from enrich. Set by the
	// scanner for folder books only; UpsertBook records it (books.abs_*).
	ABS ABSReading `json:"-"`
	// Attachments are the book's .audiosilo/ files (catalog/attachments.go): their
	// signature from the scan's walk, then what they say from enrich. Set by the
	// scanner; UpsertBook records them (books.attachments_*, book_attachments).
	Attachments AttachmentsReading `json:"-"`
	Files       []BookFile         `json:"files,omitempty"`
	Chapters    []metadata.Chapter `json:"chapters,omitempty"`
	// ChaptersSource is "community" when Chapters are a community chapter list
	// fitted onto the audio, "generated" when they are the chapters the Bridge
	// found in the narration (its chapters.json), rather than the files' own
	// (books.chapters_source); omitted otherwise. Set by GetBook (single-book
	// reads).
	ChaptersSource string `json:"chapters_source,omitempty"`

	// DirectPlayable, when set, reports whether the audio codec plays natively in
	// browsers (so the client knows when to request ?transcode=1). Computed by the
	// API for single-book responses; nil/omitted in list views.
	DirectPlayable *bool `json:"direct_playable,omitempty"`

	// DedupKey groups copies of the same logical book (across libraries, and later
	// across servers) so a client can collapse duplicates. It is a display-grouping
	// HINT, not an identity - never key durable state on it. Set on de-duplicated
	// list responses (search / recent).
	DedupKey string `json:"dedup_key,omitempty"`
	// MultiFile reports whether the book has more than one audio file (a multipart
	// book). Used to rank copies when de-duplicating (a single file beats a
	// multipart copy). Set on de-duplicated list responses; nil/omitted elsewhere.
	MultiFile *bool `json:"multi_file,omitempty"`
	// OtherLocations are the same book's other (non-winning) copies in a
	// de-duplicated list, so a client can show "also on X" and let the user switch.
	OtherLocations []BookLocation `json:"other_locations,omitempty"`
}

// CoverColor is a cover's palette (media.Palette): Bg its dominant colour; Accent
// a vibrant one that reads against Bg and OnAccent white or black for text on it,
// both "" when the cover has none.
type CoverColor = media.Palette

// BookLocation points at one copy of a book in a particular library - used to
// list the non-winning copies behind a de-duplicated search/recent result.
type BookLocation struct {
	LibraryID   int64  `json:"library_id"`
	LibraryName string `json:"library_name"`
	Path        string `json:"path"`
	Format      string `json:"format,omitempty"`
	Size        int64  `json:"size,omitempty"`
	MultiFile   bool   `json:"multi_file,omitempty"`
}

// BookFile is one audio file belonging to a multi-file book.
type BookFile struct {
	RelPath  string  `json:"rel_path"`
	Seq      int     `json:"seq"`
	Duration float64 `json:"duration"`
	Format   string  `json:"format"`
	// Codec is this part's audio codec (ffprobe); admin-console only, since the
	// player reads the book-level codec.
	Codec string `json:"-"`
	Size  int64  `json:"size"`
}

// Catalog provides indexed reads/writes over the store.
type catalogDB interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	WriteRowContext(context.Context, string, ...any) *sql.Row
	WithTx(context.Context, string, func(*sql.Tx) error) error
	Ping(context.Context) error
}

type Catalog struct {
	// PodcastChanged hears that a podcast episode's index row was written (the
	// Bridge wakes media requests waiting on it); wired before serving.
	PodcastChanged func(int64, string)
	db             catalogDB
	now            func() time.Time
	// onChange hears that a book's index row was written (OnBookChange): the
	// listeners, replaced whole (copy-on-write) by each registration.
	onChange          atomic.Pointer[[]func()]
	onChangeMu        sync.Mutex
	podcastStreamsMu  sync.Mutex
	podcastStreams    map[Ref]int
	podcastScheduleMu sync.Mutex
	podcastVisit      Ref
}

// OnBookChange adds f to the listeners that hear, after the fact and without
// blocking, that a book was indexed or edited (UpsertBook, EditBook, EditBooks,
// SetEnrichment, ClearCommunityMatches, a library's new metadata source
// re-resolving its books in UpdateLibrary) or its custom cover set or removed:
// what a background check of the books may want to look at again. f must return
// at once; nil is ignored. Safe to call while the catalog is in use.
func (c *Catalog) OnBookChange(f func()) {
	if f == nil {
		return
	}
	c.onChangeMu.Lock()
	defer c.onChangeMu.Unlock()
	var fs []func()
	if old := c.onChange.Load(); old != nil {
		fs = append(fs, *old...)
	}
	fs = append(fs, f)
	c.onChange.Store(&fs)
}

// changed tells the OnBookChange listeners.
func (c *Catalog) changed() {
	if fs := c.onChange.Load(); fs != nil {
		for _, f := range *fs {
			f()
		}
	}
}

// New returns a Catalog. now may be nil to use time.Now.
func New(db *store.DB, now func() time.Time) *Catalog {
	if now == nil {
		now = time.Now
	}
	return &Catalog{db: db, now: now}
}

// Ping verifies the underlying database is reachable for reads (backs GET /healthz).
func (c *Catalog) Ping(ctx context.Context) error { return c.db.Ping(ctx) }

// ts is the server's current time as an RFC3339Nano string. Nanosecond precision
// (rather than whole-second RFC3339) keeps two server-stamped writes in the same
// wall-clock second distinguishable for last-write-wins reconciliation; parsers
// using the plain RFC3339 layout still accept the fractional form.
func (c *Catalog) ts() string { return c.now().UTC().Format(time.RFC3339Nano) }
