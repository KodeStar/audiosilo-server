package importer

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

// Payload is one ABS user's history, normalized: what an import keeps (gzipped
// JSON, catalog.ImportPayload) so a review's cutoff change and the apply never
// fetch again. It names only the books the user has history for, and nothing
// secret. Times are epoch milliseconds, positions and listening seconds.
type Payload struct {
	V         int        `json:"v"`
	Items     []Item     `json:"items"`
	Sessions  []Session  `json:"sessions"`
	Progress  []Progress `json:"progress"`
	Bookmarks []Bookmark `json:"bookmarks"`
}

// payloadVersion is Payload.V: a stored payload of another version (a shape this
// build doesn't know) is refused, and the import fetched again.
const payloadVersion = 1

// Item is an ABS book: its place on disk and its metadata. Gone: it was deleted
// from ABS, and is known only from a session's snapshot of its metadata (no path).
type Item struct {
	ID       string `json:"id"`
	RelPath  string `json:"rel_path,omitempty"`  // the book folder (or file), relative to its ABS library folder
	FullPath string `json:"full_path,omitempty"` // the same, absolute on the ABS host
	IsFile   bool   `json:"is_file,omitempty"`
	// Siblings is how many ABS items share its parent folder (it included), so a
	// single-file book can stand for its folder when it is alone there.
	Siblings int     `json:"siblings,omitempty"`
	Gone     bool    `json:"gone,omitempty"`
	Title    string  `json:"title"`
	Author   string  `json:"author"`
	Series   string  `json:"series,omitempty"`
	Sequence string  `json:"sequence,omitempty"`
	ASIN     string  `json:"asin,omitempty"`
	ISBN     string  `json:"isbn,omitempty"`
	Duration float64 `json:"duration"`
}

// Session is one ABS listening session on a book.
type Session struct {
	ItemID        string  `json:"item_id"`
	Started       int64   `json:"started"`
	Updated       int64   `json:"updated"`
	StartPos      float64 `json:"start_pos"`
	EndPos        float64 `json:"end_pos"`
	Duration      float64 `json:"duration"`
	Listened      float64 `json:"listened"`
	Device        string  `json:"device,omitempty"`
	ClientVersion string  `json:"client_version,omitempty"`
	Platform      string  `json:"platform,omitempty"`
}

// Progress is ABS's progress on one book. Finished and Started are 0 when
// unknown.
type Progress struct {
	ItemID     string  `json:"item_id"`
	Position   float64 `json:"position"`
	Duration   float64 `json:"duration"`
	Done       bool    `json:"done"`
	Started    int64   `json:"started"`
	Updated    int64   `json:"updated"`
	FinishedAt int64   `json:"finished_at"`
}

// Bookmark is one ABS bookmark.
type Bookmark struct {
	ItemID   string  `json:"item_id"`
	Position float64 `json:"position"`
	Title    string  `json:"title"`
	Created  int64   `json:"created"`
}

// isBook leaves out podcast episodes (ABS keeps them in the same lists).
func isBook(mediaType string, episodeID *string) bool {
	return (mediaType == "" || mediaType == "book") && (episodeID == nil || *episodeID == "")
}

// itemIndex is the server's items by id, with how many share each one's folder:
// built once per fetch, for every user it reads.
type itemIndex struct {
	byID     map[string]*absItem
	siblings map[folderKey]int
}

type folderKey struct{ lib, folder, dir string }

func folderOf(it *absItem) folderKey {
	return folderKey{it.LibraryID, it.FolderID, path.Dir(it.RelPath)}
}

func newItemIndex(items []absItem) *itemIndex {
	x := &itemIndex{byID: make(map[string]*absItem, len(items)), siblings: map[folderKey]int{}}
	for i := range items {
		it := &items[i]
		x.byID[it.ID] = it
		x.siblings[folderOf(it)]++
	}
	return x
}

