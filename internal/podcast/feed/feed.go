// Package feed reads the Bridge's versioned podcast index without probing audio.
package feed

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/relpath"
)

const MaxReadBytes = 32 << 20

var ErrInvalidFeed = errors.New("invalid feed.json")
var ErrVersion = errors.New("unsupported feed.json version")

type File struct {
	Version               int       `json:"version"`
	Generator             string    `json:"generator"`
	ShowID                string    `json:"show_id"`
	FeedURL               string    `json:"feed_url"`
	FeedURLSHA256         string    `json:"feed_url_sha256"`
	PreviousFeedURLSHA256 []string  `json:"previous_feed_url_sha256"`
	Private               bool      `json:"private"`
	FeedHost              string    `json:"feed_host"`
	SubscribedAt          string    `json:"subscribed_at"`
	UpdatedAt             string    `json:"updated_at"`
	Show                  Show      `json:"show"`
	Episodes              []Episode `json:"episodes"`
	// ReadAt is when ReadFile began reading the file (zero for a File not
	// read from disk): catalog.IndexPodcastShow orders its folds by it.
	ReadAt time.Time `json:"-"`
	// Rerooted is that an episode path named another folder, so ReadFile
	// looked at the disk to read it (rerooter; inShow, but for show's own
	// folder in another spelling): Episodes depends on more than this file,
	// and a copy remembered by the file alone would go stale.
	Rerooted bool `json:"-"`
	// Unsure is that a look of the rerooter's failed as a share does when a
	// retry may get past it (metadata.TransientFault: EIO, ESTALE), so an
	// episode it would have read in show's folder may be left out of Episodes
	// for this read only. Such a read is Rerooted too.
	Unsure bool `json:"-"`
}
type Show struct {
	Title       string   `json:"title"`
	Author      string   `json:"author"`
	Description string   `json:"description"`
	Link        string   `json:"link"`
	Language    string   `json:"language"`
	Type        string   `json:"type"`
	Explicit    bool     `json:"explicit"`
	Categories  []string `json:"categories"`
	PodcastGUID string   `json:"podcast_guid"`
	Image       *Image   `json:"image"`
}
type Image struct {
	File         string `json:"file"`
	SourceSHA256 string `json:"source_sha256"`
	FetchedAt    string `json:"fetched_at"`
}
type Episode struct {
	ID              string       `json:"id"`
	GUID            string       `json:"guid"`
	Path            string       `json:"path"`
	Title           string       `json:"title"`
	Published       string       `json:"published"`
	Description     string       `json:"description"`
	Season          *int         `json:"season"`
	Episode         *int         `json:"episode"`
	EpisodeType     string       `json:"episode_type"`
	Explicit        bool         `json:"explicit"`
	DurationSeconds *int         `json:"duration_seconds"`
	Enclosure       Enclosure    `json:"enclosure"`
	ChaptersURL     string       `json:"chapters_url"`
	Transcripts     []Transcript `json:"transcripts"`
	FirstSeen       string       `json:"first_seen"`
	RemovedAt       string       `json:"removed_at"`
	Fetch           *Fetch       `json:"fetch"`
}
type Enclosure struct {
	URL    string `json:"url"`
	Type   string `json:"type"`
	Length int64  `json:"length"`
}
type Transcript struct {
	URL      string `json:"url"`
	Type     string `json:"type"`
	Language string `json:"language"`
}
type Fetch struct {
	Generation      int    `json:"generation"`
	SHA256          string `json:"sha256"`
	Bytes           int64  `json:"bytes"`
	DurationMS      int64  `json:"duration_ms"`
	ContentType     string `json:"content_type"`
	FetchedAt       string `json:"fetched_at"`
	DeletedAt       string `json:"deleted_at"`
	TimelineChanged bool   `json:"timeline_changed"`
	Codec           string `json:"codec,omitempty"`
}

// A malformed optional Fetch must not discard the feed's episode. Its zero
// value has no generation authority, so it stays visible and unready.
func (f *Fetch) UnmarshalJSON(raw []byte) error {
	type fields Fetch
	var decoded fields
	if err := json.Unmarshal(raw, &decoded); err != nil {
		*f = Fetch{}
		return nil
	}
	*f = Fetch(decoded)
	return nil
}

