package middleware

// Structural guard over the LONG-LIVED CHANNELS of Keeper's operator surfaces
// (NIM-858, generalising the fix NIM-844 made one channel at a time).
//
// PROBLEM. A channel that outlives the request which authorized it has no next
// request, and [RejectRevoked] can only refuse a next request. So every such
// channel needs its own re-check, and NOTHING about writing one reminds you:
// the code compiles, the route answers 200, events flow, and the only symptom is
// a revoked Archon who keeps receiving them.
//
// That is not hypothetical, and it is not a single mistake. There were THREE
// channels of this class and they were found one at a time, months apart, by three
// separate reviews:
//
//	console WebSocket          GET /v1/console                          NIM-844
//	run-events SSE             GET /v1/incarnations/{id}/runs/{a}/events NIM-844
//	MCP apply-event SSE        GET /mcp/events                          NIM-858
//
// The third is the one that makes the case. ADR-068 §A3 wrote the second as a
// deliberate narrow DUPLICATE of the third, and NIM-844 then fixed the duplicate
// and left the original — with a comment in the fix saying so. Three independent
// discoveries of one defect class means the LIST was nowhere, so the fourth would
// have been found the same way: by somebody noticing.
//
// WHAT THIS FILE DOES. It derives the list from the source instead of keeping one,
// and requires every entry to be accounted for. Adding a channel with any of the
// markers below and not declaring it here is RED on the day it is written — which
// is the property that makes this worth more than the three patches.
//
// THE MARKERS: what OPENING a long-lived channel looks like on this surface.
//
//   - `text/event-stream` as a string literal — an SSE stream, both the two that
//     exist and any third;
//   - a `websocket.Upgrader` literal or an `.Upgrade(` call — the response is
//     replaced by a socket and the handler keeps it;
//   - a `huma.StreamResponse` literal — the handler returns and a body callback
//     keeps writing after it;
//   - an import of huma's own SSE helper, which writes the headers and holds the
//     stream INSIDE the library and so carries none of the three above at the call
//     site. It was the rejected alternative for the /v1 stream, which makes it the
//     obvious thing for a fourth channel to reach for.
//
// Matching is deliberately WIDE. A false match costs one line in
// notALongLivedChannel and records the decision; a false miss is an unguarded
// channel that looks guarded, which is the defect itself. `isSSERequest` below
// matches on a string literal it merely READS, and is declared rather than
// excluded by a cleverer matcher.
//
// WHAT IT DOES NOT COVER, deliberately and by name:
//
//   - WHETHER a declared re-check is CORRECT. This file proves a channel was
//     accounted for and that its file reaches the shared mechanism; that the
//     predicate decides the right thing is the subject of
//     longlived_reauth_test.go (console + /v1 stream) and
//     mcp/sse_reauth_test.go (/mcp/events), each of which mutates the loop away
//     and goes red.
//   - The Keeper↔Soul gRPC `EventStream` (ADR-012), which is long-lived and is
//     NOT this class: its peer is a Soul authenticated by mTLS client
//     certificate, not an Archon holding a JWT, and revoking a Soul is the
//     SoulSeed lifecycle rather than the RBAC snapshot. A guard over both would
//     have to name two different perimeters.
//   - A channel that opens without any of the markers — an `http.Hijacker` used
//     directly, say. Two exist in this package (audit.go, authlimit.go) and both
//     are pass-throughs that hijack nothing themselves; a handler that really
//     took the socket that way would be missed here. Add the marker when one
//     appears rather than assuming this list found it. The same goes for a shared
//     "write the SSE headers" helper: every channel through it would collapse
//     onto the helper's ONE site and the others would carry no marker. huma's own
//     SSE helper is exactly that shape, which is why its import is a marker of
//     its own.
//   - WHETHER A FILE'S SECOND CHANNEL RE-CHECKS. `usesSharedMechanism` is
//     computed per FILE, because the console's marker sites and its re-check are
//     legitimately three different declarations. So a second channel added to a
//     file that already calls [ReauthTicker] inherits the answer. What is still
//     red there is the entry itself: an undeclared channel is named, and a
//     declared one naming a `reauth` that its OWN FILE does not declare is named.
//     The residual gap is a channel deliberately declared with the neighbour's
//     predicate from the same file — a false statement in the registry rather than
//     an omission, and no guard over the source can tell those apart.
//   - Anything outside the keeper module.
//
// HOW TO BREAK IT ON PURPOSE — both were run as a real edit:
//   - add a handler that writes `text/event-stream` anywhere under keeper/ →
//     UNDECLARED LONG-LIVED CHANNEL names it;
//   - take [ReauthTicker] out of mcp/sse.go → the entry claiming to re-check is
//     reported as not reaching the shared mechanism.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// longLivedChannel — one channel that outlives its authorizing request, and what
// re-decides access while it is open.
type longLivedChannel struct {
	// reauth names the predicate the channel re-decides on, so a reader of this
	// list can go and read the rule rather than trusting the entry.
	reauth string
	// ticket is where the hole was found and closed.
	ticket string
}

