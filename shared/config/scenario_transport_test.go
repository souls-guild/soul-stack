package config

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestTransportSpec_ScalarAndDictDecodeTheSame is the contract the two forms
// exist for: `transport: ssh` and `transport: { ssh: }` are the SAME statement
// written two ways, and a consumer must not be able to tell them apart. A
// decoder that answered differently would make the scalar a distinct transport
// with the same name.
func TestTransportSpec_ScalarAndDictDecodeTheSame(t *testing.T) {
	scalarName, scalarParams, scalarOK := TransportSpecOf("ssh")
	dictName, dictParams, dictOK := TransportSpecOf(map[string]any{"ssh": nil})

	if !scalarOK || !dictOK {
		t.Fatalf("ok: scalar=%v dict=%v, want both true", scalarOK, dictOK)
	}
	if scalarName != dictName {
		t.Errorf("name: scalar=%q dict=%q", scalarName, dictName)
	}
	if len(scalarParams) != 0 || len(dictParams) != 0 {
		t.Errorf("params: scalar=%v dict=%v, want both empty", scalarParams, dictParams)
	}
	if scalarName != TransportSSH {
		t.Errorf("name = %q, want %q", scalarName, TransportSSH)
	}
}

func TestTransportSpecOf(t *testing.T) {
	cases := []struct {
		name       string
		in         any
		wantName   string
		wantParams map[string]any
		wantOK     bool
	}{
		{name: "unset", in: nil},
		{name: "scalar ssh", in: "ssh", wantName: "ssh", wantOK: true},
		{name: "scalar agent", in: "agent", wantName: "agent", wantOK: true},
		{name: "scalar unregistered", in: "rsh"},
		{
			name:       "dict with params",
			in:         map[string]any{"ssh": map[string]any{"ssh_provider": "vault-bastion", "port": uint64(2222)}},
			wantName:   "ssh",
			wantParams: map[string]any{"ssh_provider": "vault-bastion", "port": uint64(2222)},
			wantOK:     true,
		},
		{
			name:       "dict keyed by any (the goccy shape for a nested map)",
			in:         map[any]any{"ssh": map[any]any{"user": "deploy"}},
			wantName:   "ssh",
			wantParams: map[string]any{"user": "deploy"},
			wantOK:     true,
		},
		// Two keys decode to FALSE rather than to one of them: map iteration has
		// no order, and picking would make the run's transport depend on it.
		{name: "two transports", in: map[string]any{"ssh": nil, "agent": nil}},
		{name: "empty dict", in: map[string]any{}},
		{name: "dict of an unregistered transport", in: map[string]any{"rsh": nil}},
		{name: "params not a mapping", in: map[string]any{"ssh": "vault-bastion"}},
		{name: "wrong type entirely", in: []any{"ssh"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, params, ok := TransportSpecOf(tc.in)
			if ok != tc.wantOK || name != tc.wantName {
				t.Fatalf("TransportSpecOf(%v) = (%q, %v, %v), want (%q, _, %v)", tc.in, name, params, ok, tc.wantName, tc.wantOK)
			}
			if tc.wantParams != nil && !reflect.DeepEqual(params, tc.wantParams) {
				t.Errorf("params = %v, want %v", params, tc.wantParams)
			}
		})
	}
}

// TestTransportRegistry_IsClosed pins the enumeration itself. The whole reason
// the value space is closed is that `soul-lint` judges the key OFFLINE — adding
// a transport has to be a deliberate act that also updates
// docs/scenario/orchestration.md and the params table, not a side effect of
// touching a map.
func TestTransportRegistry_IsClosed(t *testing.T) {
	want := []string{TransportAgent, TransportSSH}
	if got := TransportNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("TransportNames() = %v, want %v — a new transport needs its params table and its doc row too", got, want)
	}
	if TransportRegistered("rsh") {
		t.Error("an unregistered name reported as registered")
	}
	// `agent` takes no params; anything under it is an unknown_key. If that
	// changes, the doc row changes with it.
	if len(transportParams[TransportAgent]) != 0 {
		t.Errorf("the agent transport grew params: %v", transportParams[TransportAgent])
	}
}