// ValidFetch is the authority check for a managed generation. An invalid fetch
// remains readable as metadata, but never authorizes serving or deletion.
func ValidFetch(f *Fetch) bool {
	if f == nil || f.Generation < 1 || !ValidHex(f.SHA256, 64) || f.Bytes <= 0 || f.DurationMS <= 0 {
		return false
	}
	if _, err := time.Parse(time.RFC3339, f.FetchedAt); err != nil {
		return false
	}
	if f.DeletedAt != "" {
		if _, err := time.Parse(time.RFC3339, f.DeletedAt); err != nil {
			return false
		}
	}
	return true
}

// ShowPath is the one rule for a show folder (and an episode's file name):
// one non-hidden segment. A line break is refused too, as the Bridge never
// names a folder with one and a podcast.poll path may not hold one.
func ShowPath(p string) bool {
	return p != "" && len(p) <= relpath.MaxBytes && p != "." && p != ".." && !strings.HasPrefix(p, ".") &&
		!strings.ContainsAny(p, "/\\\x00\r\n")
}
func EpisodePath(show, p string) bool {
	if len(p) > relpath.MaxBytes {
		return false
	}
	parts := strings.Split(p, "/")
	if !ShowPath(show) || len(parts) != 2 || parts[0] != show || !ShowPath(parts[1]) || path.Clean(p) != p {
		return false
	}
	switch strings.ToLower(path.Ext(p)) {
	case ".mp3", ".m4a", ".m4b", ".mp4", ".ogg", ".opus", ".flac":
		return true
	default:
		return false
	}
}
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
func timestamp(s string) string {
	t, e := time.Parse(time.RFC3339, s)
	if e != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ReadFile reads filename, show's feed.json under the library root root,
// through root (metadata.ReadRegularFile), as every read of a library file is:
// it refuses links and non-regular files, and a show folder swapped for a link
// out of the library since its check is never followed; so are its looks at
// the folders beside it (rerooter). Invalid episode paths are skipped;
// diagnostics deliberately contain no feed data or enclosure URLs. An episode
// path naming show in another Unicode spelling or case is read as show's
// (inShow), one naming a show folder renamed by hand in show (rerooter).
func ReadFile(root, filename, show string, log *slog.Logger) (*File, error) {
	if !ShowPath(show) {
		return nil, ErrInvalidFeed
	}
	readAt := time.Now()
	raw, err := metadata.ReadRegularFile(root, filename, MaxReadBytes, ErrInvalidFeed, ErrInvalidFeed)
	if err != nil {
		return nil, err
	}
	var data File
	if json.Unmarshal(raw, &data) != nil {
		return nil, ErrInvalidFeed
	}
	if data.Version > 1 {
		return nil, ErrVersion
	}
	if data.Version != 1 || len(data.Episodes) > 20000 || !ValidHex(data.ShowID, 16) || !ValidHex(data.FeedURLSHA256, 64) {
		return nil, ErrInvalidFeed
	}
	data.ReadAt = readAt
	data.Show.Title = capText(data.Show.Title, 300)
	data.Show.Author = capText(data.Show.Author, 300)
	data.Show.Description = capText(data.Show.Description, 4000)
	data.Show.Language = capText(data.Show.Language, 35)
	if data.Show.Type != "serial" {
		data.Show.Type = "episodic"
	}
	if len(data.Show.Categories) > 10 {
		data.Show.Categories = data.Show.Categories[:10]
	}
	if data.FeedURL != "" && IsPrivateFeedURL(data.FeedURL) {
		data.Private = true
	}
	if data.Private {
		data.FeedURL = ""
		data.Show.Link = ""
	}
	out := make([]Episode, 0, len(data.Episodes))
	seen := map[string]bool{}
	reroot := rerooter{root: root, dir: filepath.Dir(filename), show: show, nfc: norm.NFC.String(show), showID: data.ShowID}
	for _, e := range data.Episodes {
		e.Path = reroot.inShow(e.Path)
		e.Path = reroot.path(e)
		if !EpisodePath(show, e.Path) || seen[e.Path] {
			if log != nil {
				log.Warn("skipping invalid podcast episode path")
			}
			continue
		}
		seen[e.Path] = true
		e.Title = capText(e.Title, 500)
		if e.Title == "" {
			e.Title = "Untitled episode"
		}
		e.Description = capText(e.Description, 2000)
		e.GUID = capText(e.GUID, 1024)
		e.Published = timestamp(e.Published)
		e.FirstSeen = timestamp(e.FirstSeen)
		if e.Season != nil && *e.Season <= 0 {
			e.Season = nil
		}
		if e.Episode != nil && *e.Episode <= 0 {
			e.Episode = nil
		}
		if e.EpisodeType != "trailer" && e.EpisodeType != "bonus" {
			e.EpisodeType = "full"
		}
		if e.DurationSeconds != nil && *e.DurationSeconds < 0 {
			e.DurationSeconds = nil
		}
		if data.Private {
			e.Enclosure.URL = ""
			e.ChaptersURL = ""
			e.Transcripts = []Transcript{}
		}
		out = append(out, e)
	}
	data.Episodes = out
	data.Rerooted, data.Unsure = reroot.looked, reroot.unsure
	return &data, nil
}

// inShow is p with its folder spelled as show when the two differ only in
// Unicode normal form (NFC or NFD) or in case: one name, as the Bridge reads
// it, so every episode is read at the spelling on disk, a download or not. So
// it is while that spelling opens dir itself (a disk that ignores the
// difference: APFS, SMB; nothing else is looked at). A name in another normal
// form is read so too while it is no folder holding the show (heldElsewhere:
// gone, or another show's, on a disk that keeps the two apart); one that holds
// the show too, or can't be told, is the original dir is a copy of, and the
// rerooter lists none of the copy's episodes twice (nor, for this read, of a
// name it could not look at for now: Unsure). But for dir itself, what
// dir lists then depends on that other folder (looked). A name in another case
// is read so only as dir itself: on a disk that keeps the two apart they are
// two folders, never merged, and the rerooter reads it as any other name. Only
// one leading segment that is a valid show folder name is replaced; any other
// path is answered as given.
func (r *rerooter) inShow(p string) string {
	folder, file, ok := strings.Cut(p, "/")
	if !ok || folder == r.show || !ShowPath(folder) {
		return p
	}
	same, ok := r.same[folder]
	if !ok {
		if nfc := norm.NFC.String(folder); nfc == r.nfc {
			same = r.sameName(folder, true)
		} else {
			same = strings.EqualFold(nfc, r.nfc) && r.sameName(folder, false)
		}
		if r.same == nil {
			r.same = map[string]bool{}
		}
		r.same[folder] = same
	}
	if !same {
		return p
	}
	return r.show + "/" + file
}

// sameName is inShow's look at folder, show in another normal form (form) or
// case, beside dir: whether it is read as show. A look that fails for now
// (metadata.TransientFault) is no answer: not read as show this time (Unsure).
func (r *rerooter) sameName(folder string, form bool) bool {
	other, err := r.lstat(filepath.Join(filepath.Dir(r.dir), folder))
	if err == nil {
		self, err := r.lstat(r.dir)
		if err == nil && os.SameFile(self, other) {
			return true
		}
		if r.fault(err) {
			return false
		}
	}
	if r.fault(err) || !form {
		return false
	}
	r.looked = true
	return !r.heldAt(folder)
}

// rerooter reads an episode path of the form "<other folder>/<file>" as
// "<show>/<file>": a folder renamed by hand keeps its feed.json, which names
// the old folder until the Bridge's next poll rewrites it. It does so only for
// a downloaded episode (a valid fetch, not deleted) whose file dir, show's
// folder, holds as a regular file of the fetch's size now (the index's own
// rule for a download: one still being copied in, or changed by hand, is not
// one), and only while the folder named does not hold the show too
// (heldElsewhere: dir is a copy beside its original, whose episodes they
// are). Only one leading segment that is a valid show
// folder name is replaced, and the file name must make a valid episode path of
// show, so nothing outside show's own folder is ever named; any other path is
// answered as given, for EpisodePath to judge. An episode left out waits for
// the Bridge to rewrite feed.json: listed in a folder the Bridge has not taken
// in, one not downloaded there would be asked for there, a fetch the Bridge
// can only fail. dir is listed once, on the first path that needs it. A look
// that fails for now (metadata.TransientFault: dir's listing, the folder
// named) reads no episode in show's folder that it might have, and makes the
// read Unsure: its caller keeps what it knows rather than drop the episode.
type rerooter struct {
	root              string // the library's root, every look made through it
	dir, show, showID string
	nfc               string           // show in NFC (inShow)
	files             map[string]int64 // dir's regular files, each with its size
	held              map[string]bool  // heldElsewhere, by folder named
	same              map[string]bool  // inShow's answer, by folder named
	looked            bool             // the disk was looked at (File.Rerooted)
	unsure            bool             // a look failed for now (File.Unsure)
}

func (r *rerooter) path(e Episode) string {
	folder, file, ok := strings.Cut(e.Path, "/")
	if !ok || folder == r.show || !ShowPath(folder) || !EpisodePath(r.show, r.show+"/"+file) ||
		!ValidFetch(e.Fetch) || e.Fetch.DeletedAt != "" {
		return e.Path
	}
	r.looked = true
	if r.files == nil {
		r.files = map[string]int64{}
		d, err := metadata.ReadDirBelow(r.root, r.dir)
		r.fault(err)
		if err == nil {
			for _, en := range d.Entries {
				if !en.Type().IsRegular() {
					continue
				}
				info, err := d.Info(en)
				if err == nil && info.Mode().IsRegular() {
					r.files[en.Name()] = info.Size()
				}
				r.fault(err)
			}
			_ = d.Close()
		}
	}
	if size, ok := r.files[file]; !ok || size != e.Fetch.Bytes || r.heldAt(folder) {
		return e.Path
	}
	return r.show + "/" + file
}

// fault reports whether err is a look's failure that a retry may get past
// (metadata.TransientFault: EIO, ESTALE), and if so makes the read Unsure (and
// Rerooted: its listing depended on the disk).
func (r *rerooter) fault(err error) bool {
	if !metadata.TransientFault(err) {
		return false
	}
	r.looked, r.unsure = true, true
	return true
}

// heldAt is heldElsewhere, asked once a folder.
func (r *rerooter) heldAt(folder string) bool {
	held, ok := r.held[folder]
	if !ok {
		held = r.heldElsewhere(folder)
		if r.held == nil {
			r.held = map[string]bool{}
		}
		r.held[folder] = held
	}
	return held
}

// heldElsewhere reports whether folder, beside dir in the library root, holds
// the show too (its feed.json names showID: dir is a copy, and indexing its
// episodes would list them twice), or that can't be told: while nothing
// changes (a permission, an unusable feed.json), or for now (a look failing as
// a share does that a retry may get past, metadata.TransientFault: EIO,
// ESTALE, which makes the read Unsure, so its caller never takes the episodes
// left out for gone). A folder that is gone, no directory or has no feed.json
// holds nothing, nor does dir itself under another spelling (a rename only in
// case or Unicode form, on a file system that ignores it; inShow reads most).
func (r *rerooter) heldElsewhere(folder string) bool {
	other := filepath.Join(filepath.Dir(r.dir), folder)
	st, err := r.lstat(other)
	switch {
	case metadata.NotExist(err):
		return false
	case err != nil:
		r.fault(err)
		return true
	case !st.IsDir():
		return false
	}
	self, err := r.lstat(r.dir)
	if r.fault(err) {
		return true
	}
	if err == nil && os.SameFile(self, st) {
		return false
	}
	raw, err := metadata.ReadRegularFile(r.root, filepath.Join(other, "feed.json"), MaxReadBytes, ErrInvalidFeed, ErrInvalidFeed)
	if metadata.NotExist(err) {
		return false
	}
	r.fault(err)
	var f struct {
		ShowID string `json:"show_id"`
	}
	return err != nil || json.Unmarshal(raw, &f) != nil || f.ShowID == r.showID
}

// lookFaults holds the paths FaultForTests named, each with its error.
var lookFaults sync.Map

// FaultForTests makes the rerooter's every stat of path fail with err (nil
// clears it): for tests of a share's fault (EIO, ESTALE), which a test can't
// make a disk give. Keyed by the path, so tests running side by side don't
// touch each other's reads. It panics outside a test binary (as
// metadata.ForceFFprobeForTests does).
func FaultForTests(path string, err error) {
	if !testing.Testing() {
		panic("feed.FaultForTests called outside a test binary")
	}
	if err == nil {
		lookFaults.Delete(path)
		return
	}
	lookFaults.Store(path, err)
}

// lstat is the rerooter's os.Lstat of name, looked at through the library's
// root (metadata.LstatBelow), failing as FaultForTests asked.
func (r *rerooter) lstat(name string) (os.FileInfo, error) {
	if err, ok := lookFaults.Load(name); ok {
		return nil, &os.PathError{Op: "lstat", Path: name, Err: err.(error)}
	}
	return metadata.LstatBelow(r.root, name)
}

func ValidHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