// longLivedChannels — the inventory. A site matched below must be here or in
// notALongLivedChannel; being in neither is the defect this file exists for.
var longLivedChannels = map[string]longLivedChannel{
	// GET /v1/console — a PTY, unbounded in time, carrying an arbitrary root
	// shell. The most expensive of the three to leave open.
	"internal/api/console_ws.go:<package-level>:websocket.Upgrader": {
		reauth: "consoleConn.reauthorize", ticket: "NIM-844",
	},
	"internal/api/console_ws.go:consoleWSHandler:Upgrade()": {
		reauth: "consoleConn.reauthorize", ticket: "NIM-844",
	},

	// GET /v1/incarnations/{id}/runs/{apply_id}/events — up to sseMaxLifetime of
	// task plans and per-host results.
	"internal/api/huma_incarnation_runevents.go:registerHumaIncarnationRunEvents:huma.StreamResponse": {
		reauth: "runEventsStillAuthorized", ticket: "NIM-844",
	},
	"internal/api/huma_incarnation_runevents.go:streamRunEvents:text/event-stream": {
		reauth: "runEventsStillAuthorized", ticket: "NIM-844",
	},

	// GET /mcp/events — the same payloads on a listener [RejectRevoked] does not
	// run on at all (NIM-551), so this one had nothing anywhere that said no.
	"internal/mcp/sse.go:buildSSEHandler:text/event-stream": {
		reauth: "sseStillAuthorized", ticket: "NIM-858",
	},
}

// notALongLivedChannel — a site the matcher reaches that does not open a channel,
// with the reason. An entry here is a decision on record; silence would be the
// defect.
var notALongLivedChannel = map[string]string{
	"internal/api/middleware/auth.go:isSSERequest:text/event-stream": "READS the Accept header to classify an incoming request. " +
		"It opens nothing and holds nothing; the literal is a comparison operand",

	"internal/api/huma_incarnation_runevents.go:incRunEventsOperation:text/event-stream": "the OpenAPI media-type " +
		"DECLARATION for the operation. The stream it describes is the entry above, which re-checks",

	"internal/api/huma_console_recording.go:registerHumaConsoleRecordingCast:huma.StreamResponse": "a finite file copy, not a " +
		"subscription: the body writes out one stored recording and ends. Its length is the recording's, not the operator's " +
		"attention, and authorization plus the audit write both happen BEFORE the handler returns — so there is no window in " +
		"which rights could be withdrawn under a stream that is still being decided. A re-check here would have nothing to " +
		"re-decide between the first byte and the last",
}

// markers — the shapes that open a long-lived channel, in the order a reader of
// the key will meet them.
const (
	markerSSE       = "text/event-stream"
	markerUpgrader  = "websocket.Upgrader"
	markerUpgrade   = "Upgrade()"
	markerStreamRsp = "huma.StreamResponse"
	markerHumaSSE   = "huma/v2/sse"

	// humaSSEImport — huma's own SSE helper. It was the REJECTED alternative for
	// the /v1 run-events stream (huma_incarnation_runevents.go), so it is both in
	// the module graph and the obvious thing for a fourth channel to reach for —
	// and it emits no marker at the call site at all.
	humaSSEImport = "github.com/danielgtaylor/huma/v2/sse"
)

