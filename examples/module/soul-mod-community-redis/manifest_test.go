// Guard on the schema document ↔ implementation contract (NIM-206).
//
// `modules[redis].states.<state>.input` is the ONLY thing param-level strictness
// reads (ADR-0076, NIM-163): a key a state omits has no declaration, so since
// NIM-204 enforced strictness for plugins too (ADR-0076(t)) a legitimate call
// carrying it FAILS with module.unknown_param. A prose promise in a comment is
// not a declaration — four states used to carry that promise and declare nothing,
// which is what this file exists to prevent from coming back.
//
// Since NIM-377 the declaration lives in the generated schema document
// (`schema.json`) rather than a hand-written `manifest.yaml`, and it carries no
// `namespace:`/`name:` of its own: this artifact serves the module `redis`, and
// address level 1 (`community` in `community.redis.acl`) comes from the alias an
// operator registers it under, which appears nowhere in these bytes.
//
// The two halves are checked together on purpose. TestConnectParams* proves the
// key lists below are the ones the Go parse path actually reads (a rename in
// tls.go/helpers.go breaks it); TestManifestStatesDeclareWhatTheyAccept proves
// every state declares exactly those plus its own params. Split across two
// packages the pair would only agree by convention.
package main

import (
	"os"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
	"google.golang.org/protobuf/types/known/structpb"
)

// moduleName is the one module this artifact serves — address level 2.
const moduleName = "redis"

// connectParams — read by parseConnConfig (which calls parseTLS) for EVERY state
// dispatched through the shared connect path in Apply, i.e. all but cluster.
var connectParams = []string{
	"addr", "username", "password", "db",
	"tls", "tls_ca", "tls_cert", "tls_key", "tls_skip_verify",
}

// clusterConnectParams — cluster has no single instance: it builds a connConfig
// per node from its nodes-map, so it reads the same auth+TLS keys but neither
// addr nor db.
var clusterConnectParams = []string{
	"username", "password",
	"tls", "tls_ca", "tls_cert", "tls_key", "tls_skip_verify",
}

// sourceConnectParams — offset-synced opens a SECOND connection to the external
// master with credentials and a PKI of its own (parseSourceTLS).
var sourceConnectParams = []string{
	"source_addr", "source_password",
	"source_tls", "source_tls_ca", "source_tls_cert", "source_tls_key", "source_tls_skip_verify",
}

// secretParams — params carrying a password or PEM. Declaring one without
// `secret: true` would leave it unmasked in logs/traces/UI (ADR-010).
var secretParams = map[string]bool{
	"password": true, "source_password": true, "master_password": true, "auth_pass": true,
	"tls_ca": true, "tls_cert": true, "tls_key": true,
	"source_tls_ca": true, "source_tls_cert": true, "source_tls_key": true,
	"master_tls_ca": true, "master_tls_cert": true, "master_tls_key": true,
}

// loadModule reads the published schema document and returns the module this
// artifact serves. It also runs the SDK validator: a document that keeper would
// reject at `plugin.allow` must not pass here either.
func loadModule(t *testing.T) schema.Module {
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
	mod, ok := doc.Module(moduleName)
	if !ok {
		t.Fatalf("%s declares no module %q, only %v", schema.SchemaFileName, moduleName, doc.ModuleNames())
	}
	if len(mod.States) == 0 {
		t.Fatalf("module %q declares no states", moduleName)
	}
	return mod
}

// with returns base plus extra as a fresh slice — the shared *Params vars must
// never be appended into.
func with(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	return append(out, extra...)
}