// TestValidateTransportField walks the key through the validator over real
// YAML, so the diagnostics are the ones an author sees.
func TestValidateTransportField(t *testing.T) {
	cases := []struct {
		name      string
		transport string
		wantCodes []string
	}{
		{name: "scalar", transport: `transport: ssh`},
		{name: "scalar agent", transport: `transport: agent`},
		{name: "dict, no params", transport: "transport:\n      ssh:"},
		{name: "dict with every param", transport: "transport:\n      ssh:\n        ssh_provider: vault-bastion\n        user: deploy\n        port: 2222"},
		{name: "null", transport: `transport:`},

		{name: "unregistered scalar", transport: `transport: rsh`, wantCodes: []string{"transport_unknown"}},
		{name: "unregistered key", transport: "transport:\n      rsh: {}", wantCodes: []string{"transport_unknown"}},
		{name: "two keys", transport: "transport:\n      ssh: {}\n      agent: {}", wantCodes: []string{"transport_multiple"}},
		{name: "three keys", transport: "transport:\n      ssh: {}\n      agent: {}\n      rsh: {}", wantCodes: []string{"transport_multiple", "transport_multiple"}},
		{name: "empty dict", transport: `transport: {}`, wantCodes: []string{"transport_multiple"}},
		{name: "a list", transport: `transport: [ssh]`, wantCodes: []string{"type_mismatch"}},
		{name: "params not a mapping", transport: "transport:\n      ssh: vault-bastion", wantCodes: []string{"type_mismatch"}},
		{name: "unknown param", transport: "transport:\n      ssh:\n        primary_ip: 10.0.0.7", wantCodes: []string{"unknown_key"}},
		{name: "param on agent", transport: "transport:\n      agent:\n        user: deploy", wantCodes: []string{"unknown_key"}},
		{name: "provider format", transport: "transport:\n      ssh:\n        ssh_provider: Vault_Bastion", wantCodes: []string{"name_invalid_format"}},
		{name: "port out of range", transport: "transport:\n      ssh:\n        port: 70000", wantCodes: []string{"value_out_of_range"}},
		{name: "port zero", transport: "transport:\n      ssh:\n        port: 0", wantCodes: []string{"value_out_of_range"}},
		{name: "port as a string", transport: "transport:\n      ssh:\n        port: \"22\"", wantCodes: []string{"type_mismatch"}},
		{name: "user not a string", transport: "transport:\n      ssh:\n        user: 42", wantCodes: []string{"type_mismatch"}},
		{
			name:      "interpolated provider",
			transport: "transport:\n      ssh:\n        ssh_provider: \"${ vars.ssh_provider }\"",
			wantCodes: []string{"transport_interpolation_unsupported"},
		},
		// The NAME, not only a param. `${ vars.t }` is not a typo in a transport
		// name, and transport_unknown would send the author hunting the wrong
		// mistake against "known: agent, ssh".
		{
			name:      "interpolated name, scalar",
			transport: `transport: "${ vars.t }"`,
			wantCodes: []string{"transport_interpolation_unsupported"},
		},
		{
			name:      "interpolated name, dict key",
			transport: "transport:\n      \"${ vars.t }\": {}",
			wantCodes: []string{"transport_interpolation_unsupported"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "name: t\ntasks:\n  - name: task\n    " + tc.transport + "\n    apply:\n      destiny: d\n"
			_, _, diags, err := LoadScenarioManifestFromBytes("scn.yml", []byte(src), ValidateOptions{})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			assertCodes(t, diags, tc.wantCodes)
		})
	}
}

