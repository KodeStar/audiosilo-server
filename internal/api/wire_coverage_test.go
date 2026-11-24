package api

import (
	"encoding"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The wire goldens' coverage guard (audiosilo#96, server#347): a golden only
// guards the fields its scenario sets, so an omitempty field no seed gives a
// value is absent from every golden, and its removal, a retype or the next new
// field beside it would pass. Each wireCase names the Go types its envelope
// encodes (types) and the functions whose map literals build it (keys); this
// guard fails when
//
//   - a JSON field of one of those types (or of a type reached through one)
//     appears in no golden at all: seed it, in a new case when it is a variant
//     the others must not change shape for, or list it in wireUnseen with why
//     no golden can carry it;
//   - a string key one of those functions writes into a map appears in none of
//     its cases' goldens.
//
// So a new field cannot ship without a golden, and the player's parity test
// (which reads the goldens) sees it.

// wireUnseen lists the fields no golden can carry, as "<Go type>.<json name>"
// (or "<function>[<key>]" for a map key), each with the reason. A listed field
// a golden now carries fails, so the entry goes when a seed arrives.
var wireUnseen = map[string]string{
	"api.listenerLibrary.root": "shadows catalog.Library's root so a listener's row never carries it: always empty " +
		"(the libraries golden, an admin's, carries root)",
	"library.Entry.split_discs": "the console's: set on an admin's listing only (annotateWithBooks), never the player's",
	"library.Listing.folder":    "the console's: an admin's ?detail=1 only",
}

// checkWireCoverage runs the guard over the cases' committed goldens.
func checkWireCoverage(t *testing.T, cases []wireCase) {
	t.Helper()
	shapes := map[string]any{}
	for _, c := range cases {
		shapes[c.name] = readWireGolden(t, c.name).Shape
	}
	keysOf := func(fn string) []string { return mapKeysOf(t, fn) }
	for _, gap := range wireGaps(cases, shapes, wireUnseen, keysOf) {
		t.Error(gap)
	}
}

// wireGaps is the guard: what the cases' types and keys can write that no
// golden (shapes, by case name) carries and unseen does not excuse, and each
// entry of unseen that is stale. keysOf lists a function's map keys.
func wireGaps(cases []wireCase, shapes map[string]any, unseen map[string]string, keysOf func(string) []string) []string {
	cov := wireCoverage{reached: map[string]bool{}, seen: map[string]bool{}}
	keyed := map[string]map[string]bool{} // function -> the field names its cases' goldens hold
	for _, c := range cases {
		paths := map[string]string{}
		flattenShape(shapes[c.name], "$", paths)
		for _, at := range slices.Sorted(maps.Keys(c.types)) {
			// The type itself counts as a field: an optional block no golden
			// holds is one gap, not one per field of it.
			id := c.types[at].String()
			cov.reached[id] = true
			if _, ok := paths[at]; ok {
				cov.seen[id] = true
				cov.walk(c.types[at], at, paths, map[string]bool{})
			}
		}
		for _, fn := range c.keys {
			if keyed[fn] == nil {
				keyed[fn] = map[string]bool{}
			}
			for p := range paths {
				for _, seg := range pathFields(p) {
					keyed[fn][seg] = true
				}
			}
		}
	}
	// A key counts as carried when some golden of a case naming its function
	// holds a field of that name at any depth: the literal's place in the
	// envelope (a nested map, a map another function returns) is not tracked.
	for _, fn := range slices.Sorted(maps.Keys(keyed)) {
		for _, k := range keysOf(fn) {
			id := fn + "[" + k + "]"
			cov.reached[id] = true
			if keyed[fn][k] {
				cov.seen[id] = true
			}
		}
	}
	var gaps []string
	for _, id := range slices.Sorted(maps.Keys(cov.reached)) {
		_, listed := unseen[id]
		switch {
		case !cov.seen[id] && !listed:
			gaps = append(gaps, id+" is on the wire but in no golden, so nothing guards it (CROSS-REPO.md §15): "+
				"seed it in a wireCase (a new case for a variant the others must not change shape for), "+
				"then run go test ./internal/api -run TestWireGolden -update; "+
				"or list it in wireUnseen with why no golden can carry it")
		case cov.seen[id] && listed:
			gaps = append(gaps, "wireUnseen lists "+id+", which a golden now carries: remove the entry")
		}
	}
	for _, id := range slices.Sorted(maps.Keys(unseen)) {
		if !cov.reached[id] {
			gaps = append(gaps, "wireUnseen lists "+id+", which no case's types or keys reach: remove the entry")
		}
	}
	return gaps
}

// readWireGolden reads a case's committed golden.
func readWireGolden(t *testing.T, name string) wireGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(wireDir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var g wireGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return g
}

// pathFields are the field names along a flattened shape path ("$.a[].b" is a, b).
func pathFields(p string) []string {
	var out []string
	for _, seg := range strings.Split(strings.TrimPrefix(p, "$"), ".") {
		if seg = strings.TrimRight(seg, "[]"); seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// wireCoverage records, per "<type>.<field>" (and per "<function>[<key>]"),
// whether a case's types reach it and whether a golden carries it.
type wireCoverage struct {
	reached, seen map[string]bool
}

var (
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

// walk follows typ, encoded at path, through the golden's paths: each field of
// a struct is reached, seen when the golden holds it, and followed into its
// own type there. A type that encodes itself (time.Time, json.RawMessage) and
// an interface are leaves.
func (c *wireCoverage) walk(typ reflect.Type, path string, paths map[string]string, onPath map[string]bool) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Implements(jsonMarshaler) || reflect.PointerTo(typ).Implements(jsonMarshaler) ||
		typ.Implements(textMarshaler) || reflect.PointerTo(typ).Implements(textMarshaler) {
		return
	}
	visit := typ.String() + " " + path
	if onPath[visit] {
		return
	}
	onPath[visit] = true
	defer delete(onPath, visit)
	switch typ.Kind() {
	case reflect.Struct:
		for _, f := range jsonFields(typ) {
			id := f.owner.String() + "." + f.name
			c.reached[id] = true
			p := path + "." + f.name
			if _, ok := paths[p]; ok {
				c.seen[id] = true
				c.walk(f.typ, p, paths, onPath)
			}
		}
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			return // []byte is a base64 string
		}
		if _, ok := paths[path+"[]"]; ok {
			c.walk(typ.Elem(), path+"[]", paths, onPath)
		}
	case reflect.Map:
		// The keys are data: follow the value type into each key the golden holds.
		for p := range paths {
			rest, ok := strings.CutPrefix(p, path+".")
			if ok && !strings.ContainsAny(rest, ".[") {
				c.walk(typ.Elem(), p, paths, onPath)
			}
		}
	}
}

// wireField is one JSON field of a struct: its name, its Go type, and the
// struct declaring it (an embedded struct's fields belong to it, not to the
// type embedding it).
type wireField struct {
	name  string
	typ   reflect.Type
	owner reflect.Type
}

// jsonFields are the fields encoding/json writes for struct typ, an untagged
// embedded struct's promoted into it.
func jsonFields(typ reflect.Type) []wireField {
	var out []wireField
	for i := range typ.NumField() {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if f.Anonymous && name == "" && ft.Kind() == reflect.Struct {
			out = append(out, jsonFields(ft)...)
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, wireField{name: name, typ: f.Type, owner: typ})
	}
	return out
}

// mapKeysOf lists the string-literal keys function or method fn (in this
// package's non-test files) writes into a map: a map literal's keys
// and m["key"] = assignments.
func mapKeysOf(t *testing.T, fn string) []string {
	t.Helper()
	decl := wireFuncDecl(t, fn)
	keys := map[string]bool{}
	lit := func(e ast.Expr) {
		if b, ok := e.(*ast.BasicLit); ok && b.Kind == token.STRING {
			if s, err := strconv.Unquote(b.Value); err == nil {
				keys[s] = true
			}
		}
	}
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if mt, ok := x.Type.(*ast.MapType); ok {
				if id, ok := mt.Key.(*ast.Ident); ok && id.Name == "string" {
					for _, el := range x.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							lit(kv.Key)
						}
					}
				}
			}
		case *ast.AssignStmt:
			for _, l := range x.Lhs {
				if ix, ok := l.(*ast.IndexExpr); ok {
					lit(ix.Index)
				}
			}
		}
		return true
	})
	return slices.Sorted(maps.Keys(keys))
}

