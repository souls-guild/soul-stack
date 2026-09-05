// Guards on parameter typing at the artifact's boundary (NIM-800, the mongo half
// of NIM-778).
//
// ★ WHAT THESE ASSERT, AND WHY IT IS NOT "the password is not in argv"
//
// The live defect was `tls: "true"` written as a string: [boolOrDefault] fell back
// to `false`, the connection to mongod went out in PLAINTEXT with the admin
// password in it, and the step reported success. So the assertion has to be that
// the step is REFUSED. Asserting instead that the password does not appear in the
// command arguments would pass on a build that lost the parameter altogether, and
// pass on the broken build too — the password was never in argv, it was in the
// connection.
//
// And it has to hold on the APPLY path, not only in Validate: the runtime calls
// Apply, and a runner is free never to call Validate at all.
package main

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestApply_WrongTypedTLS_IsRefusedAndNeverConnects is the guard for the defect
// itself: a string where a boolean is declared must stop the step BEFORE a socket
// is opened, not silently downgrade the connection.
func TestApply_WrongTypedTLS_IsRefusedAndNeverConnects(t *testing.T) {
	conn := &fakeConn{}
	connects := 0
	m := &MongoModule{connect: func(_ context.Context, cfg connConfig) (mongoConn, error) {
		connects++
		conn.cfg = cfg
		return conn, nil
	}}

	stream := &applyStream{}
	_ = m.instance().Apply(&pluginv1.ApplyRequest{
		State: "pinged",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"tls":      "true", // a STRING where a boolean is declared
		}),
	}, stream)

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "params.tls") || !strings.Contains(msg, "boolean") {
		t.Errorf("refusal must name the param and the expected type, got %q", msg)
	}
	if connects != 0 {
		t.Errorf("a refused step still opened %d connection(s) — the plaintext leak is exactly this", connects)
	}
	if conn.pinged || len(conn.calls) != 0 {
		t.Errorf("a refused step still reached mongod: pinged=%v calls=%v", conn.pinged, conn.calls)
	}
	assertEventsNoSecret(t, stream)
}

// TestApply_WrongTypedPassword_IsRefusedNotAnonymous — the other direction of the
// same fallback: `stringOrEmpty` on a number gives "", which is an ANONYMOUS
// connection where an authenticated one was declared.
func TestApply_WrongTypedPassword_IsRefusedNotAnonymous(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)

	stream := &applyStream{}
	_ = m.instance().Apply(&pluginv1.ApplyRequest{
		State: "pinged",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": 12345, // a NUMBER where a string is declared
		}),
	}, stream)

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "params.password") {
		t.Errorf("refusal must name params.password, got %q", msg)
	}
	if conn.cfg.addr != "" {
		t.Error("a refused step still built a connection config")
	}
}

// TestValidate_WrongTypedParam_IsRefused — the same refusal on the Validate path,
// which is where an author should meet it.
func TestValidate_WrongTypedParam_IsRefused(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.instance().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "pinged",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017", "tls_skip_verify": "yes"}),
	})
	if reply.GetOk() {
		t.Fatal("expected Ok=false for a string where a boolean is declared")
	}
	if len(reply.GetErrors()) == 0 || !strings.Contains(reply.GetErrors()[0], "params.tls_skip_verify") {
		t.Errorf("refusal must name the param, got %v", reply.GetErrors())
	}
}

// TestApply_WrongTypedParam_OnANewObject — the check is derived from [object.decl],
// so an object added later inherits it without anyone remembering to. This is the
// NIM-805 half: `wait_primary_seconds` is declared Int, and a string there must be
// refused before the replica-set connection is opened.
func TestApply_WrongTypedParam_OnANewObject(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)

	stream := &applyStream{}
	_ = m.replicaset().Apply(&pluginv1.ApplyRequest{
		State: "initiated",
		Params: mustStruct(t, map[string]any{
			"addr":                 "127.0.0.1:27017",
			"name":                 "rs0",
			"members":              map[string]any{"a": map[string]any{"host": "h1:27017"}},
			"wait_primary_seconds": "60",
		}),
	}, stream)

	msg := failureMessage(t, stream)
	if !strings.Contains(msg, "params.wait_primary_seconds") || !strings.Contains(msg, "integer") {
		t.Errorf("refusal must name the param and the expected type, got %q", msg)
	}
	if len(conn.calls) != 0 {
		t.Errorf("a refused step still reached mongod: %v", conn.calls)
	}
}

