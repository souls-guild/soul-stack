// The integration lane runs TestMain with containers, and the child process
// these guards start would bring up a second set.

//go:build !integration

package api

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/wiretime/wiretimetest"
)

// TestWireTimes_EveryServedSchemaAnswersInUTC is the NIM-1160 guard for REST.
//
// No schema is listed here. Every component schema the served Operator API
// registers (fullSpecGroups, kept complete by TestFullSpec_CoversAllRoutes) is
// filled through reflection with an instant in the process zone — the way pgx
// scans timestamptz and time.Now answers — and encoded by the huma API's own
// JSON format. A new time field in any of them is filled and checked without an
// edit here.
func TestWireTimes_EveryServedSchemaAnswersInUTC(t *testing.T) {
	if !wiretimetest.InSaoPaulo(t) {
		return
	}
	at := time.Unix(1793383200, 0) // 2026-10-30T14:00:00-03:00
	seen := map[reflect.Type]bool{}
	schemas, instants := 0, 0
	for _, g := range fullSpecGroups() {
		api := newHumaCadenceAPI(chi.NewRouter())
		if err := g.register(api); err != nil {
			t.Fatalf("%s: register: %v", g.prefix, err)
		}
		reg := api.OpenAPI().Components.Schemas
		names := make([]string, 0, len(reg.Map()))
		for name := range reg.Map() {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			typ := reg.TypeFromRef("#/components/schemas/" + name)
			if typ == nil || seen[typ] {
				continue
			}
			seen[typ] = true
			f := &timeFiller{at: at}
			v := reflect.New(typ).Elem()
			f.fill(v, 0)
			if f.slots == 0 {
				continue
			}
			var buf bytes.Buffer
			if err := api.Marshal(&buf, "application/json", v.Interface()); err != nil {
				t.Errorf("%s: encode: %v", name, err)
				continue
			}
			got := wiretimetest.Instants(t, buf.Bytes())
			if len(got) != f.slots {
				t.Errorf("%s: filled %d instants, the answer carries %d:\n%s", name, f.slots, len(got), buf.Bytes())
			}
			for path, s := range got {
				p, _ := time.Parse(time.RFC3339Nano, s)
				if !strings.HasSuffix(s, "Z") || !p.Equal(at) {
					t.Errorf("%s %s = %s, want %s", name, path, s, at.UTC().Format(time.RFC3339))
				}
			}
			schemas++
			instants += len(got)
		}
	}
	// A floor, not an inventory: it fails if the registry walk stops reaching
	// the types, which would leave every assertion above vacuously green.
	if schemas < 40 {
		t.Fatalf("only %d schemas carried an instant (%d instants); the registry walk reaches too little to mean anything", schemas, instants)
	}
	t.Logf("%d schemas, %d instants, all UTC", schemas, instants)
}

// timeFiller sets every field encoding/json writes to a non-zero value, and
// every time.Time to at, counting those.
type timeFiller struct {
	at    time.Time
	slots int
}

var timeType = reflect.TypeFor[time.Time]()

func (f *timeFiller) fill(v reflect.Value, depth int) {
	t := v.Type()
	if t == timeType {
		if v.CanSet() {
			v.Set(reflect.ValueOf(f.at))
			f.slots++
		}
		return
	}
	if depth > 8 || (t.Kind() != reflect.Struct && !v.CanSet()) {
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		p := reflect.New(t.Elem())
		f.fill(p.Elem(), depth+1)
		v.Set(p)
	case reflect.Struct:
		for i := range t.NumField() {
			sf := t.Field(i)
			if (sf.IsExported() || sf.Anonymous) && sf.Tag.Get("json") != "-" {
				f.fill(v.Field(i), depth+1)
			}
		}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return // []byte and json.RawMessage
		}
		s := reflect.MakeSlice(t, 1, 1)
		f.fill(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Array:
		for i := range v.Len() {
			f.fill(v.Index(i), depth+1)
		}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return
		}
		m := reflect.MakeMap(t)
		e := reflect.New(t.Elem()).Elem()
		f.fill(e, depth+1)
		m.SetMapIndex(reflect.ValueOf("k").Convert(t.Key()), e)
		v.Set(m)
	case reflect.String:
		v.SetString("x")
	}
}

// encodeExempt names, by file, function and the encoded expression, the JSON
// encodes in the answer packages that skip wiretime.UTC, each with the reason
// its value can hold no instant. Stored values are not exempt: a target or an
// input is answered back verbatim. A method named MarshalJSON needs no entry: it
// runs inside an encode that already moved its receiver to UTC.
var encodeExempt = map[string]string{
	"api/handlers/run_gate.go:InsertDeferredRun:[]string{incID}": "incarnation IDs",
	"api/handlers/voyage.go:buildVoyageRow:resolved":             "incarnation IDs",
	"api/huma_served_spec.go:openAPISpecJSONBytes:spec":          "the OpenAPI document, which describes instants and holds none",
	"mcp/voyage.go:invokeVoyageHandler:b":                        "the request to the REST handler the tool calls, whose own encodes are held here",
	"mcp/server.go:jsonString:s":                                 "a string",
}