// wireFuncDecl finds function or method fn among the package's non-test files.
func wireFuncDecl(t *testing.T, fn string) *ast.FuncDecl {
	t.Helper()
	var found []*ast.FuncDecl
	files, err := wireSource()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn && fd.Body != nil {
				found = append(found, fd)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d functions named %s in package api, want one", len(found), fn)
	}
	return found[0]
}

// wireSource is the package's non-test files, parsed.
var wireSource = sync.OnceValues(func() ([]*ast.File, error) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
})

// The guard's own test: synthetic types and shapes, so each rule is shown
// passing (allowed) and failing (denied) without the real envelopes.
type wireTestBase struct {
	B string `json:"b"`
}

type wireTestInner struct {
	X string `json:"x"`
	Y int    `json:"y,omitempty"`
}

type wireTestOuter struct {
	wireTestBase
	A     string                   `json:"a"`
	In    *wireTestInner           `json:"in,omitempty"`
	List  []wireTestInner          `json:"list"`
	When  time.Time                `json:"when"` // encodes itself: a leaf
	Skip  string                   `json:"-"`
	ByKey map[string]wireTestInner `json:"by_key"`
	hide  string
}

func TestWireGaps(t *testing.T) {
	t.Parallel()
	_ = wireTestOuter{}.hide
	shape := func(raw string) any {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	inner := `{"x":"string","y":"number"}`
	full := `{"a":"string","b":"string","in":` + inner + `,"list":[` + inner + `],"when":"string","by_key":{"k1":` + inner + `}}`
	outer := map[string]reflect.Type{"$": reflect.TypeFor[wireTestOuter]()}
	noKeys := func(string) []string { return nil }
	gaps := func(shapes map[string]string, unseen map[string]string, cases ...wireCase) []string {
		s := map[string]any{}
		for n, raw := range shapes {
			s[n] = shape(raw)
		}
		return wireGaps(cases, s, unseen, noKeys)
	}

	// Allowed: every field in a golden; or spread over two variants of one type.
	if g := gaps(map[string]string{"full": full}, nil, wireCase{name: "full", types: outer}); len(g) != 0 {
		t.Fatalf("full: %q", g)
	}
	bare := `{"a":"string","b":"string","list":[{"x":"string"}],"when":"string","by_key":{}}`
	if g := gaps(map[string]string{"bare": bare, "full": full}, nil,
		wireCase{name: "bare", types: outer}, wireCase{name: "full", types: outer}); len(g) != 0 {
		t.Fatalf("two variants: %q", g)
	}

	// Denied: an optional field and block no golden holds, an embedded
	// struct's field (its own type's), a type absent at its path.
	got := gaps(map[string]string{"bare": `{"a":"string","list":[{"x":"string"}],"when":"string","by_key":{}}`, "none": `{}`}, nil,
		wireCase{name: "bare", types: outer},
		wireCase{name: "none", types: map[string]reflect.Type{"$.x": reflect.TypeFor[wireTestInner]()}})
	want := []string{"api.wireTestBase.b", "api.wireTestInner", "api.wireTestInner.y", "api.wireTestOuter.in"}
	if ids := gapIDs(got); !slices.Equal(ids, want) {
		t.Fatalf("gaps = %q, want %q", ids, want)
	}

	// wireUnseen excuses a gap; an entry a golden carries, or nothing reaches, is stale.
	got = gaps(map[string]string{"full": full}, map[string]string{"api.wireTestOuter.a": "", "api.wireTestOuter.gone": ""},
		wireCase{name: "full", types: outer})
	if len(got) != 2 || !strings.Contains(got[0], "api.wireTestOuter.a, which a golden now carries") ||
		!strings.Contains(got[1], "api.wireTestOuter.gone, which no case") {
		t.Fatalf("stale entries: %q", got)
	}
	got = gaps(map[string]string{"bare": bare}, map[string]string{"api.wireTestOuter.in": "", "api.wireTestInner.y": ""},
		wireCase{name: "bare", types: outer})
	if len(got) != 0 {
		t.Fatalf("excused gaps: %q", got)
	}

	// A map key: carried when a golden of the function's case holds it.
	keysOf := func(string) []string { return []string{"code", "error"} }
	got = wireGaps([]wireCase{{name: "err", keys: []string{"f"}}}, map[string]any{"err": shape(`{"error":"string"}`)}, nil, keysOf)
	if ids := gapIDs(got); !slices.Equal(ids, []string{"f[code]"}) {
		t.Fatalf("key gaps = %q", ids)
	}

	// The keys come from the function's source: its map literals and assignments.
	if k := mapKeysOf(t, "writeErrorCode"); !slices.Equal(k, []string{"code", "error"}) {
		t.Fatalf("writeErrorCode keys = %q", k)
	}
	if k := mapKeysOf(t, "authMethods"); !slices.Equal(k, []string{"methods", "name", "oidc"}) {
		t.Fatalf("authMethods keys = %q", k)
	}
}

// gapIDs is each gap's field id (its message's first word).
func gapIDs(gaps []string) []string {
	out := make([]string, len(gaps))
	for i, g := range gaps {
		out[i], _, _ = strings.Cut(g, " ")
	}
	return out
}