// TestCheckParamTypes_Table covers each declared type, plus the three cases the
// check deliberately does NOT report.
func TestCheckParamTypes_Table(t *testing.T) {
	decl := module.Input{
		"s": {Type: module.String},
		"b": {Type: module.Bool},
		"i": {Type: module.Int},
		"n": {Type: module.Number},
		"l": {Type: module.List},
		"m": {Type: module.Map},
	}

	cases := []struct {
		name    string
		fields  map[string]any
		refused bool
	}{
		{"string ok", map[string]any{"s": "x"}, false},
		{"string given a number", map[string]any{"s": 1}, true},
		{"bool ok", map[string]any{"b": true}, false},
		{"bool given a string", map[string]any{"b": "true"}, true},
		{"int ok", map[string]any{"i": 7}, false},
		{"int given a fraction is NOT truncated", map[string]any{"i": 7.5}, true},
		{"number takes a fraction", map[string]any{"n": 7.5}, false},
		{"list ok", map[string]any{"l": []any{1}}, false},
		{"list given a map", map[string]any{"l": map[string]any{}}, true},
		{"map ok", map[string]any{"m": map[string]any{"k": 1}}, false},
		{"map given a list", map[string]any{"m": []any{}}, true},

		// The three it leaves alone, on purpose.
		{"an ABSENT key is what a default is for", map[string]any{}, false},
		{"a NULL means unset, not a wrong type", map[string]any{"b": nil}, false},
		{"an UNDECLARED key is the engine's unknown_param", map[string]any{"nope": 1}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := checkParamTypes(decl, mustStruct(t, tc.fields).GetFields())
			if tc.refused && len(errs) == 0 {
				t.Errorf("expected a refusal, got none")
			}
			if !tc.refused && len(errs) > 0 {
				t.Errorf("expected no refusal, got %v", errs)
			}
		})
	}
}

// TestCheckParamTypes_DeterministicOrder — two wrong params must be reported in a
// stable order, or the same input gives a different message every run.
func TestCheckParamTypes_DeterministicOrder(t *testing.T) {
	decl := module.Input{"alpha": {Type: module.Bool}, "beta": {Type: module.Bool}}
	fields := mustStruct(t, map[string]any{"beta": "x", "alpha": "x"}).GetFields()

	first := strings.Join(checkParamTypes(decl, fields), "; ")
	for i := 0; i < 20; i++ {
		if got := strings.Join(checkParamTypes(decl, fields), "; "); got != first {
			t.Fatalf("report order is not stable: %q vs %q", first, got)
		}
	}
	if !strings.HasPrefix(first, "params.alpha") {
		t.Errorf("expected the report sorted by param name, got %q", first)
	}
}

// TestNestedReaders_RefuseRatherThanDefault — [checkParamTypes] reaches a declared
// map and stops, so the readers inside a nested spec carry the rule the rest of the
// way. `priority: "0"` falling back to the default 1 would make a member the
// operator pinned out of elections able to win one.
func TestNestedReaders_RefuseRatherThanDefault(t *testing.T) {
	spec := mustStruct(t, map[string]any{
		"str":  1,
		"int":  "2",
		"num":  "3",
		"bool": "true",
		"map":  []any{},
		"list": map[string]any{},
	}).GetFields()

	if _, err := stringField(spec, "str", "params.x.str", ""); err == nil {
		t.Error("stringField accepted a number")
	}
	if _, err := intField(spec, "int", "params.x.int", 1); err == nil {
		t.Error("intField accepted a string")
	}
	if _, err := numberField(spec, "num", "params.x.num", 1); err == nil {
		t.Error("numberField accepted a string")
	}
	if _, err := boolField(spec, "bool", "params.x.bool", false); err == nil {
		t.Error("boolField accepted a string — this is the NIM-778 fallback, one level down")
	}
	if _, err := mapField(spec, "map", "params.x.map"); err == nil {
		t.Error("mapField accepted a list")
	}
	if _, err := listFieldOf(spec, "list", "params.x.list"); err == nil {
		t.Error("listFieldOf accepted a map")
	}
}

