package api

// Structural guard against a create body being ACCEPTED AND DROPPED ([NIM-817]).
//
// PROBLEM. A create route binds one struct for the schema (huma) and calls the
// handler with another (handler-native), and the projection between them is a
// hand-written field list. Omit a line and there is no symptom anywhere: the body
// validates against a schema that declares the field, the route answers 201, and
// the value is gone. No existing test fails, because every per-domain test sends
// only the fields it already knows about. `label` was lost that way on all EIGHT
// registries at once for the whole of NIM-728/729 — the schema declared it, the
// handler input carried it, the domain stored it, and eight literals in
// huma_<domain>.go did not mention it.
//
// That silence is the defect, not the field. A client that gets a 201 cannot tell
// "stored" from "discarded" without reading the row back, so the wire contract is
// not "the schema says the field exists" — it is "a field the schema accepts
// reaches the write".
//
// DETECTION MECHANISM — two halves, and neither is sufficient alone:
//
//  1. COMPLETENESS, derived from the spec rather than from a list: every POST
//     operation in buildFullOpenAPISpec whose request body declares `label` must
//     appear in createBodyProjections. A ninth registry that grows a caption and
//     forgets the projection goes red here. Derived from the spec for the same
//     reason the audit guard derives its route set — a hand-maintained inventory
//     of what to check has the exact failure mode it is checking for.
//
//  2. PER-FIELD, by reflection over the real projection function, in two passes.
//     ALL FIELDS AT ONCE, each holding a value unique to it: the native field of
//     the same name must hold that same value. Uniqueness is what makes this catch
//     mis-SOURCING (`Label: b.Task`) and not only omission — an earlier revision
//     compared against the zero value and would have passed a projection that wrote
//     the task selector into the caption column. Then ONE FIELD AT A TIME, which
//     identifies each field's source outright and so covers the types that have too
//     few values to be unique.
//
// This half generalises past `label`: it is not a test that captions survive, it
// is a test that nothing is dropped or cross-wired, so the next field added to one
// of these eight bodies is covered the day it is added.
//
// WHAT IT DOES NOT COVER, deliberately and by name:
//
//   - The OTHER hand-written projections in this package, of which there are NINE:
//     toCadenceCreateRequest and toCadencePatchRequest (huma_cadence.go),
//     toVoyageCreateRequest (huma_voyage.go), toPushApplyInput (huma_push_op.go),
//     toModuleFormPrepInput (huma_module_op.go), and toSoulCovenAssignInput,
//     toSoulTraitsAssignInput, toSoulSshTargetInput, toErrandExecRequest
//     (huma_soul.go). They have the
//     same shape and the same hazard, but they TRANSFORM as they project
//     (toNotifyRequests, derefBool, marshalAnnotations, "" → nil), so a
//     value-equality guard would report a false drop on every transformed field.
//     Covering them needs per-field transform declarations, which is a larger
//     piece of work than this ticket; they were hand-checked clean at the time of
//     writing. Tracked in [NIM-824] — do not read their absence here as coverage.
//   - Nothing on the count of value-indistinguishable types — but only because
//     there are TWO passes. A `bool` holds one bit, so the all-fields pass cannot
//     tell two of them apart however it fills them: with three *bool on
//     TidingCreateRequest, `Enabled: b.OnlyFailures` was green under that pass
//     alone. TestCreateProjections_ReadEachFieldFromItsOwnSource sets one field at
//     a time and closes it.
//   - A field whose type changes shape across the boundary AND whose native form
//     does not share the wire form's field names — `Subject` → subject.Selector is
//     the only one here. Those are checked for PRESENCE only; `selector()` itself
//     is pinned in huma_subject.go's own tests.
//
// HOW TO BREAK IT ON PURPOSE (the mutations this file exists to catch):
//   - delete the `Label:` line from any of the eight functions in
//     huma_create_input.go → the route's subtest names that field;
//   - change `Label: b.Label` to `Label: b.Task` in toTidingCreateInput → same,
//     reported as a value mismatch rather than as a drop;
//   - change `Enabled: b.Enabled` to `Enabled: b.OnlyFailures` there → caught by
//     the one-field-at-a-time pass, and by that pass only.
//
// The remaining hole — a route that stops CALLING its projection — is not
// reachable from here, because this drives the functions and not the routes.
// TestLabelOnCreate_ReachesTheWriteAndTheReply (huma_label_test.go) closes it FOR
// THE CAPTION by driving all eight routes over HTTP. Only for the caption: a
// closure that went back to an inline literal carrying `Label` but dropping some
// other field would leave both guards green.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// createBodyProjection — one create route's wire→native projection, in a form a
// test can drive: `body` is a zero value of the request struct (reflection fills
// it), `project` runs the production function on a filled copy.
//
// unmapped names request fields the projection deliberately does not carry, with
// the reason. Empty on every route today: all eight bodies map field-for-field by
// name. An entry here is a decision on record — the alternative, an unexplained
// gap, is the defect this file exists for.
type createBodyProjection struct {
	body     any
	project  func(any) any
	unmapped map[string]string
}

