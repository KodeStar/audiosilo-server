package catalog

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	unorm "golang.org/x/text/unicode/norm"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// A podcast library's root may neither sit inside another library's nor hold
// one (ErrRootOverlaps, checkLibraryRoot): the scans of the two would list one
// folder's files twice, as episodes and as books. Roots are compared as
// written and as their links resolve (a podcast root reached through a link
// into another library's folder is that folder), by the platform's rule for
// names: Windows and macOS fold case (macOS a name's Unicode spellings too),
// and a volume's root (`/`, `C:\`) holds every folder on it (server#179).

// rootResolveWait bounds resolving every library's root before a create or
// edit takes the writer (resolveRoots): a share that stopped answering leaves
// its root compared as written.
const rootResolveWait = 2 * time.Second

// resolveRoots resolves, outside the writer, the links of the roots the
// overlap check compares lib's against (absolute form -> resolved, only those
// that resolve to another path): lib's and every other library's when lib is
// a podcast library, else lib's and the podcast libraries' (checkLibraryRoot
// compares only pairs with one); nil when there is no such pair. A root that
// doesn't resolve in time, or at all (not there yet, a share gone), is left
// out.
func (c *Catalog) resolveRoots(ctx context.Context, lib Library) (map[string]string, error) {
	libs, err := c.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	roots := []string{lib.Root}
	for _, l := range libs {
		if l.ID != lib.ID && (lib.Kind == KindPodcasts || l.Kind == KindPodcasts) {
			roots = append(roots, l.Root)
		}
	}
	if len(roots) == 1 {
		return nil, nil
	}
	return resolveRootLinks(ctx, roots), nil
}

// resolveRootLinks resolves each of roots' links at once, under one
// rootResolveWait: absolute form -> resolved, for those that resolve to
// another path. A root not there yet (a new library's folder the scan makes)
// resolves through the nearest folder above it that is.
func resolveRootLinks(ctx context.Context, roots []string) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, rootResolveWait)
	defer cancel()
	deadline, _ := ctx.Deadline()
	out := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		wg.Go(func() {
			real, ok := resolveLinks(ctx, abs, deadline)
			if ok && real != abs {
				mu.Lock()
				out[abs] = real
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return out
}

// resolveLinks is abs with its links resolved (filepath.EvalSymlinks, bounded
// by deadline), the part below the nearest folder that is there kept as
// written; false when no folder above it resolves.
func resolveLinks(ctx context.Context, abs string, deadline time.Time) (string, bool) {
	rest := ""
	for p := abs; ; {
		real, err := metadata.EvalSymlinksBounded(ctx, p, metadata.WaitLeft(deadline))
		if err == nil {
			return filepath.Join(real, rest), true
		}
		up := filepath.Dir(p)
		if !metadata.NotExist(err) || up == p {
			return "", false
		}
		rest, p = filepath.Join(filepath.Base(p), rest), up
	}
}

// formsOverlap reports whether roots a and b (absolute) overlap as written or
// as resolved (each root's resolved form, when it has one).
func formsOverlap(a, b string, resolved map[string]string) bool {
	for _, x := range []string{a, resolved[a]} {
		for _, y := range []string{b, resolved[b]} {
			if x != "" && y != "" && pathOverlaps(x, y) {
				return true
			}
		}
	}
	return false
}

// pathOverlaps reports whether absolute, cleaned paths a and b are one folder
// or one holds the other, by this platform's rule (rootsOverlap). macOS's file
// systems (APFS, HFS+) also take a name's two Unicode spellings (NFC, NFD) as
// one, so there both are compared in NFC.
func pathOverlaps(a, b string) bool {
	if runtime.GOOS == "darwin" {
		a, b = unorm.NFC.String(a), unorm.NFC.String(b)
	}
	return rootsOverlap(a, b, filepath.Separator, runtime.GOOS == "windows" || runtime.GOOS == "darwin")
}

// rootsOverlap is pathOverlaps for a platform whose separator is sep and whose
// names compare case-insensitively when fold: a path is inside another when it
// continues it past a separator, or the other is a volume's root (a cleaned
// path ends in a separator only then: `/`, `C:\`, `\\host\share\`).
func rootsOverlap(a, b string, sep byte, fold bool) bool {
	if fold {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	within := func(p, root string) bool {
		if !strings.HasPrefix(p, root) {
			return false
		}
		return len(p) == len(root) || root[len(root)-1] == sep || p[len(root)] == sep
	}
	return within(a, b) || within(b, a)
}

// RootOverlap is two libraries whose roots overlap, one of them a podcast
// library: refused on a create or edit, but a pair stored before that check
// knew links or folded case (or made by a link added since) stays until an
// admin moves one.
type RootOverlap struct {
	Library, Other Library
}

// RootOverlaps lists the stored libraries whose roots overlap one another, a
// podcast library among each pair, by checkLibraryRoot's rule: what the
// launcher warns about at start.
func (c *Catalog) RootOverlaps(ctx context.Context) ([]RootOverlap, error) {
	libs, err := c.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	roots := make([]string, len(libs))
	for i, l := range libs {
		if roots[i], err = filepath.Abs(l.Root); err != nil {
			return nil, fmt.Errorf("library %q root: %w", l.Name, err)
		}
	}
	if !slices.ContainsFunc(libs, func(l Library) bool { return l.Kind == KindPodcasts }) {
		return nil, nil
	}
	resolved := resolveRootLinks(ctx, roots)
	var out []RootOverlap
	for i := range libs {
		for j := i + 1; j < len(libs); j++ {
			if libs[i].Kind != KindPodcasts && libs[j].Kind != KindPodcasts {
				continue
			}
			if formsOverlap(roots[i], roots[j], resolved) {
				out = append(out, RootOverlap{Library: libs[i], Other: libs[j]})
			}
		}
	}
	return out, nil
}
