package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// notAudited are the admin routes that change something yet write no audit event,
// each with why. Every other non-GET /api/v1/admin route must reach a.audit.
var notAudited = map[string]string{
	"handleScanLibrary":            "scans are recorded in scan_runs (Health > Jobs), with who asked",
	"handleScanAll":                "scans are recorded in scan_runs",
	"handleRescanBook":             "re-reads files; changes nothing an admin chose",
	"handleCancelJob":              "a cancelled scan is recorded in scan_runs",
	"handleUpdateCheck":            "asks GitHub; changes nothing",
	"handleAdminCovers":            "a read (a POST only for its batch body)",
	"handleAdminBookWorks":         "a read (a POST only for its batch body)",
	"handleCommunityCovers":        "a read (a POST only for its batch body)",
	"handleTestNotifyTarget":       "sends a test message; its outcome is recorded on the destination",
	"handleImportUsers":            "connects to Audiobookshelf and lists its users; changes nothing",
	"handleUpdateImport":           "changes only an unapplied import's review (its cutoff)",
	"handleDeleteImport":           "drops an unapplied import's record; it wrote nothing (or an undo took it out, audited)",
	"handleCheckCommunityChapters": "re-checks the community chapter list; changes nothing an admin chose (picking a chapter source is a book.edit)",
}

// TestAdminChangesAreAudited reads the route table and the package's call graph: a
// handler counts as audited when it calls a.audit, directly or through another
// method of the API (applyIgnore).
func TestAdminChangesAreAudited(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	route := regexp.MustCompile(`mux\.Handle\("(POST|PUT|PATCH|DELETE) (/api/v1/admin[^"]*)",[^\n]*?a\.(handle\w+)`)
	matches := route.FindAllStringSubmatch(string(src), -1)
	if len(matches) < 30 {
		t.Fatalf("found only %d admin write routes: the pattern no longer matches api.go", len(matches))
	}

	calls := map[string]map[string]bool{} // method -> a.X methods it calls
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			callees := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "a" {
						callees[sel.Sel.Name] = true
					}
				}
				return true
			})
			calls[fn.Name.Name] = callees
		}
	}
	var audits func(string, map[string]bool) bool
	audits = func(fn string, seen map[string]bool) bool {
		if seen[fn] {
			return false
		}
		seen[fn] = true
		for callee := range calls[fn] {
			if callee == "audit" || audits(callee, seen) {
				return true
			}
		}
		return false
	}

	for _, m := range matches {
		method, path, handler := m[1], m[2], m[3]
		if _, exempt := notAudited[handler]; exempt {
			continue
		}
		if _, known := calls[handler]; !known {
			t.Errorf("%s %s: handler %s not found", method, path, handler)
			continue
		}
		if !audits(handler, map[string]bool{}) {
			t.Errorf("%s %s (%s) changes something but writes no audit event; call a.audit or list it in notAudited with why", method, path, handler)
		}
	}
}