// buildPayload normalizes what was fetched for one user: their book sessions,
// progress and bookmarks, and the items those name (all of the server's items,
// in items, are needed to count each one's siblings). A session on an item ABS
// no longer has keeps it as a gone item, from the session's metadata; progress
// or a bookmark on one (ABS deletes those with the item) is dropped.
func buildPayload(u *absUserDetail, sessions []absSession, items *itemIndex) *Payload {
	p := &Payload{V: payloadVersion,
		Items: []Item{}, Sessions: []Session{}, Progress: []Progress{}, Bookmarks: []Bookmark{}}
	// known: whether each id looked up is one of the server's items (added);
	// gone: the ids added as gone items, which addItem keeps answering false for,
	// so their progress and bookmarks stay dropped (matchAll relies on that).
	known, gone := map[string]bool{}, map[string]bool{}
	addItem := func(id string) bool {
		if done, ok := known[id]; ok {
			return done
		}
		it, ok := items.byID[id]
		if ok {
			title, author, series, seq := it.Media.Metadata.describe()
			p.Items = append(p.Items, Item{ID: id, RelPath: it.RelPath, FullPath: it.Path, IsFile: it.IsFile,
				Siblings: items.siblings[folderOf(it)],
				Title:    title, Author: author, Series: series, Sequence: seq,
				ASIN: strings.TrimSpace(it.Media.Metadata.ASIN), ISBN: strings.TrimSpace(it.Media.Metadata.ISBN),
				Duration: it.Media.Duration})
		}
		known[id] = ok
		return ok
	}
	// Sessions arrive newest first: a gone item takes the newest snapshot.
	for _, s := range sessions {
		if !isBook(s.MediaType, s.EpisodeID) || s.LibraryItemID == "" {
			continue
		}
		if !addItem(s.LibraryItemID) && !gone[s.LibraryItemID] {
			title, author, series, seq := s.MediaMetadata.describe()
			if title == "" {
				title = s.DisplayTitle
			}
			if author == "" {
				author = s.DisplayAuthor
			}
			p.Items = append(p.Items, Item{ID: s.LibraryItemID, Gone: true, Title: title, Author: author,
				Series: series, Sequence: seq, ASIN: strings.TrimSpace(s.MediaMetadata.ASIN),
				ISBN: strings.TrimSpace(s.MediaMetadata.ISBN), Duration: s.Duration})
			gone[s.LibraryItemID] = true
		}
		d := s.DeviceInfo
		p.Sessions = append(p.Sessions, Session{ItemID: s.LibraryItemID,
			Started: int64(s.StartedAt), Updated: int64(s.UpdatedAt), StartPos: s.StartTime, EndPos: s.CurrentTime,
			Duration: s.Duration, Listened: float64(s.TimeListening),
			Device:        firstNonEmpty(d.DeviceName, join(d.Manufacturer, d.Model), browserOn(d.BrowserName, d.OSName), d.ClientName),
			ClientVersion: d.ClientVersion, Platform: platformOf(d.ClientName)})
	}
	for _, mp := range u.MediaProgress {
		if !isBook(mp.MediaItemType, mp.EpisodeID) || !addItem(mp.LibraryItemID) {
			continue
		}
		pr := Progress{ItemID: mp.LibraryItemID, Position: mp.CurrentTime, Duration: mp.Duration,
			Done: mp.IsFinished, Started: int64(mp.StartedAt), Updated: int64(mp.LastUpdate)}
		if mp.FinishedAt != nil {
			pr.FinishedAt = int64(*mp.FinishedAt)
		}
		p.Progress = append(p.Progress, pr)
	}
	for _, b := range u.Bookmarks {
		if !addItem(b.LibraryItemID) {
			continue
		}
		p.Bookmarks = append(p.Bookmarks, Bookmark{ItemID: b.LibraryItemID, Position: b.Time, Title: b.Title,
			Created: int64(b.CreatedAt)})
	}
	return p
}

// seriesSeq splits a minified item's seriesName ("The Expanse #2.5", several
// joined by ", ") into the first series and its sequence.
var seriesSeq = regexp.MustCompile(`^(.*?)(?:\s+#([0-9]+(?:\.[0-9]+)?))?$`)

// describe is the metadata's title, author(s), first series and its sequence,
// from whichever shape it came in.
func (m absMetadata) describe() (title, author, series, seq string) {
	title = strings.TrimSpace(m.Title)
	author = strings.TrimSpace(m.AuthorName)
	if author == "" {
		names := make([]string, 0, len(m.Authors))
		for _, a := range m.Authors {
			names = append(names, a.Name)
		}
		author = strings.Join(names, ", ")
	}
	if len(m.Series) > 0 {
		return title, author, strings.TrimSpace(m.Series[0].Name), strings.TrimSpace(m.Series[0].Sequence)
	}
	first, _, _ := strings.Cut(m.SeriesName, ", ")
	if parts := seriesSeq.FindStringSubmatch(strings.TrimSpace(first)); parts != nil {
		series, seq = parts[1], parts[2]
	}
	return title, author, series, seq
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func join(a, b string) string { return strings.TrimSpace(a + " " + b) }

func browserOn(browser, os string) string {
	if browser == "" || os == "" {
		return browser
	}
	return browser + " on " + os
}

// platformOf is the AudioSilo platform name of an ABS client ("Abs iOS").
func platformOf(client string) string {
	l := strings.ToLower(client)
	switch {
	case strings.Contains(l, "ios"):
		return "ios"
	case strings.Contains(l, "android"):
		return "android"
	case strings.Contains(l, "web"):
		return "web"
	}
	return ""
}

// maxPayload bounds a stored payload once unzipped.
const maxPayload = 512 << 20

// encodePayload is p as the catalog stores it (gzipped JSON).
func encodePayload(p *Payload) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(p); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodePayload reads a stored payload.
func decodePayload(b []byte) (*Payload, error) {
	if len(b) == 0 {
		return nil, errors.New("import has no payload")
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	var p Payload
	if err := json.NewDecoder(io.LimitReader(zr, maxPayload)).Decode(&p); err != nil {
		return nil, err
	}
	if p.V != payloadVersion {
		return nil, fmt.Errorf("import payload version %d, this server reads %d: start the import again", p.V, payloadVersion)
	}
	return &p, nil
}
