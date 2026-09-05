// Guard on the schema document ↔ implementation contract (NIM-206, NIM-525, NIM-769).
//
// `modules[<object>].states.<action>.input` is the ONLY thing param-level strictness
// reads (ADR-0076, NIM-163): a key a state omits has no declaration, so since
// NIM-204 enforced strictness for plugins too (ADR-0076(t)) a legitimate call
// carrying it FAILS with module.unknown_param. A prose promise in a comment is not
// a declaration.
//
// The document is GENERATED from the Go value (`mongoBundle`), not written beside
// it: `soul-mod stamp` runs the artifact's `schema` subcommand and writes those
// bytes both into the binary and to `schema.json`, and
// TestPublishedSchemaMatchesTheBundle below is the local half of that guard.
// The document carries no `namespace:`/`name:` of its own: this artifact serves
// eight objects, and address level 1 (`mongo` in `mongo.instance.pinged`) comes
// from the alias an operator registers it under, which appears nowhere in these
// bytes.
//
// The three halves are checked together on purpose. TestConnectParamsAreRead proves
// the key list below is the one the Go parse path actually reads (a rename in
// tls.go/helpers.go breaks it); TestManifestStatesDeclareWhatTheyAccept proves every
// state declares exactly those plus its own params; and
// TestDeclaredStatesAreDispatched proves the object that SERVES a state is the one
// that DECLARES it. Split across two packages the set would only agree by convention.
package main

import (
	"bytes"
	"os"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
	"google.golang.org/protobuf/types/known/structpb"
)

// connectParams — read by parseConnConfig (which calls parseTLS) for EVERY action
// of this artifact: the two that go through the shared connect path in
// [object.Apply], and the two user actions that open the connection themselves.
var connectParams = []string{
	"addr", "username", "password", "auth_db",
	"tls", "tls_ca", "tls_cert", "tls_key", "tls_skip_verify",
}

// secretParams — params carrying a password or PEM. Declaring one without
// `secret: true` would leave it unmasked in logs/traces/UI (ADR-010).
var secretParams = map[string]bool{
	"password": true, "user_password": true,
	"tls_ca": true, "tls_cert": true, "tls_key": true,
}

// objects — the eight objects this artifact serves, paired with their dispatch
// tables. Address level 2 in `mongo.<object>.<action>`.
func objects(m *MongoModule) map[string]*object {
	return map[string]*object{
		"collection": m.collection(),
		"command":    m.command(),
		"database":   m.database(),
		"index":      m.index(),
		"instance":   m.instance(),
		"replicaset": m.replicaset(),
		"role":       m.role(),
		"user":       m.user(),
	}
}

// loadDocument reads the PUBLISHED schema document and runs the SDK validator: a
// document keeper would reject at `plugin.allow` must not pass here either.
func loadDocument(t *testing.T) schema.Document {
	t.Helper()
	raw, err := os.ReadFile(schema.SchemaFileName)
	if err != nil {
		t.Fatalf("read %s: %v", schema.SchemaFileName, err)
	}
	doc, err := schema.Unmarshal(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", schema.SchemaFileName, err)
	}
	for _, i := range schema.Validate(doc) {
		if i.Level == schema.LevelError {
			t.Errorf("%s is invalid: %s at %s", schema.SchemaFileName, i, i.Path)
		}
	}
	// The bytes are canonical (sorted keys, no insignificant whitespace) because
	// they are hashed and signed — a hand edit that reformats them is a finding.
	canonical, err := schema.IsCanonical(raw)
	if err != nil || !canonical {
		t.Errorf("%s is not canonical (%v) — regenerate it, do not hand-edit", schema.SchemaFileName, err)
	}
	return doc
}

// loadModule returns one object's declaration from the published document.
func loadModule(t *testing.T, name string) schema.Module {
	t.Helper()
	doc := loadDocument(t)
	mod, ok := doc.Module(name)
	if !ok {
		t.Fatalf("%s declares no module %q, only %v", schema.SchemaFileName, name, doc.ModuleNames())
	}
	if len(mod.States) == 0 {
		t.Fatalf("module %q declares no states", name)
	}
	return mod
}

// TestPublishedSchemaMatchesTheBundle — `schema.json` is what `mongoBundle`
// renders, byte for byte. It is GENERATED (`soul-mod stamp`, which runs the
// artifact's own `schema` subcommand), and the moment someone edits a Def without
// re-stamping, everything downstream — soul-lint, `plugin.allow`, the module form —
// is reading a contract the binary no longer implements (NIM-525).
func TestPublishedSchemaMatchesTheBundle(t *testing.T) {
	fromCode, err := mongoBundle(&MongoModule{}).Schema()
	if err != nil {
		t.Fatalf("render the bundle: %v", err)
	}
	published, err := os.ReadFile(schema.SchemaFileName)
	if err != nil {
		t.Fatalf("read %s: %v", schema.SchemaFileName, err)
	}
	if !bytes.Equal(published, fromCode) {
		t.Fatalf("%s disagrees with the bundle — re-run `soul-mod stamp dist/soul-mod-mongo`\n"+
			"  published: %d bytes\n  code:      %d bytes", schema.SchemaFileName, len(published), len(fromCode))
	}
}

