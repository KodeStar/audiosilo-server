package api

import (
	"cmp"
	"encoding"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
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
//     its cases' goldens where that function's map lands (its place, "$"
//     unless the case names another), outside the fields the case's types
//     account for and the places of the case's other functions;
//   - a case names no types and no keys, so the guard would see nothing of it.
//
// So a new field cannot ship without a golden, and the player's parity test
// (which reads the goldens) sees it.

// wireUnseen lists the fields no golden carries, as "<Go type>.<json name>"
// (or "<function>[<key>]" for a map key), each with the reason: one no golden
// can carry, or one only an admin's console view sets, which the player never
// acts on. A listed field a golden now carries fails, so the entry goes when a
// seed arrives.
var wireUnseen = map[string]string{
	"api.listenerLibrary.root": "shadows catalog.Library's root so a listener's row never carries it: always empty " +
		"(the libraries golden, an admin's, carries root)",
	"library.Entry.split_discs": "the console's: set for an admin caller only (annotateWithBooks); the player mirrors it but never acts on it",
	"library.Listing.folder":    "the console's: an admin's ?detail=1 only",
}

// checkWireCoverage runs the guard over the cases' goldens' shapes (by case
// name), which the shape checks have just held to the committed files.
func checkWireCoverage(t *testing.T, cases []wireCase, shapes map[string]any) {
	t.Helper()
	keysOf := func(fn string) []string { return mapKeysOf(t, fn) }
	for _, gap := range wireGaps(cases, shapes, wireUnseen, keysOf) {
		t.Error(gap)
	}
}

// wireGaps is the guard: what the cases' types and keys can write that no
// golden (shapes, by case name) carries and unseen does not excuse, and each
// entry of unseen that is stale. keysOf lists a function's map keys.
func wireGaps(cases []wireCase, shapes map[string]any, unseen map[string]string, keysOf func(string) []string) []string {
	var gaps []string
	cov := wireCoverage{}
	keyed := map[string]map[string]bool{} // function -> the field names its cases' goldens hold at its place
	for _, c := range cases {
		if len(c.types) == 0 && len(c.keys) == 0 {
			gaps = append(gaps, c.name+" names no types and no keys, so the guard sees nothing of its envelope: name them")
		}
		paths := map[string]string{}
		flattenShape(shapes[c.name], "$", paths)
		typed := map[string]bool{} // the paths a type's field (or a map's data key) accounts for
		for at, typ := range c.types {
			// The type itself counts as a field: an optional block no golden
			// holds is one gap, not one per field of it.
			if cov.mark(typ.String(), paths, at) {
				cov.walk(typ, at, paths, typed)
			}
		}
		places := map[string]string{} // function -> where its map lands
		for _, k := range c.keys {
			fn, at, _ := strings.Cut(k, "@")
			places[fn] = cmp.Or(at, "$")
		}
		for fn, at := range places {
			if keyed[fn] == nil {
				keyed[fn] = map[string]bool{}
			}
			for p := range paths {
				if wireKeyPath(p, at, places, typed) {
					// The field the path ends in ("$.a.b[]" is b).
					keyed[fn][strings.TrimRight(p[strings.LastIndex(p, ".")+1:], "[]")] = true
				}
			}
		}
	}
	// A key counts as carried when some golden of a case naming its function
	// holds a field of that name below the function's place, at any depth there
	// (a nested literal's keys sit deeper), but not in a field one of the case's
	// types accounts for, nor below another function's place inside it.
	for fn, names := range keyed {
		for _, k := range keysOf(fn) {
			id := fn + "[" + k + "]"
			cov[id] = cov[id] || names[k]
		}
	}
	for _, id := range slices.Sorted(maps.Keys(cov)) {
		_, listed := unseen[id]
		switch {
		case !cov[id] && !listed:
			gaps = append(gaps, id+" is on the wire but in no golden, so nothing guards it (CROSS-REPO.md §15): "+
				"seed it in a wireCase (a new case for a variant the others must not change shape for), "+
				"then run go test ./internal/api -run TestWireGolden -update; "+
				"or list it in wireUnseen with why no golden can carry it")
		case cov[id] && listed:
			gaps = append(gaps, "wireUnseen lists "+id+", which a golden now carries: remove the entry")
		}
	}
	for _, id := range slices.Sorted(maps.Keys(unseen)) {
		if _, reached := cov[id]; !reached {
			gaps = append(gaps, "wireUnseen lists "+id+", which no case's types or keys reach: remove the entry")
		}
	}
	return gaps
}

// wireKeyPath reports whether golden path p can hold a key of the function
// whose map lands at at: p is below at, in no field typed accounts for (p or
// one of its parents), and below no other function's place inside at.
func wireKeyPath(p, at string, places map[string]string, typed map[string]bool) bool {
	if !strings.HasPrefix(p, at+".") {
		return false
	}
	for q := p; q != at; q = q[:strings.LastIndexAny(q, ".[")] {
		if typed[q] {
			return false
		}
	}
	for _, other := range places {
		if other != at && strings.HasPrefix(other, at+".") && strings.HasPrefix(p, other+".") {
			return false
		}
	}
	return true
}

// wireCoverage holds, per "<type>.<field>" (and per "<function>[<key>]") a
// case's types reach, whether a golden carries it.
type wireCoverage map[string]bool

// mark records id reached, and seen when the golden holds path; it reports
// whether it does.
func (c wireCoverage) mark(id string, paths map[string]string, path string) bool {
	_, ok := paths[path]
	c[id] = c[id] || ok
	return ok
}

var (
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

// walk follows typ, encoded at path, through the golden's paths: each field of
// a struct is reached, seen when the golden holds it, and followed into its
// own type there; typed gets each field's path (and each map data key's). A
// type that encodes itself (time.Time, json.RawMessage) and an interface are
// leaves. Each step follows a longer path the golden holds, so a recursive
// type ends with the golden.
func (c wireCoverage) walk(typ reflect.Type, path string, paths map[string]string, typed map[string]bool) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	// A pointer's method set holds the value's.
	if pt := reflect.PointerTo(typ); pt.Implements(jsonMarshaler) || pt.Implements(textMarshaler) {
		return
	}
	switch typ.Kind() {
	case reflect.Struct:
		for _, f := range jsonFields(typ) {
			p := path + "." + f.name
			typed[p] = true
			if c.mark(f.owner.String()+"."+f.name, paths, p) {
				c.walk(f.typ, p, paths, typed)
			}
		}
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			return // []byte is a base64 string
		}
		if _, ok := paths[path+"[]"]; ok {
			c.walk(typ.Elem(), path+"[]", paths, typed)
		}
	case reflect.Map:
		// The keys are data: follow the value type into each key the golden holds.
		for p := range paths {
			rest, ok := strings.CutPrefix(p, path+".")
			if ok && !strings.ContainsAny(rest, ".[") {
				typed[p] = true
				c.walk(typ.Elem(), p, paths, typed)
			}
		}
	}
}