// createBodyProjections — the eight create routes whose body carries a caption
// ([ADR-0085]). Keyed by route so the completeness half can compare against the
// spec's own topology.
var createBodyProjections = map[route]createBodyProjection{
	{http.MethodPost, "/v1/services"}: {
		body:    ServiceRegisterRequest{},
		project: func(b any) any { return toServiceRegisterInput(b.(ServiceRegisterRequest)) },
	},
	{http.MethodPost, "/v1/incarnations"}: {
		body:    IncarnationCreateRequest{},
		project: func(b any) any { return toIncarnationCreateInput(b.(IncarnationCreateRequest)) },
	},
	{http.MethodPost, "/v1/heralds"}: {
		body:    HeraldCreateRequest{},
		project: func(b any) any { return toHeraldCreateInput(b.(HeraldCreateRequest)) },
	},
	{http.MethodPost, "/v1/tidings"}: {
		body:    TidingCreateRequest{},
		project: func(b any) any { return toTidingCreateInput(b.(TidingCreateRequest)) },
	},
	{http.MethodPost, "/v1/vigils"}: {
		body:    VigilCreateRequest{},
		project: func(b any) any { return toVigilCreateInput(b.(VigilCreateRequest)) },
	},
	{http.MethodPost, "/v1/decrees"}: {
		body:    DecreeCreateRequest{},
		project: func(b any) any { return toDecreeCreateInput(b.(DecreeCreateRequest)) },
	},
	{http.MethodPost, "/v1/augur/omens"}: {
		body:    OmenCreateRequest{},
		project: func(b any) any { return toOmenCreateInput(b.(OmenCreateRequest)) },
	},
	{http.MethodPost, "/v1/push-providers"}: {
		body:    PushProviderCreateRequest{},
		project: func(b any) any { return toPushProviderCreateInput(b.(PushProviderCreateRequest)) },
	},
}

// labelIsNotACaption — POST routes whose body carries a `label` that is NOT a
// display caption, with the reason. The spec sweep below cannot tell the two apart
// by shape: both are a JSON string called `label`, and only the meaning differs.
//
// Listing them is the deliberate half of the same decision the projections are: an
// entry here says "checked, different field", where silence would say nothing.
var labelIsNotACaption = map[route]string{
	{http.MethodPost, "/v1/souls/coven"}: "ADR-008: `label` here is a COVEN TAG being appended or removed in bulk, " +
		"not a display caption — the route creates nothing and the value is already carried by " +
		"toSoulCovenAssignInput (huma_soul.go)",
}

