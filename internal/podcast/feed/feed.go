// Package feed reads the Bridge's versioned podcast index without probing audio.
package feed

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-server/internal/metadata"
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
}

func ShowPath(p string) bool {
	return p != "" && p != "." && p != ".." && !strings.HasPrefix(p, ".") && !strings.ContainsAny(p, "/\\\x00")
}
func EpisodePath(show, p string) bool {
	parts := strings.Split(p, "/")
	return len(parts) == 2 && parts[0] == show && ShowPath(parts[1]) && path.Clean(p) == p && metadata.IsAudio(p)
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

// ReadFile refuses links and non-regular files. Invalid episode paths are skipped;
// diagnostics deliberately contain no feed data or enclosure URLs.
func ReadFile(filename, show string, log *slog.Logger) (*File, error) {
	if !ShowPath(show) {
		return nil, ErrInvalidFeed
	}
	fi, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > MaxReadBytes {
		return nil, ErrInvalidFeed
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !os.SameFile(fi, st) || st.Size() > MaxReadBytes {
		return nil, ErrInvalidFeed
	}
	var data File
	dec := json.NewDecoder(io.LimitReader(f, MaxReadBytes+1))
	if err := dec.Decode(&data); err != nil {
		return nil, ErrInvalidFeed
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, ErrInvalidFeed
	}
	if data.Version > 1 {
		return nil, ErrVersion
	}
	if data.Version != 1 || len(data.Episodes) > 20000 || !validHex(data.ShowID, 16) || !validHex(data.FeedURLSHA256, 64) {
		return nil, ErrInvalidFeed
	}
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
	for _, e := range data.Episodes {
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
	return &data, nil
}
func validHex(s string, n int) bool {
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