// TestBundleIsValid — the same rules keeper applies at approval time, applied at
// build time. Impl included: a Def with no implementation would serve nothing.
func TestBundleIsValid(t *testing.T) {
	for _, i := range mongoBundle(&MongoModule{}).Validate() {
		if i.Level == schema.LevelError {
			t.Errorf("bundle is invalid: %s at %s", i, i.Path)
		}
	}
}

// TestDeclaredStatesAreDispatched — the object that DECLARES a state is the one
// that SERVES it, in both directions. This is the guard the object split needs
// (NIM-769): three objects share one driver, so a state declared on `instance` and
// dispatched only by `user` would lint clean, pass every param check, and fail at
// apply time with "unknown state" on a live host.
func TestDeclaredStatesAreDispatched(t *testing.T) {
	doc := loadDocument(t)
	served := objects(&MongoModule{})

	if len(doc.Modules) != len(served) {
		t.Fatalf("the document declares %d modules, the artifact serves %d (%v)",
			len(doc.Modules), len(served), doc.ModuleNames())
	}

	for _, mod := range doc.Modules {
		obj, ok := served[mod.Name]
		if !ok {
			t.Errorf("module %q is declared but no object serves it", mod.Name)
			continue
		}
		declared := make([]string, 0, len(mod.States))
		for state := range mod.States {
			declared = append(declared, state)
		}
		sort.Strings(declared)

		for _, missing := range diff(declared, obj.states()) {
			t.Errorf("%s.%s is declared but nothing dispatches it", mod.Name, missing)
		}
		for _, extra := range diff(obj.states(), declared) {
			t.Errorf("%s.%s is dispatched but not declared — strictness has no contract for it", mod.Name, extra)
		}
	}
}

// with returns base plus extra as a fresh slice — the shared connectParams var must
// never be appended into.
func with(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	return append(out, extra...)
}

// TestManifestStatesDeclareWhatTheyAccept — every state declares EXACTLY the keys
// its implementation reads: the shared connect set plus its own. Both directions
// matter — a missing key is the NIM-206 hole, an extra one promises an operator a
// param nothing reads.
//
// The two user rows are what the action split bought (NIM-769): `present` and
// `absent` used to share ONE declared input, so `absent` was promised `roles` and
// `user_password` and param strictness had no contract to hold either to. `state`
// appears in neither row on purpose — the verb is the address now.
func TestManifestStatesDeclareWhatTheyAccept(t *testing.T) {
	want := map[string]map[string][]string{
		"command": {
			"run": with(connectParams, "db", "command", "changed"),
		},
		"instance": {
			"pinged": with(connectParams),
		},
		"user": {
			// `name` is who is managed, `username` (in connectParams) is who the
			// step authenticates as — two different people, which is why both are
			// declared. `database` is the login/roles context of the user itself.
			"present": with(connectParams, "name", "database", "roles", "user_password"),
			"absent":  with(connectParams, "name", "database"),
		},
		// The NIM-805 five. Each action declares the addressing keys it reads plus
		// the options it may send, and nothing else: an action that declared an
		// option it does not read would promise an operator a knob attached to
		// nothing, and one that read a key it does not declare would fail the call
		// with module.unknown_param before Apply ever ran.
		"replicaset": {
			"initiated":      with(connectParams, "name", "members", "wait_primary_seconds", "primary_addr"),
			"member-added":   with(connectParams, "member", "wait_primary_seconds", "primary_addr"),
			"member-removed": with(connectParams, "host", "wait_primary_seconds", "primary_addr"),
			"reconfigured":   with(connectParams, "members", "wait_primary_seconds", "primary_addr"),
		},
		"role": {
			"present": with(connectParams, "name", "database", "privileges", "roles"),
			"absent":  with(connectParams, "name", "database"),
		},
		"collection": {
			"present": with(connectParams, "database", "name",
				"capped", "size", "max", "collation", "timeseries", "clustered_index",
				"validator", "validation_level", "validation_action"),
			"absent": with(connectParams, "database", "name"),
		},
		"index": {
			"present": with(connectParams, "database", "collection", "name", "keys",
				"unique", "sparse", "partial_filter_expression", "collation",
				"expire_after_seconds", "hidden"),
			"absent": with(connectParams, "database", "collection", "name"),
		},
		"database": {
			// One action, and the missing `present` is the design: MongoDB has no
			// command that creates a database (database.go).
			"absent": with(connectParams, "name"),
		},
	}

	for objName, states := range want {
		t.Run(objName, func(t *testing.T) {
			mod := loadModule(t, objName)
			if len(mod.States) != len(states) {
				t.Fatalf("module %q declares %d states, table covers %d — a new state needs a row here",
					objName, len(mod.States), len(states))
			}

			for state, wantKeys := range states {
				t.Run(state, func(t *testing.T) {
					def, ok := mod.States[state]
					if !ok {
						t.Fatalf("module %q has no state %q", objName, state)
					}
					got := make([]string, 0, len(def.Input))
					for name := range def.Input {
						got = append(got, name)
					}
					sort.Strings(got)
					sorted := append([]string(nil), wantKeys...)
					sort.Strings(sorted)

					for _, missing := range diff(sorted, got) {
						t.Errorf("param %q is read but NOT declared — strictness has no contract for it (NIM-206)", missing)
					}
					for _, extra := range diff(got, sorted) {
						t.Errorf("param %q is declared but nothing reads it", extra)
					}
				})
			}
		})
	}
}