// TestCreateProjections_CarryEveryRequestField drives each projection with a body
// in which every field holds a value unique to it, and pins that every field
// arrives unchanged and from the right source.
func TestCreateProjections_CarryEveryRequestField(t *testing.T) {
	for r, p := range createBodyProjections {
		t.Run(r.String(), func(t *testing.T) {
			filled := reflect.New(reflect.TypeOf(p.body)).Elem()
			f := &uniqueFiller{}
			f.fill(t, filled, r.String())

			native := reflect.ValueOf(p.project(filled.Interface()))
			if native.Kind() != reflect.Struct {
				t.Fatalf("projection returned %s, want a struct", native.Kind())
			}
			assertFieldsCarried(t, filled, native, p.unmapped, filled.Type().Name())
		})
	}
}

// assertFieldsCarried compares a filled wire value against the projection's output,
// field by field and BY NAME, recursing into nested structs whose shape carries
// over.
//
// The three outcomes, in order of strength:
//
//   - same type → the values must be EQUAL. This is the check that catches
//     cross-wiring, and it is the one that applies to almost every field.
//   - the native side is a pointer to the wire's type (a map that becomes a *map
//     because "omitted" and "empty" differ to the write) → dereference and compare
//     equal. Without this, a projection that pointed at a fresh empty map would
//     read as carrying the caller's.
//   - shape changed → PRESENCE only, and the reason is logged rather than passed
//     over in silence, so a reader of the output knows which fields got the weaker
//     check.
func assertFieldsCarried(t *testing.T, wire, native reflect.Value, unmapped map[string]string, path string) {
	t.Helper()
	wireT := wire.Type()
	for i := 0; i < wireT.NumField(); i++ {
		f := wireT.Field(i)
		if !f.IsExported() {
			continue
		}
		at := path + "." + f.Name
		if why, ok := unmapped[f.Name]; ok {
			if why == "" {
				t.Errorf("field %s is listed as unmapped with no reason; "+
					"an unexplained gap is what this guard exists to catch", at)
			}
			continue
		}

		got := native.FieldByName(f.Name)
		if !got.IsValid() {
			t.Errorf("request field %s has NO field of that name on the handler input %s.\n"+
				"The schema accepts it, so a caller will send it and get a 201 — and it reaches nothing. "+
				"Carry it in huma_create_input.go, or list it in `unmapped` with the reason it is dropped.",
				at, native.Type())
			continue
		}
		assertValueCarried(t, wire.Field(i), got, at, native.Type())
	}
}

// assertValueCarried is one field's comparison; see assertFieldsCarried for the
// three outcomes.
func assertValueCarried(t *testing.T, want, got reflect.Value, at string, nativeT reflect.Type) {
	t.Helper()

	if want.Type() == got.Type() {
		if reflect.DeepEqual(want.Interface(), got.Interface()) {
			return
		}
		if got.IsZero() {
			t.Errorf("request field %s was SET on the body and arrives empty on %s — "+
				"accepted and silently dropped ([NIM-817]).\n"+
				"Add `%s: b.%s` to the projection in huma_create_input.go.",
				at, nativeT, lastSegment(at), lastSegment(at))
			return
		}
		t.Errorf("request field %s arrives on %s holding %v, want %v — the projection carries "+
			"a value for this field, but not THIS field's value: it is reading the wrong "+
			"source ([NIM-817]).\nCheck the right-hand side of `%s:` in huma_create_input.go.",
			at, nativeT, render(got), render(want), lastSegment(at))
		return
	}

	// A map or slice that becomes a pointer to itself across the boundary.
	if got.Kind() == reflect.Pointer && got.Type().Elem() == want.Type() {
		if got.IsNil() {
			t.Errorf("request field %s was SET on the body and arrives nil on %s — "+
				"accepted and silently dropped ([NIM-817]).", at, nativeT)
			return
		}
		assertValueCarried(t, want, got.Elem(), at, nativeT)
		return
	}

	// A nested struct whose field names all carry over — recurse, so a projection
	// that builds the nested value with an inline literal cannot drop one of its
	// members and still read as "carried".
	wantS, gotS := derefStruct(want), derefStruct(got)
	if wantS.IsValid() && gotS.IsValid() && sameFieldNames(wantS.Type(), gotS.Type()) {
		if got.Kind() == reflect.Pointer && got.IsNil() {
			t.Errorf("request field %s was SET on the body and arrives nil on %s — "+
				"accepted and silently dropped ([NIM-817]).", at, nativeT)
			return
		}
		assertFieldsCarried(t, wantS, gotS, nil, at)
		return
	}

	// Shape changed beyond what can be compared by value: presence only.
	if got.IsZero() {
		t.Errorf("request field %s was SET on the body and is zero on %s — "+
			"accepted and silently dropped ([NIM-817]).\n"+
			"Add `%s: b.%s` to the projection in huma_create_input.go.",
			at, nativeT, lastSegment(at), lastSegment(at))
		return
	}
	t.Logf("%s: %s -> %s changes shape; checked for PRESENCE only, not value",
		at, want.Type(), got.Type())
}