// TestNestedReaders_AbsentAndNullTakeTheDefault — the same two cases the top-level
// check leaves alone must behave the same one level down, or a member spec written
// with `hidden:` and nothing after it would be refused rather than read as unset.
func TestNestedReaders_AbsentAndNullTakeTheDefault(t *testing.T) {
	spec := mustStruct(t, map[string]any{"given": nil}).GetFields()

	for _, key := range []string{"given", "missing"} {
		if got, err := intField(spec, key, "params.x", 42); err != nil || got != 42 {
			t.Errorf("intField(%q) = %v, %v; want the default 42", key, got, err)
		}
		if got, err := boolField(spec, key, "params.x", true); err != nil || !got {
			t.Errorf("boolField(%q) = %v, %v; want the default true", key, got, err)
		}
	}
}

// TestEveryObjectCarriesItsDecl — the check is only live on an object that wired
// its declaration in, and one added later that forgets to would silently accept
// every coercion this file exists to refuse. `decl` is not optional.
func TestEveryObjectCarriesItsDecl(t *testing.T) {
	for name, obj := range objects(&MongoModule{}) {
		if len(obj.decl) == 0 {
			t.Errorf("object %q carries no decl — params.go cannot refuse anything on it", name)
			continue
		}
		for _, state := range obj.states() {
			if _, ok := obj.decl[state]; !ok {
				t.Errorf("%s.%s is dispatched but its declaration is not in decl", name, state)
			}
		}
	}
}

// TestConnectInputIsNotShared — [connectInput] must return a FRESH map: the value
// reaches both the schema renderer and [object.decl], and a shared one would let a
// mutation on one action's declaration reach another's.
func TestConnectInputIsNotShared(t *testing.T) {
	a := connectInput(module.Input{"own": {Type: module.String}})
	b := connectInput(nil)

	a["addr"] = module.Param{Type: module.Bool}
	if b["addr"].Type != module.String {
		t.Error("connectInput returned a shared map: editing one action's declaration changed another's")
	}
	if _, leaked := b["own"]; leaked {
		t.Error("connectInput leaked one action's own params into another's declaration")
	}
}

// mustFields is the params shorthand the object tests below use.
func mustFields(t *testing.T, m map[string]any) map[string]*structpb.Value {
	t.Helper()
	return mustStruct(t, m).GetFields()
}

// TestEveryActionRefusesAWrongTypedParam is the guard for the sentence this rule
// exists to make true: it holds on EVERY object, not only where the author of the
// day happened to be working.
//
// `tls` is declared Bool on all fifteen actions of all eight objects — it is the
// one parameter the shared connection block gives every one of them — so a string
// there exercises the same refusal fifteen times, including on the three PILOT
// objects (`command`, `instance`, `user`) that predate the check. Their behaviour
// is otherwise untouched by NIM-805; this is the one thing they gained, and it is
// the thing that would otherwise be true only of the new five.
//
// It asserts the REFUSAL, and that nothing reached mongod. Asserting instead that
// the password is absent from the command arguments would pass on the broken
// build too: the password was never in argv, it was in the connection.
func TestEveryActionRefusesAWrongTypedParam(t *testing.T) {
	for objName, obj := range objects(&MongoModule{}) {
		for _, state := range obj.states() {
			t.Run(objName+"."+state, func(t *testing.T) {
				conn := &fakeConn{}
				connects := 0
				m := &MongoModule{connect: func(_ context.Context, cfg connConfig) (mongoConn, error) {
					connects++
					conn.cfg = cfg
					return conn, nil
				}}

				stream := &applyStream{}
				_ = objects(m)[objName].Apply(&pluginv1.ApplyRequest{
					State: state,
					Params: mustStruct(t, map[string]any{
						"addr":     "127.0.0.1:27017",
						"username": "default_admin",
						"password": secretPass,
						"tls":      "true", // a STRING where a boolean is declared
					}),
				}, stream)

				msg := failureMessage(t, stream)
				if !strings.Contains(msg, "params.tls") || !strings.Contains(msg, "boolean") {
					t.Errorf("the refusal must name the param and the expected type, got %q", msg)
				}
				if connects != 0 {
					t.Errorf("a refused step opened %d connection(s) — the plaintext leak is exactly this", connects)
				}
				if len(conn.calls) != 0 || conn.pinged {
					t.Errorf("a refused step reached mongod: pinged=%v calls=%v", conn.pinged, conn.calls)
				}
				assertEventsNoSecret(t, stream)
			})
		}
	}
}