// wireField is one JSON field of a struct: its name, its Go type, and the
// struct declaring it (an embedded struct's fields belong to it, not to the
// type embedding it); depth (how many embedded structs down) and tagged (a
// json tag names it) decide between fields of one name.
type wireField struct {
	name   string
	typ    reflect.Type
	owner  reflect.Type
	depth  int
	tagged bool
}

// jsonFields are the fields encoding/json writes for struct typ, an untagged
// embedded struct's promoted into it. Of fields sharing a name it writes the
// shallowest, at one depth the only tagged one, and else none of them.
func jsonFields(typ reflect.Type) []wireField {
	all := structFields(typ, 0)
	var out []wireField
	for i, f := range all {
		written := true
		for j, g := range all {
			if j != i && g.name == f.name && (g.depth < f.depth || g.depth == f.depth && (g.tagged || !f.tagged)) {
				written = false
				break
			}
		}
		if written {
			out = append(out, f)
		}
	}
	return out
}

// structFields are struct typ's JSON field candidates at depth, an untagged
// embedded struct's among them one deeper.
func structFields(typ reflect.Type, depth int) []wireField {
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
			out = append(out, structFields(ft, depth+1)...)
			continue
		}
		if !f.IsExported() {
			continue
		}
		out = append(out, wireField{name: cmp.Or(name, f.Name), typ: f.Type, owner: typ, depth: depth, tagged: name != ""})
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

// wireTestShadow's own b shadows wireTestBase's, and its two embedded C's
// cancel out.
type wireTestShadow struct {
	wireTestBase
	wireTestC1
	wireTestC2
	B int `json:"b"`
}

type wireTestC1 struct {
	C string
}

type wireTestC2 struct {
	C string
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
	outer := wireAt[wireTestOuter]("$")
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
		wireCase{name: "none", types: wireAt[wireTestInner]("$.x")})
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

	// Denied: a case naming no types and no keys.
	if got := gaps(map[string]string{"bare": bare}, nil, wireCase{name: "bare"}); len(got) != 1 ||
		!strings.HasPrefix(got[0], "bare names no types and no keys") {
		t.Fatalf("a case naming nothing: %q", got)
	}

	// Of fields sharing a name, the one encoding/json writes: the shallower b,
	// and neither of two C at one depth.
	if b, _ := json.Marshal(wireTestShadow{}); string(b) != `{"b":0}` {
		t.Fatalf("encoding/json writes %s", b)
	}
	var fields []string
	for _, f := range jsonFields(reflect.TypeFor[wireTestShadow]()) {
		fields = append(fields, f.owner.String()+"."+f.name)
	}
	if !slices.Equal(fields, []string{"api.wireTestShadow.b"}) {
		t.Fatalf("shadowed fields = %q", fields)
	}

	// A map key: carried when a golden of the function's case holds it below
	// the function's place ("g@$.inner"; "$" when unnamed), but not in a field
	// the case's types account for (f[x]) nor below another function's place
	// (f[code]).
	keys := map[string][]string{"f": {"code", "error", "x"}, "g": {"code"}}
	keysOf := func(fn string) []string { return keys[fn] }
	got = wireGaps([]wireCase{{name: "err", keys: []string{"f", "g@$.inner"}, types: wireAt[wireTestInner]("$.typed")}},
		map[string]any{"err": shape(`{"error":"string","typed":` + inner + `,"inner":{"code":"string"}}`)}, nil, keysOf)
	if ids := gapIDs(got); !slices.Equal(ids, []string{"f[code]", "f[x]"}) {
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