// derefStruct returns v as a struct value, following one pointer, or an invalid
// value when v is not a struct in either form.
func derefStruct(v reflect.Value) reflect.Value {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return v
}

// sameFieldNames reports whether every exported field of a has a field of that name
// on b — the precondition for recursing into a nested pair.
//
// Every name, not merely one: subject.Selector answers to `Incarnation` while
// spelling the rest of the wire Subject's dimensions differently (`Coven` against
// `Covens`), and recursing on that partial overlap would report the wire spelling
// as a dropped field when the projection is correct.
func sameFieldNames(a, b reflect.Type) bool {
	exported := 0
	for i := 0; i < a.NumField(); i++ {
		if a.Field(i).IsExported() {
			exported++
		}
	}
	// No exported fields to compare: recursing would assert nothing and report
	// success. Fall through to the presence check instead, which at least fails on
	// a dropped value.
	if exported == 0 {
		return false
	}
	for i := 0; i < a.NumField(); i++ {
		f := a.Field(i)
		if !f.IsExported() {
			continue
		}
		if _, ok := b.FieldByName(f.Name); !ok {
			return false
		}
	}
	return true
}

func lastSegment(path string) string { return path[strings.LastIndex(path, ".")+1:] }

// render prints a value with pointers followed, so a mismatch report names the two
// captions rather than two addresses.
func render(v reflect.Value) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "nil"
		}
		return fmt.Sprintf("&%v", v.Elem().Interface())
	}
	return fmt.Sprintf("%v", v.Interface())
}

// TestCreateProjections_ReadEachFieldFromItsOwnSource fills ONE request field at a
// time and requires the native field of that name to be the one that arrives.
//
// It exists because value-uniqueness cannot settle every type. A `bool` holds one
// bit, so with three `*bool` on TidingCreateRequest two of them necessarily carry
// the same value however the filler chooses them, and `Enabled: b.OnlyFailures`
// was green under the all-fields pass. Here it is not: with only `Enabled` set,
// a projection reading `OnlyFailures` reads nil and the field arrives empty.
//
// One field at a time is what makes the source unambiguous, and it is a sound
// fixture ONLY because these eight projections are straight copies — no field's
// treatment depends on another's. A projection that branched on a second field
// would need its own case rather than this loop.
//
// The two passes are complementary and both are needed: this one identifies the
// SOURCE of each field but says nothing about a projection that misbehaves on a
// fully-populated body, which is the shape a real request has.
func TestCreateProjections_ReadEachFieldFromItsOwnSource(t *testing.T) {
	for r, p := range createBodyProjections {
		t.Run(r.String(), func(t *testing.T) {
			wireT := reflect.TypeOf(p.body)
			for i := 0; i < wireT.NumField(); i++ {
				f := wireT.Field(i)
				if !f.IsExported() {
					continue
				}
				if _, skip := p.unmapped[f.Name]; skip {
					continue
				}
				t.Run(f.Name, func(t *testing.T) {
					body := reflect.New(wireT).Elem()
					at := wireT.Name() + "." + f.Name
					(&uniqueFiller{oneHot: true}).fill(t, body.Field(i), at)

					native := reflect.ValueOf(p.project(body.Interface()))
					got := native.FieldByName(f.Name)
					if !got.IsValid() {
						// The missing-field case is reported once, by the other pass.
						return
					}
					if got.IsZero() {
						t.Errorf("with ONLY %s set on the body, %s.%s arrives empty — "+
							"the projection is not reading this field ([NIM-817]).\n"+
							"Either the `%s:` line is missing from huma_create_input.go, or its "+
							"right-hand side names a different field of the request.",
							at, native.Type(), f.Name, f.Name)
					}
				})
			}
		})
	}
}