// TestManifestSecretParamsAreMasked — a password/PEM param must be declared secret
// with the vault-ref pattern, or it reaches logs/traces/UI in the clear.
func TestManifestSecretParamsAreMasked(t *testing.T) {
	for _, mod := range loadDocument(t).Modules {
		for state, def := range mod.States {
			for name, p := range def.Input {
				if !secretParams[name] {
					continue
				}
				if !p.Secret {
					t.Errorf("%s.%s.%s: carries a password/PEM but is not declared secret", mod.Name, state, name)
				}
				if p.Pattern != "^vault:.*" {
					t.Errorf("%s.%s.%s: secret param must pin pattern ^vault:.* , got %q", mod.Name, state, name, p.Pattern)
				}
			}
		}
	}
}

// TestEveryModuleDeclaresItsSide — `side: soul` is declared per object (NIM-749),
// not inherited from a default. The zero value of the field IS soul, so an object
// that forgot it looks identical in behaviour and different in the document an
// operator reads.
func TestEveryModuleDeclaresItsSide(t *testing.T) {
	for _, mod := range loadDocument(t).Modules {
		if mod.Side != schema.SideSoul {
			t.Errorf("module %q declares side %q, want %q", mod.Name, mod.Side, schema.SideSoul)
		}
	}
}

// TestConnectParamsAreRead — connectParams is what parseConnConfig actually reads,
// not a list that drifted from it. Each key is fed alone and must land in the
// resulting connConfig; a rename in helpers.go/tls.go fails here rather than
// silently making the manifest table above assert the wrong set.
func TestConnectParamsAreRead(t *testing.T) {
	cases := []struct {
		key   string
		value any
		got   func(connConfig) any
		want  any
	}{
		{"addr", "127.0.0.1:27017", func(c connConfig) any { return c.addr }, "127.0.0.1:27017"},
		{"username", "default_admin", func(c connConfig) any { return c.username }, "default_admin"},
		{"password", "s3cret", func(c connConfig) any { return c.password }, "s3cret"},
		{"auth_db", "appdb", func(c connConfig) any { return c.authDB }, "appdb"},
		{"tls", true, func(c connConfig) any { return c.tls.enabled }, true},
		{"tls_ca", "CA-PEM", func(c connConfig) any { return c.tls.caPEM }, "CA-PEM"},
		{"tls_cert", "CERT-PEM", func(c connConfig) any { return c.tls.certPEM }, "CERT-PEM"},
		{"tls_key", "KEY-PEM", func(c connConfig) any { return c.tls.keyPEM }, "KEY-PEM"},
		{"tls_skip_verify", true, func(c connConfig) any { return c.tls.skipVerify }, true},
	}
	if len(cases) != len(connectParams) {
		t.Fatalf("connectParams has %d keys, table covers %d", len(connectParams), len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			// addr is required by parseConnConfig, so it is always present.
			fields := map[string]any{"addr": "127.0.0.1:27017"}
			fields[tc.key] = tc.value
			s, err := structpb.NewStruct(fields)
			if err != nil {
				t.Fatalf("build params: %v", err)
			}
			cfg, err := parseConnConfig(s)
			if err != nil {
				t.Fatalf("parseConnConfig: %v", err)
			}
			if got := tc.got(cfg); got != tc.want {
				t.Errorf("param %q not read by parseConnConfig: got %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// diff returns the members of a missing from b; both must be sorted.
func diff(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}