// TestWireTimes_EveryEncoderGoesThroughUTC holds the other half of the REST and
// MCP guards: every JSON encode in keeper/internal/api and keeper/internal/mcp
// passes its value through wiretime.UTC, or is named in encodeExempt. The
// schema walk above proves the huma format; this is what catches an answer
// encoded beside it — a pre-encoded json.RawMessage body, writeJSON, an MCP
// tool result, a stored target answered back raw.
func TestWireTimes_EveryEncoderGoesThroughUTC(t *testing.T) {
	used := map[string]bool{}
	sites := 0
	forEachGoFile(t, func(rel string, src []byte, fset *token.FileSet, file *ast.File) {
		if !strings.HasPrefix(rel, "api/") && !strings.HasPrefix(rel, "mcp/") {
			return
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "MarshalJSON" {
				continue
			}
			chained := map[ast.Node]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || chained[call] {
					return true
				}
				arg, isEncode := encodedValue(call)
				if !isEncode {
					return true
				}
				if sel := call.Fun.(*ast.SelectorExpr); sel.Sel.Name == "Encode" {
					chained[sel.X] = true
				}
				sites++
				if isWireUTC(arg) {
					return true
				}
				key := rel + ":" + fn.Name.Name + ":"
				if arg != nil {
					key += string(src[fset.Position(arg.Pos()).Offset:fset.Position(arg.End()).Offset])
				}
				if _, ok := encodeExempt[key]; ok {
					used[key] = true
					return true
				}
				t.Errorf("%s: %s encodes a value without wiretime.UTC; wrap it, or name %q in encodeExempt with the reason it can hold no instant",
					fset.Position(call.Pos()), fn.Name.Name, key)
				return true
			})
		}
	})
	for key := range encodeExempt {
		if !used[key] {
			t.Errorf("encodeExempt names %s, which encodes nothing without wiretime.UTC any more; drop the entry", key)
		}
	}
	if sites < 20 {
		t.Fatalf("found only %d encode sites; the scan is not reading the packages", sites)
	}
}

// encodedValue reports whether call is a JSON encode and returns the value it
// encodes (nil for a json.NewEncoder not chained into Encode).
func encodedValue(call *ast.CallExpr) (ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	switch {
	case isPkgCall(sel, "json", "Marshal"), isPkgCall(sel, "json", "MarshalIndent"):
		return call.Args[0], true
	case sel.Sel.Name == "Encode":
		if inner, ok := sel.X.(*ast.CallExpr); ok {
			if s, ok := inner.Fun.(*ast.SelectorExpr); ok && isPkgCall(s, "json", "NewEncoder") {
				return call.Args[0], true
			}
		}
	case sel.Sel.Name == "Marshal":
		if x, ok := sel.X.(*ast.SelectorExpr); ok && isPkgCall(x, "huma", "DefaultJSONFormat") {
			return call.Args[1], true
		}
	case isPkgCall(sel, "json", "NewEncoder"):
		return nil, true
	}
	return nil, false
}

func isPkgCall(sel *ast.SelectorExpr, pkg, name string) bool {
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg && sel.Sel.Name == name
}

func isWireUTC(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && isPkgCall(sel, "wiretime", "UTC")
}

// TestWireTimes_NoInstantFormattedOutsideUTC catches the instants the encoder
// never sees as a time: those formatted into a string first — an audit payload
// read back through the audit log, a window's text, a refusal's detail. An
// audit payload is built in whichever package acts, so every RFC 3339 Format in
// keeper/internal goes through .UTC(), and nothing moves it out of UTC again,
// in the same expression.
func TestWireTimes_NoInstantFormattedOutsideUTC(t *testing.T) {
	sites := 0
	forEachGoFile(t, func(_ string, _ []byte, fset *token.FileSet, file *ast.File) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Format" && sel.Sel.Name != "AppendFormat") || !hasRFC3339Arg(call) {
				return true
			}
			sites++
			if !receiverEndsInUTC(sel.X) {
				t.Errorf("%s: an instant is formatted as RFC 3339 without .UTC(); its offset is the Keeper host's zone", fset.Position(call.Pos()))
			}
			return true
		})
	})
	if sites < 20 {
		t.Fatalf("found only %d RFC 3339 formats; the scan is not reading the packages", sites)
	}
}

func hasRFC3339Arg(call *ast.CallExpr) bool {
	for _, a := range call.Args {
		if s, ok := a.(*ast.SelectorExpr); ok && (isPkgCall(s, "time", "RFC3339") || isPkgCall(s, "time", "RFC3339Nano")) {
			return true
		}
	}
	return false
}

// receiverEndsInUTC walks a method chain such as t.UTC().Truncate(time.Second)
// back from the Format and reports whether it meets .UTC() before anything
// that sets a zone again (In, Local).
func receiverEndsInUTC(e ast.Expr) bool {
	for {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		switch sel.Sel.Name {
		case "UTC":
			return len(call.Args) == 0
		case "In", "Local":
			return false
		}
		e = sel.X
	}
}

// forEachGoFile parses every non-test Go file under keeper/internal; rel is
// the path from there, the form encodeExempt is keyed by.
func forEachGoFile(t *testing.T, visit func(rel string, src []byte, fset *token.FileSet, file *ast.File)) {
	t.Helper()
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("..", path)
		if err != nil {
			return err
		}
		visit(filepath.ToSlash(rel), src, fset, file)
		return nil
	})
	if err != nil {
		t.Fatalf("walk keeper/internal: %v", err)
	}
}