// TestCreateProjections_CoverEveryCaptionedCreateRoute is the completeness half:
// the set of routes above must be the set the SPEC says exists.
//
// A registry that grows a caption on create and never wires the projection is the
// exact recurrence of [NIM-817], and it is invisible to the per-field half — that
// one only checks routes it already knows about. The spec is the source of the set
// for the same reason the audit guard uses it: an inventory maintained by hand
// forgets things in precisely the situation it is meant to catch.
//
// Its own blind spots, since they bound what a green result means: a body schema
// reached through a second level of `$ref` or through `allOf`, and any create that
// is not a POST.
func TestCreateProjections_CoverEveryCaptionedCreateRoute(t *testing.T) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	captioned := map[route]struct{}{}
	for path, item := range spec.Paths {
		op, ok := pathItemOps(item)[http.MethodPost]
		if !ok {
			continue
		}
		if bodyDeclaresLabel(spec, op) {
			captioned[route{method: http.MethodPost, path: normalizePath(path)}] = struct{}{}
		}
	}
	if len(captioned) == 0 {
		t.Fatal("no POST route in the spec declares `label` in its body — either the schemas regressed " +
			"or the resolver below stopped following $ref, and the guard would pass vacuously")
	}

	var missing, stale []string
	for r := range captioned {
		_, projected := createBodyProjections[r]
		why, exempt := labelIsNotACaption[r]
		switch {
		case projected && exempt:
			t.Errorf("%s is in both createBodyProjections and labelIsNotACaption — "+
				"the two registries must be disjoint", r)
		case exempt && why == "":
			t.Errorf("%s is exempt with no reason recorded", r)
		case !projected && !exempt:
			missing = append(missing, r.String())
		}
	}
	for r := range createBodyProjections {
		if _, ok := captioned[r]; !ok {
			stale = append(stale, "createBodyProjections: "+r.String())
		}
	}
	for r := range labelIsNotACaption {
		if _, ok := captioned[r]; !ok {
			stale = append(stale, "labelIsNotACaption: "+r.String())
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	if len(missing) > 0 {
		t.Errorf("CREATE ROUTE ACCEPTS `label` WITH NO GUARDED PROJECTION — %d:\n  %s\n"+
			"-> add each to createBodyProjections. Until then nothing proves the caption reaches the write, "+
			"which is the whole of [NIM-817]. If the field is not a display caption, say so in "+
			"labelIsNotACaption.", len(missing), strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("STALE DECLARATION — %d entries with no matching POST route declaring `label` "+
			"in the spec:\n  %s", len(stale), strings.Join(stale, "\n  "))
	}
}

// bodyDeclaresLabel reports whether the operation's JSON request body has a `label`
// property, following one level of $ref into components (huma emits the body schema
// as a reference).
func bodyDeclaresLabel(spec *huma.OpenAPI, op *huma.Operation) bool {
	if op == nil || op.RequestBody == nil {
		return false
	}
	mt, ok := op.RequestBody.Content["application/json"]
	if !ok || mt.Schema == nil {
		return false
	}
	sch := mt.Schema
	if sch.Ref != "" {
		name := sch.Ref[strings.LastIndex(sch.Ref, "/")+1:]
		resolved, ok := spec.Components.Schemas.Map()[name]
		if !ok {
			return false
		}
		sch = resolved
	}
	_, has := sch.Properties["label"]
	return has
}

// uniqueFiller writes a value into every exported field reachable from a request
// struct, and — for every type that can hold one — a value no other field holds.
//
// Uniqueness is the point. A filler that wrote the same token everywhere would
// prove only that a field is non-empty afterwards, which a projection reading the
// WRONG source field satisfies just as well as a correct one.
//
// Values are shaped to be plausible rather than minimal — a one-element slice, a
// one-entry map, an allocated pointer — because a projection may legitimately drop
// an EMPTY collection (push-provider params keeps nil rather than pointing at an
// empty map), and a fixture that could not tell "empty" from "absent" would let the
// real omission through.
type uniqueFiller struct {
	n int
	// oneHot is set by the one-field-at-a-time pass, where only one field is
	// filled and uniqueness is therefore irrelevant — what matters is that the
	// value is NON-ZERO, so a bool must be true rather than alternating.
	oneHot bool
}

func (f *uniqueFiller) next() int { f.n++; return f.n }

func (f *uniqueFiller) fill(t *testing.T, v reflect.Value, where string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(fmt.Sprintf("guard-%d", f.next()))
	case reflect.Bool:
		// A bool holds one bit and cannot be made unique, so the all-fields pass
		// cannot tell two of them apart whatever it writes here. That gap is closed
		// by the one-field-at-a-time pass instead, where the only bool set is this
		// one and `true` is the whole signal.
		v.SetBool(f.oneHot || f.next()%2 == 0)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(f.next()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(f.next()))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(f.next()))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		f.fill(t, v.Elem(), where)
	case reflect.Interface:
		if v.NumMethod() != 0 {
			t.Fatalf("%s: the filler cannot produce a value for the interface %s — "+
				"teach it one rather than letting the field look dropped", where, v.Type())
		}
		v.Set(reflect.ValueOf(fmt.Sprintf("guard-%d", f.next())))
	case reflect.Slice:
		// json.RawMessage is a []byte that must PARSE, not an arbitrary byte: a
		// projection is free to unmarshal it, and garbage there would fail for a
		// reason that has nothing to do with what is guarded here.
		if v.Type() == reflect.TypeOf(json.RawMessage(nil)) {
			v.Set(reflect.ValueOf(json.RawMessage(fmt.Sprintf(`{"guard":%d}`, f.next()))))
			return
		}
		elem := reflect.New(v.Type().Elem()).Elem()
		f.fill(t, elem, where)
		v.Set(reflect.Append(v, elem))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key := reflect.New(v.Type().Key()).Elem()
		f.fill(t, key, where)
		val := reflect.New(v.Type().Elem()).Elem()
		f.fill(t, val, where)
		v.SetMapIndex(key, val)
	case reflect.Struct:
		exported := 0
		for i := 0; i < v.NumField(); i++ {
			if !v.Type().Field(i).IsExported() {
				continue
			}
			exported++
			f.fill(t, v.Field(i), where)
		}
		// An opaque struct (time.Time, netip.Addr) would be left at its zero value,
		// and the comparison would then pass whatever the projection did with it.
		if exported == 0 {
			t.Fatalf("%s: the filler cannot produce a distinguishable %s — it has no "+
				"exported fields, so the field would look carried whatever happens to it",
				where, v.Type())
		}
	default:
		t.Fatalf("%s: the filler cannot produce a distinguishable %s — "+
			"a field of an unhandled kind would look dropped whatever the projection does", where, v.Kind())
	}
}