// TestTransportSpec_RoundTripsFromRealYAML closes the gap the hand-built cases
// above cannot: every other test feeds the decoder Go values chosen to match
// what the decoder was written against. This one goes YAML → goccy → Task
// → TransportSpec, so the shape the LOADER actually produces is the shape under
// test. If goccy ever changed it (ordered maps, a different integer type), the
// key would become a silent no-op — valid to the linter, invisible to the
// dispatcher — with every other test in this file still green.
func TestTransportSpec_RoundTripsFromRealYAML(t *testing.T) {
	cases := []struct {
		name       string
		transport  string
		wantName   string
		wantParams map[string]any
	}{
		{name: "scalar", transport: "transport: ssh", wantName: TransportSSH},
		{name: "dict, no params", transport: "transport:\n      ssh:", wantName: TransportSSH},
		{name: "dict, flow style", transport: "transport: { ssh: {} }", wantName: TransportSSH, wantParams: map[string]any{}},
		{
			name:       "dict with every param",
			transport:  "transport:\n      ssh:\n        ssh_provider: vault-bastion\n        user: deploy\n        port: 2222",
			wantName:   TransportSSH,
			wantParams: map[string]any{"ssh_provider": "vault-bastion", "user": "deploy", "port": uint64(2222)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "name: t\ntasks:\n  - name: task\n    " + tc.transport + "\n    apply:\n      destiny: d\n"
			m, _, diags, err := LoadScenarioManifestFromBytes("scn.yml", []byte(src), ValidateOptions{})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			assertCodes(t, diags, nil)
			if m == nil || len(m.Tasks) != 1 {
				t.Fatalf("manifest = %v, want one task", m)
			}
			name, params, ok := m.Tasks[0].TransportSpec()
			if !ok {
				t.Fatalf("TransportSpec() reported no transport for %q — the loader's shape is not one the decoder handles (Transport = %#v)", tc.transport, m.Tasks[0].Transport)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if tc.wantParams != nil && !reflect.DeepEqual(params, tc.wantParams) {
				t.Errorf("params = %#v, want %#v", params, tc.wantParams)
			}
		})
	}
}

// TestValidateTransportOnKeeper — the open question NIM-866 left to the
// implementation, answered as a refusal: a keeper-side task never leaves the
// Keeper, so there is no host at the far end and no transport to name. The
// side is derived from the module address (ADR-0087), so the refusal must fire
// with no `on:` written at all — that is the half a rule keyed on `on: keeper`
// would miss, which is the exact shape of the `when:` hole ADR-0087 records.
func TestValidateTransportOnKeeper(t *testing.T) {
	cases := []struct {
		name    string
		task    string
		wantHit bool
	}{
		{
			name:    "keeper-side module, no on: written",
			task:    "    module: core.soul.registered\n    transport: ssh\n    params:\n      sid: a.example.com",
			wantHit: true,
		},
		{
			name:    "keeper-side module, dict form",
			task:    "    module: core.soul.registered\n    transport:\n      ssh:\n        user: deploy\n    params:\n      sid: a.example.com",
			wantHit: true,
		},
		{
			name:    "Soul-side module keeps the key",
			task:    "    module: core.pkg.installed\n    transport: ssh\n    params:\n      name: redis",
			wantHit: false,
		},
		{
			name:    "keeper-side module without the key",
			task:    "    module: core.soul.registered\n    params:\n      sid: a.example.com",
			wantHit: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "name: t\ntasks:\n  - name: task\n" + tc.task + "\n"
			_, _, diags, err := LoadScenarioManifestFromBytes("scn.yml", []byte(src), ValidateOptions{})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			got := false
			for _, d := range diags {
				if d.Code == "transport_on_keeper_invalid" {
					got = true
				}
			}
			if got != tc.wantHit {
				t.Fatalf("transport_on_keeper_invalid = %v, want %v (diags=%v)", got, tc.wantHit, codesOf(diags))
			}
		})
	}
}

// TestTransportIsAKnownTaskKey guards the wiring rather than the rule: the key
// list is derived from the struct by reflection, so a `transport:` that lost
// its yaml tag would come back as `unknown_key` on every scenario that writes
// it — a refusal with the wrong reason, which is harder to read than none.
func TestTransportIsAKnownTaskKey(t *testing.T) {
	if !taskKnownKeys["transport"] {
		t.Fatal("transport is not in taskKnownKeys — check the yaml tag on Task.Transport")
	}
}

func codesOf(diags []diag.Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		if d.Level == diag.LevelError {
			out = append(out, d.Code)
		}
	}
	return out
}

// assertCodes compares the ERROR-level codes of a load against the expected
// multiset. Exact rather than subset: a rule that fires twice where it should
// fire once is a rule an author reads as two problems.
func assertCodes(t *testing.T, diags []diag.Diagnostic, want []string) {
	t.Helper()
	got := codesOf(diags)
	if len(got) != len(want) {
		t.Fatalf("error codes = %v, want %v", got, want)
	}
	counts := map[string]int{}
	for _, c := range got {
		counts[c]++
	}
	for _, c := range want {
		counts[c]--
	}
	for c, n := range counts {
		if n != 0 {
			t.Fatalf("error codes = %v, want %v (%q off by %d)", got, want, c, n)
		}
	}
}