// TestManifestStatesDeclareWhatTheyAccept — every state declares EXACTLY the keys
// its implementation reads: the shared connect set (or the cluster variant) plus
// its own. Both directions matter — a missing key is the NIM-206 hole, an extra
// one promises an operator a param nothing reads.
func TestManifestStatesDeclareWhatTheyAccept(t *testing.T) {
	want := map[string][]string{
		"command":        with(connectParams, "args", "changed"),
		"pinged":         with(connectParams),
		"role":           with(connectParams),
		"replica-synced": with(connectParams),
		"config":         with(connectParams, "config", "rewrite"),
		"acl":            with(connectParams),
		"cluster": with(clusterConnectParams,
			"action", "nodes", "replicas_per_shard", "topology",
			"new_node", "seed", "role", "master", "node",
			"from", "to", "slots", "source_nodes", "shards_dest"),
		// master_tls_ca/cert/key are declared but NOT read by the plugin: Redis
		// reads the replication link's PEMs from DISK by path, so render places
		// them and the plugin only flips tls-replication.
		//
		// Recorded as a decision, not a leftover (NIM-229). Expressing
		// "consumed by render" in the manifest is not implementable: parsing is
		// yaml.Strict() and discovery skips a slot on a decode error, so a new
		// key would make an older Soul lose the whole module rather than read
		// the annotation - the same wall NIM-204 hit looking for an opt-in
		// flag. Moving them out of the module's params would fail every
		// scenario that passes them, since NIM-204 made a plugin manifest gate
		// its input. So they stay declared, masked, and documented as
		// render-consumed in docs/module/community/redis/README.md. This entry
		// is the roster that keeps the exception from spreading in silence.
		"replica": with(connectParams,
			"master_addr", "source_external", "master_password", "master_username",
			"master_tls", "master_tls_ca", "master_tls_cert", "master_tls_key"),
		"offset-synced": with(connectParams,
			with(sourceConnectParams, "lag_threshold", "skip_checksum")...),
		"detached": with(connectParams),
		"sentinel": with(connectParams,
			"master_name", "monitor", "config", "auth_user", "auth_pass", "redis_version"),
	}

	mod := loadModule(t)
	if len(mod.States) != len(want) {
		t.Fatalf("module %q declares %d states, table covers %d — a new state needs a row here",
			moduleName, len(mod.States), len(want))
	}

	for state, wantKeys := range want {
		t.Run(state, func(t *testing.T) {
			def, ok := mod.States[state]
			if !ok {
				t.Fatalf("module %q has no state %q", moduleName, state)
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
}

// TestManifestSecretParamsAreMasked — a password/PEM param must be declared
// secret with the vault-ref pattern, or it reaches logs/traces/UI in the clear.
func TestManifestSecretParamsAreMasked(t *testing.T) {
	mod := loadModule(t)
	for state, def := range mod.States {
		for name, p := range def.Input {
			if !secretParams[name] {
				continue
			}
			if !p.Secret {
				t.Errorf("%s.%s: carries a password/PEM but is not declared secret", state, name)
			}
			if p.Pattern != "^vault:.*" {
				t.Errorf("%s.%s: secret param must pin pattern ^vault:.* , got %q", state, name, p.Pattern)
			}
		}
	}
}

// TestConnectParamsAreRead — connectParams is what parseConnConfig actually
// reads, not a list that drifted from it. Each key is fed alone and must land in
// the resulting connConfig; a rename in helpers.go/tls.go fails here rather than
// silently making the manifest table above assert the wrong set.
func TestConnectParamsAreRead(t *testing.T) {
	cases := []struct {
		key   string
		value any
		got   func(connConfig) any
		want  any
	}{
		{"addr", "127.0.0.1:6379", func(c connConfig) any { return c.addr }, "127.0.0.1:6379"},
		{"username", "acl-user", func(c connConfig) any { return c.username }, "acl-user"},
		{"password", "s3cret", func(c connConfig) any { return c.password }, "s3cret"},
		{"db", 3, func(c connConfig) any { return c.db }, 3},
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
			fields := map[string]any{"addr": "127.0.0.1:6379"}
			fields[tc.key] = tc.value
			s, err := structpb.NewStruct(fields)
			if err != nil {
				t.Fatalf("build params: %v", err)
			}
			// "command" — a state that addresses a keyspace, so every key of
			// connectParams including `db` is read (NIM-229: on `sentinel` the
			// keyspace is dropped on purpose, covered in db_scope_test.go).
			cfg, err := parseConnConfig("command", s)
			if err != nil {
				t.Fatalf("parseConnConfig: %v", err)
			}
			if got := tc.got(cfg); got != tc.want {
				t.Errorf("param %q not read by parseConnConfig: got %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// TestSourceConnectTLSParamsAreRead — the source_tls* half of
// sourceConnectParams reaches parseSourceTLS. source_addr/source_password are
// read inline by applyOffsetSynced and covered by offset_synced_test.go.
func TestSourceConnectTLSParamsAreRead(t *testing.T) {
	cases := []struct {
		key   string
		value any
		got   func(tlsParams) any
		want  any
	}{
		{"source_tls", true, func(p tlsParams) any { return p.enabled }, true},
		{"source_tls_ca", "CA-PEM", func(p tlsParams) any { return p.caPEM }, "CA-PEM"},
		{"source_tls_cert", "CERT-PEM", func(p tlsParams) any { return p.certPEM }, "CERT-PEM"},
		{"source_tls_key", "KEY-PEM", func(p tlsParams) any { return p.keyPEM }, "KEY-PEM"},
		{"source_tls_skip_verify", true, func(p tlsParams) any { return p.skipVerify }, true},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			s, err := structpb.NewStruct(map[string]any{tc.key: tc.value})
			if err != nil {
				t.Fatalf("build params: %v", err)
			}
			if got := tc.got(parseSourceTLS(s.GetFields())); got != tc.want {
				t.Errorf("param %q not read by parseSourceTLS: got %v, want %v", tc.key, got, tc.want)
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
