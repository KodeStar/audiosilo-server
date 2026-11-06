package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// The admin console's "Set up your server" flow (FIRST-RUN-PLAN.md Phase F1):
// which of its steps an admin finished or skipped, whether the flow is over and
// whether its Overview card was dismissed. Server-wide, in server_state (the
// pattern of support.go); migration 0053 marks a server upgraded from a release
// without the flow as set up. Everything a step sets goes through the ordinary
// admin APIs, so this records progress only.

// setupKey is the flow's row in server_state.
const setupKey = "setup_state"

// SetupSteps are the flow's step ids, in the order the console shows them.
var SetupSteps = []string{"folder", "naming", "metadata", "reach", "abs", "backups", "phone", "bridge"}

// A step's recorded outcome. A step with neither hasn't been seen yet.
const (
	SetupStepDone    = "done"
	SetupStepSkipped = "skipped"
)

// ErrInvalidSetupStep is returned by UpdateSetupState for a step id that isn't
// one of SetupSteps, or an outcome that isn't done or skipped.
var ErrInvalidSetupStep = errors.New("invalid setup step")

// SetupState is the flow's progress. A server with no row is a fresh one: not
// finished, nothing dismissed, no step seen.
type SetupState struct {
	Finished  bool              `json:"finished"`
	Dismissed bool              `json:"dismissed"`
	Steps     map[string]string `json:"steps"`
}

// SetupPatch is the fields an update sets; the rest keep their value. Steps
// merge: an id it names takes the new outcome, the others stay. It is also PUT
// /admin/setup's body.
type SetupPatch struct {
	Finished  *bool             `json:"finished"`
	Dismissed *bool             `json:"dismissed"`
	Steps     map[string]string `json:"steps"`
}

// SetupState reads the flow's progress (Steps never nil).
func (c *Catalog) SetupState(ctx context.Context) (SetupState, error) {
	return readSetupState(ctx, c.db)
}

func readSetupState(ctx context.Context, q querier) (SetupState, error) {
	st, err := getServerState[SetupState](ctx, q, setupKey)
	if st.Steps == nil {
		st.Steps = map[string]string{}
	}
	return st, err
}

// UpdateSetupState merges p into the flow's progress in one transaction and
// returns what it stored. ErrInvalidSetupStep (nothing stored) for a step id or
// outcome it doesn't know.
func (c *Catalog) UpdateSetupState(ctx context.Context, p SetupPatch) (SetupState, error) {
	for id, outcome := range p.Steps {
		if !slices.Contains(SetupSteps, id) {
			return SetupState{}, fmt.Errorf("%w: unknown step %q", ErrInvalidSetupStep, id)
		}
		if outcome != SetupStepDone && outcome != SetupStepSkipped {
			return SetupState{}, fmt.Errorf("%w: %q must be %q or %q", ErrInvalidSetupStep, id, SetupStepDone, SetupStepSkipped)
		}
	}
	var st SetupState
	err := c.db.WithTx(ctx, "UpdateSetupState", func(tx *sql.Tx) error {
		var err error
		if st, err = readSetupState(ctx, tx); err != nil {
			return err
		}
		if p.Finished != nil {
			st.Finished = *p.Finished
		}
		if p.Dismissed != nil {
			st.Dismissed = *p.Dismissed
		}
		maps.Copy(st.Steps, p.Steps)
		return putServerState(ctx, tx, setupKey, st, c.stamp())
	})
	if err != nil {
		return SetupState{}, err
	}
	return st, nil
}

// CheckLibraryRoot is library create's root check for a books library at root,
// creating nothing (the setup flow's folder preview): ErrRootOverlaps where
// CreateLibrary would refuse it (checkLibraryRoot, read outside the writer, so a
// create racing it is still checked again when it runs), else the name of a
// library already at exactly this root ("" for none), which create allows.
func (c *Catalog) CheckLibraryRoot(ctx context.Context, root string) (sameRoot string, err error) {
	return checkLibraryRoot(ctx, c.db, Library{Root: root, Kind: KindBooks})
}

// ScannedNames are a book's names as a library resolves them from what the scan
// read, before enrichment and overrides: the setup flow's naming preview.
type ScannedNames struct {
	Title    string `json:"title"`
	Author   string `json:"author"`
	Narrator string `json:"narrator"`
	Series   string `json:"series"`
	Position string `json:"position"`
}

// ResolveScanned is what a library whose metadata source is source
// (MetadataFromTags or MetadataFromPath) shows for b, a book as the scan read it
// and has not stored (its rel path, folder flag, names and Audiobookshelf file):
// the same layers a stored book's scan values go through (bookLayers.scannedFields,
// with today's path baseline), so the preview and the library never disagree.
func ResolveScanned(b *Book, source string) ScannedNames {
	scanned := fieldsOf(b)
	maps.DeleteFunc(scanned, func(_, v string) bool { return v == "" })
	l := &bookLayers{
		path: b.RelPath, isFolder: b.IsFolder, preferPath: source == MetadataFromPath,
		scanned: scanned, abs: bookFields{}, pathRev: scannedRev,
	}
	if b.IsFolder {
		l.abs = absFields(b.ABS.File)
	}
	f, _, _ := l.scannedFields()
	return ScannedNames{
		Title: f[FieldTitle], Author: f[FieldAuthor], Narrator: f[FieldNarrator],
		Series: f[FieldSeries], Position: f[FieldSeriesIndex],
	}
}
