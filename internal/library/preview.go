package library

import (
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// The setup flow's folder preview (FIRST-RUN-PLAN.md Phase F1): what a books
// library at a root would hold and how a few of its books would be named, read
// from the files without indexing anything. It walks as a scan discovers
// (discoverAuto, no folder overrides), reads a sample's tags and Audiobookshelf
// metadata.json as a scan does (setNames, readABS; no fingerprint, chapters or
// cover), and names each book both ways with the catalog's own resolver
// (catalog.ResolveScanned), so there is no second parser.

// ErrRootUnreadable is CheckRoot's answer for a root that is missing, not a
// folder, or not readable by the server.
var ErrRootUnreadable = errors.New("folder not found or not readable")

// errWalkCapped stops a walk that reached its walkCap.
var errWalkCapped = errors.New("walk limit reached")

// walkCap bounds a discovery walk: at most dirs folders, and none started after
// until. A nil walkCap bounds nothing.
type walkCap struct {
	dirs  int
	until time.Time
	seen  int
}

// another counts one more folder walked and reports whether the walk may go on.
func (c *walkCap) another() bool {
	if c == nil {
		return true
	}
	c.seen++
	return c.seen <= c.dirs && time.Now().Before(c.until)
}

// PreviewLimits bound a preview; a zero field takes its default.
type PreviewLimits struct {
	MaxDirs int           // folders walked (5,000)
	MaxTime time.Duration // the walk's time (10 s)
	Samples int           // books read and named (8)
}

func (l PreviewLimits) withDefaults() PreviewLimits {
	l.MaxDirs = cmp.Or(l.MaxDirs, 5000)
	l.MaxTime = cmp.Or(l.MaxTime, 10*time.Second)
	l.Samples = cmp.Or(l.Samples, 8)
	return l
}

// Preview is what a books library at Root would hold (as far as the walk got)
// and a sample of its books named both ways.
type Preview struct {
	Root   string        `json:"root"`
	Counts PreviewCounts `json:"counts"`
	// Samples are spread evenly over the books found, in path order.
	Samples []PreviewSample `json:"samples"`
	// Blanks counts, over the samples, the names each source leaves empty on its
	// own (blanks): the tags before any path fallback, and the folders as a
	// library that prefers them reads them (metadata.FromPathLayout). The
	// columns show the resolved names, so a source's blanks can be filled there
	// by the other source.
	Blanks PreviewBlanks `json:"blanks"`
	// Suggested is the metadata source with fewer blanks, the tags on a tie:
	// catalog.MetadataFromTags or catalog.MetadataFromPath.
	Suggested string `json:"suggested"`
}

// PreviewCounts are the audio files and books the walk found. Capped is set when
// it stopped at its limit, so there are more.
type PreviewCounts struct {
	AudioFiles  int  `json:"audio_files"`
	FolderBooks int  `json:"folder_books"`
	FileBooks   int  `json:"file_books"`
	Capped      bool `json:"capped"`
}

// PreviewBlanks are the names each source leaves empty.
type PreviewBlanks struct {
	Tags int `json:"tags"`
	Path int `json:"path"`
}

// PreviewSample is one book named as a library that prefers its tags would
// name it (Tags) and as one that prefers its folders would (Path). ABS is set
// when its folder holds an Audiobookshelf metadata.json, which goes over both.
type PreviewSample struct {
	RelPath string               `json:"rel_path"`
	Tags    catalog.ScannedNames `json:"tags"`
	Path    catalog.ScannedNames `json:"path"`
	ABS     bool                 `json:"abs"`
}

// CheckRoot checks a typed books folder as the add-library folder picker does
// (ListDirs): ErrNotAbsolute for a relative path, ErrRootUnreadable for one the
// server can't list. It returns the root cleaned.
func CheckRoot(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", ErrNotAbsolute
	}
	root = filepath.Clean(root)
	f, err := os.Open(root)
	if err != nil {
		return "", ErrRootUnreadable
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.IsDir() {
		return "", ErrRootUnreadable
	}
	if _, err := f.ReadDir(1); err != nil && !errors.Is(err, io.EOF) {
		return "", ErrRootUnreadable
	}
	return root, nil
}

// Preview reads root (as CheckRoot returned it, the caller having checked it
// against the other libraries' roots too) with ignore, a library's ignore rules
// (ErrInvalidIgnore for a bad one), within lim. It writes nothing and returns
// names and counts only, never a file's contents.
func (s *Scanner) Preview(ctx context.Context, root string, ignore []string, lim PreviewLimits) (*Preview, error) {
	lines, err := NormalizeIgnore(ignore)
	if err != nil {
		return nil, err
	}
	lim = lim.withDefaults()
	lib := catalog.Library{Name: "(setup preview)", Root: root, Kind: catalog.KindBooks}
	walk := &walkCap{dirs: lim.MaxDirs, until: time.Now().Add(lim.MaxTime)}
	books, _, err := discoverAuto(ctx, lib, nil, ParseIgnore(lines), s.log, nil, walk)
	capped := errors.Is(err, errWalkCapped)
	if err != nil && !capped {
		return nil, err
	}
	out := &Preview{Root: root, Counts: PreviewCounts{Capped: capped}, Samples: []PreviewSample{}}
	for _, b := range books {
		if b.IsFolder {
			out.Counts.FolderBooks++
			out.Counts.AudioFiles += len(b.Files)
		} else {
			out.Counts.FileBooks++
			out.Counts.AudioFiles++
		}
	}
	slices.SortFunc(books, func(a, b *catalog.Book) int { return cmp.Compare(a.RelPath, b.RelPath) })
	n := min(lim.Samples, len(books))
	for i := range n {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sample, own := s.previewSample(lib, books[i*len(books)/n])
		out.Samples = append(out.Samples, sample)
		out.Blanks.Tags += own.Tags
		out.Blanks.Path += own.Path
	}
	out.Suggested = catalog.MetadataFromTags
	if out.Blanks.Path < out.Blanks.Tags {
		out.Suggested = catalog.MetadataFromPath
	}
	return out, nil
}

// previewSample reads b's names as a scan does and names it both ways, with
// the names each source leaves empty on its own.
func (s *Scanner) previewSample(lib catalog.Library, b *catalog.Book) (PreviewSample, PreviewBlanks) {
	md, _ := metadata.Extract(absOf(lib, primaryPath(b)), s.ffprobePath)
	own := PreviewBlanks{Tags: blanks(tagNames(md)), Path: blanks(namesOf(metadata.FromPathLayout(b.RelPath, b.IsFolder)))}
	setNames(b, md)
	if b.IsFolder {
		b.ABS.File, _ = s.readABS(lib, b.RelPath, b.ABS.Sig)
	}
	return PreviewSample{
		RelPath: b.RelPath,
		Tags:    catalog.ResolveScanned(b, catalog.MetadataFromTags),
		Path:    catalog.ResolveScanned(b, catalog.MetadataFromPath),
		ABS:     b.ABS.File != nil,
	}, own
}

// tagNames are the names md's tags give on their own (nil: none), a generic
// title ("Track 01") counting as none, as the scan's title choice reads it.
func tagNames(md *metadata.Metadata) catalog.ScannedNames {
	if md == nil {
		return catalog.ScannedNames{}
	}
	n := namesOf(md)
	if metadata.IsGenericTitle(n.Title) {
		n.Title = ""
	}
	return n
}

// namesOf is a metadata reading's names, trimmed.
func namesOf(m *metadata.Metadata) catalog.ScannedNames {
	return catalog.ScannedNames{
		Title: strings.TrimSpace(m.Title), Author: strings.TrimSpace(m.Author), Narrator: strings.TrimSpace(m.Narrator),
		Series: strings.TrimSpace(m.Series), Position: catalog.FormatSeriesRange(m.SeriesIndex, m.SeriesIndexEnd),
	}
}

// blanks counts a book's empty names: its title, author, narrator and series,
// and its position when it has a series (a book in no series has none to give).
func blanks(n catalog.ScannedNames) int {
	c := 0
	for _, v := range []string{n.Title, n.Author, n.Narrator, n.Series} {
		if v == "" {
			c++
		}
	}
	if n.Series != "" && n.Position == "" {
		c++
	}
	return c
}