// TestLongLivedChannels_EveryOneIsAccountedFor is the completeness half: the set
// of channels declared above must be the set the keeper module's source opens.
func TestLongLivedChannels_EveryOneIsAccountedFor(t *testing.T) {
	for key, c := range longLivedChannels {
		if c.reauth == "" {
			t.Errorf("%s is listed as a long-lived channel with no re-check named — an entry that "+
				"names nothing is indistinguishable from one nobody checked", key)
		}
		if _, dup := notALongLivedChannel[key]; dup {
			t.Errorf("%s is in BOTH registries — the two must be disjoint, or an exemption silently "+
				"outranks a declared re-check", key)
		}
	}
	for key, why := range notALongLivedChannel {
		if why == "" {
			t.Errorf("%s is exempted with no reason recorded", key)
		}
	}

	found, declaredFuncs, err := longLivedSitesInModule(keeperModuleRoot)
	if err != nil {
		t.Fatalf("sweeping the keeper module: %v", err)
	}
	// The `reauth` name must resolve to a declaration the module actually has.
	// Without this the field is prose: an entry could name a predicate that was
	// deleted, or one that never existed, and read as an accounted-for channel.
	var phantom []string
	for key, c := range longLivedChannels {
		if c.reauth == "" {
			continue
		}
		// Scoped to the channel's OWN file. Module-wide the check would be almost
		// nothing — "Release" and "NewServer" are declared somewhere — and an
		// entry could point at any function in keeper and read as accounted for.
		file := key
		if i := strings.Index(key, ":"); i >= 0 {
			file = key[:i]
		}
		if !declaredFuncs[file+":"+c.reauth] {
			phantom = append(phantom, fmt.Sprintf("%s names %q, which %s does not declare", key, c.reauth, file))
		}
	}
	if len(phantom) > 0 {
		sort.Strings(phantom)
		t.Errorf("A CHANNEL NAMING A RE-CHECK THAT DOES NOT EXIST — %d:\n  %s\n"+
			"-> the predicate was renamed or removed, and the entry went on claiming the channel "+
			"re-decides anything ([NIM-858]).", len(phantom), strings.Join(phantom, "\n  "))
	}
	if len(found) == 0 {
		t.Fatal("the sweep matched no channel at all — either the module was restructured or the " +
			"matcher stopped recognising the shapes, and this test would pass vacuously from here on")
	}

	var undeclared, stale, unwired []string
	for key, site := range found {
		declared, isChannel := longLivedChannels[key]
		_, exempt := notALongLivedChannel[key]
		switch {
		case isChannel && exempt:
			// Already reported above.
		case !isChannel && !exempt:
			undeclared = append(undeclared, fmt.Sprintf("%s (%s)", key, site.at))
		case isChannel && !site.usesSharedMechanism:
			unwired = append(unwired, fmt.Sprintf("%s (%s), which says it re-checks via %s",
				key, site.at, declared.reauth))
		}
	}
	for key := range longLivedChannels {
		if _, ok := found[key]; !ok {
			stale = append(stale, "longLivedChannels: "+key)
		}
	}
	for key := range notALongLivedChannel {
		if _, ok := found[key]; !ok {
			stale = append(stale, "notALongLivedChannel: "+key)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(stale)
	sort.Strings(unwired)

	if len(undeclared) > 0 {
		t.Errorf("UNDECLARED LONG-LIVED CHANNEL — %d:\n  %s\n"+
			"-> a channel that outlives the request which authorized it cannot be reached by "+
			"RejectRevoked, so a revoked Archon keeps it until it closes on its own. Give it a "+
			"re-check on middleware.ReauthTicker and add it to longLivedChannels — or, if it opens "+
			"nothing that is held, say so in notALongLivedChannel with the reason. Do not leave it "+
			"unlisted: three of these were found one at a time because the list was nowhere ([NIM-858]).",
			len(undeclared), strings.Join(undeclared, "\n  "))
	}
	if len(unwired) > 0 {
		t.Errorf("A DECLARED CHANNEL THAT DOES NOT REACH THE SHARED MECHANISM — %d:\n  %s\n"+
			"-> the file opens the channel but never calls middleware.ReauthTicker, so either the "+
			"re-check was removed or it rolls its own ticker. A private cadence drifts away from the "+
			"reasoning in longlived.go without anything noticing ([NIM-858]).",
			len(unwired), strings.Join(unwired, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("STALE DECLARATION — %d entries naming a site the module no longer has:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// keeperModuleRoot — this package sits at keeper/internal/api/middleware.
const keeperModuleRoot = "../../.."

// longLivedSite — one matched marker: where it is, and whether the file it is in
// reaches the shared re-check.
type longLivedSite struct {
	at                  string
	usesSharedMechanism bool
}

// longLivedSitesInModule parses every non-test .go file under root and returns the
// marker sites it finds, keyed "<path>:<enclosing top-level decl>:<marker>".
//
// The key holds no line number on purpose: an edit above a site would otherwise
// invalidate an entry that describes the same channel.
func longLivedSitesInModule(root string) (map[string]longLivedSite, map[string]bool, error) {
	out := map[string]longLivedSite{}
	funcs := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// The root itself is spelled with dots (`../../..`), so the
			// hidden-directory rule must not be applied to it.
			if name := d.Name(); path != root &&
				(name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("%s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		shared := fileCallsReauthTicker(f)
		if importsHumaSSE(f) {
			// huma's own SSE helper writes the headers and holds the stream
			// inside the library, so a channel built on it carries NONE of the
			// three markers at the call site. The import is the only thing it
			// cannot hide.
			key := fmt.Sprintf("%s:<import>:%s", rel, markerHumaSSE)
			out[key] = longLivedSite{
				at:                  fmt.Sprintf("%s:%d", rel, fset.Position(f.Pos()).Line),
				usesSharedMechanism: shared,
			}
		}
		for _, decl := range f.Decls {
			where := "<package-level>"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				where = fd.Name.Name
				funcs[rel+":"+qualifiedFuncName(fd)] = true
			}
			for _, marker := range markersIn(decl) {
				key := fmt.Sprintf("%s:%s:%s", rel, where, marker)
				if _, dup := out[key]; dup {
					// A second site of the same marker in the same declaration is
					// the same channel said twice, not a second channel.
					continue
				}
				out[key] = longLivedSite{
					at:                  fmt.Sprintf("%s:%d", rel, fset.Position(decl.Pos()).Line),
					usesSharedMechanism: shared,
				}
			}
		}
		return nil
	})
	return out, funcs, err
}

// qualifiedFuncName renders a declaration the way longLivedChannels names it:
// `Func` for a plain function, `Receiver.Method` for a method.
func qualifiedFuncName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// importsHumaSSE reports whether the file pulls in huma's SSE helper.
func importsHumaSSE(f *ast.File) bool {
	for _, imp := range f.Imports {
		if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == humaSSEImport {
			return true
		}
	}
	return false
}

// markersIn returns the distinct channel-opening markers inside one declaration.
func markersIn(decl ast.Decl) []string {
	seen := map[string]bool{}
	ast.Inspect(decl, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.BasicLit:
			if t.Kind == token.STRING {
				if v, err := strconv.Unquote(t.Value); err == nil && v == markerSSE {
					seen[markerSSE] = true
				}
			}
		case *ast.CompositeLit:
			switch qualifiedName(t.Type) {
			case "websocket.Upgrader":
				seen[markerUpgrader] = true
			case "huma.StreamResponse":
				seen[markerStreamRsp] = true
			}
		case *ast.CallExpr:
			if s, ok := t.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Upgrade" {
				seen[markerUpgrade] = true
			}
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// fileCallsReauthTicker reports whether the file reaches the shared mechanism, by
// name — `middleware.ReauthTicker(…)` from another package, or `ReauthTicker(…)`
// from this one.
func fileCallsReauthTicker(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.SelectorExpr:
			if t.Sel.Name == "ReauthTicker" {
				found = true
			}
		case *ast.Ident:
			if t.Name == "ReauthTicker" {
				found = true
			}
		}
		return !found
	})
	return found
}

// qualifiedName renders `pkg.Type` for a selector type expression, unwrapping a
// pointer; anything else returns "".
func qualifiedName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	s, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	x, ok := s.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return x.Name + "." + s.Sel.Name
}
