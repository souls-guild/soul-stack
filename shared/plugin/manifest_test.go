package plugin

import (
	"sort"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestLoadFromBytes_SoulModuleHappyPath(t *testing.T) {
	raw := []byte(`kind: soul_module
protocol_version: 1
namespace: acme
name: redis-failover
required_capabilities:
  - network_outbound
  - vault_access
side_effects:
  - { service: redis-server }
  - { file: /etc/redis/sentinel.conf }
spec:
  states:
    promoted:
      description: Promote Redis replica to master.
      input:
        new_master_sid:
          type: string
          required: true
        password:
          type: string
          required: true
          secret: true
          pattern: "^vault:.*"
`)
	m, diags := LoadFromBytes("test.yaml", raw)
	if m == nil {
		t.Fatal("Manifest nil")
	}
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diagCodes(diags))
	}
	if m.Kind != KindSoulModule {
		t.Errorf("Kind = %q, want soul_module", m.Kind)
	}
	if m.ProtoKind() != pluginv1.Kind_KIND_SOUL_MODULE {
		t.Errorf("ProtoKind mismatch")
	}
	if m.Address() != "acme.redis-failover" {
		t.Errorf("Address = %q", m.Address())
	}
	if _, ok := m.Spec.States["promoted"]; !ok {
		t.Errorf("Spec.States missing 'promoted'")
	}
	if len(m.SideEffects) != 2 {
		t.Errorf("SideEffects len = %d", len(m.SideEffects))
	}
}

func TestLoadFromBytes_CloudDriverHappyPath(t *testing.T) {
	raw := []byte(`kind: cloud_driver
protocol_version: 1
namespace: cloud
name: aws
required_capabilities:
  - network_outbound
  - vault_access
side_effects: []
spec:
  provider_kind: aws
  profile_schema:
    type: object
    required: [region]
    properties:
      region: { type: string }
`)
	m, diags := LoadFromBytes("test.yaml", raw)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diagCodes(diags))
	}
	if m.ProtoKind() != pluginv1.Kind_KIND_CLOUD_DRIVER {
		t.Errorf("ProtoKind mismatch")
	}
	if m.Spec.ProfileSchema["type"] != "object" {
		t.Errorf("profile_schema.type = %v", m.Spec.ProfileSchema["type"])
	}
}

func TestLoadFromBytes_SSHProviderHappyPath(t *testing.T) {
	raw := []byte(`kind: ssh_provider
protocol_version: 1
namespace: ssh
name: vault-ssh
required_capabilities: [network_outbound, vault_access]
side_effects: []
spec:
  provider_kind: vault_ssh_ca
  params_schema:
    type: object
`)
	_, diags := LoadFromBytes("test.yaml", raw)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diagCodes(diags))
	}
}

// TestLoadFromBytes_SoulBeaconHappyPath — kind=soul_beacon (ADR-030 V5-2): valid
// with an arbitrary JSON Schema in spec.params_schema, without states /
// provider_kind / profile_schema (they are not allowed for this kind).
func TestLoadFromBytes_SoulBeaconHappyPath(t *testing.T) {
	raw := []byte(`kind: soul_beacon
protocol_version: 1
namespace: community
name: zfs-degraded
required_capabilities: [exec_subprocess]
side_effects: []
spec:
  params_schema:
    type: object
    required: [pool]
    properties:
      pool: { type: string }
`)
	m, diags := LoadFromBytes("test.yaml", raw)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diagCodes(diags))
	}
	if m.Kind != KindSoulBeacon {
		t.Errorf("Kind = %q, want soul_beacon", m.Kind)
	}
	if m.ProtoKind() != pluginv1.Kind_KIND_SOUL_BEACON {
		t.Errorf("ProtoKind mismatch")
	}
	if m.BinaryName() != "soul-beacon-zfs-degraded" {
		t.Errorf("BinaryName = %q", m.BinaryName())
	}
}

// TestLoadFromBytes_SoulBeaconRejectsStates — kind=soul_beacon with a states block
// must return a spec_states_not_allowed error.
func TestLoadFromBytes_SoulBeaconRejectsStates(t *testing.T) {
	raw := []byte(`kind: soul_beacon
protocol_version: 1
namespace: community
name: zfs-degraded
side_effects: []
spec:
  states:
    observed:
      description: oops
`)
	_, diags := LoadFromBytes("test.yaml", raw)
	if !containsStr(diagCodes(diags), "spec_states_not_allowed") {
		t.Errorf("expected spec_states_not_allowed, got %v", diagCodes(diags))
	}
}

// Table-driven negative cases: each fixture has `expected codes` (a set). We check
// that the code is in the result; we don't claim full set equality (the decoder may
// add related codes like type_mismatch).
func TestLoadFromBytes_FailureCases(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		wantCodes []string
	}{
		{
			name: "unknown_kind",
			yaml: `kind: lambda
protocol_version: 1
namespace: x
name: y
spec: { states: { run: { description: x, input: {} } } }
`,
			wantCodes: []string{"kind_invalid"},
		},
		{
			name: "empty_kind",
			yaml: `protocol_version: 1
namespace: x
name: y
spec: { states: { run: { description: x, input: {} } } }
`,
			wantCodes: []string{"missing_required_field"},
		},
		{
			name: "zero_protocol_version",
			yaml: `kind: soul_module
protocol_version: 0
namespace: x
name: y
spec: { states: { run: { description: x, input: {} } } }
`,
			wantCodes: []string{"protocol_version_invalid"},
		},
		{
			name: "unsupported_protocol_version",
			yaml: `kind: soul_module
protocol_version: 999
namespace: x
name: y
spec: { states: { run: { description: x, input: {} } } }
`,
			wantCodes: []string{"protocol_version_unsupported"},
		},
		{
			name: "bad_namespace_uppercase",
			yaml: `kind: soul_module
protocol_version: 1
namespace: "BAD"
name: y
spec: { states: { run: { description: x, input: {} } } }
`,
			wantCodes: []string{"namespace_invalid_format"},
		},
		{
			name: "bad_name_leading_digit",
			yaml: `kind: soul_module
protocol_version: 1
namespace: x
name: "1-bad"
spec: { states: { run: { description: x, input: {} } } }
`,
			wantCodes: []string{"name_invalid_format"},
		},
		{
			name: "empty_states_soul_module",
			yaml: `kind: soul_module
protocol_version: 1
namespace: x
name: y
spec:
  states: {}
`,
			wantCodes: []string{"spec_states_empty"},
		},
		{
			name: "bad_state_name",
			yaml: `kind: soul_module
protocol_version: 1
namespace: x
name: y
spec:
  states:
    "Bad-State":
      description: x
      input: {}
`,
			wantCodes: []string{"state_name_invalid"},
		},
		{
			name: "unknown_capability",
			yaml: `kind: soul_module
protocol_version: 1
namespace: x
name: y
required_capabilities: [magic]
spec:
  states:
    run: { description: x, input: {} }
`,
			wantCodes: []string{"capability_unknown"},
		},
		{
			name: "side_effect_multiple_keys",
			yaml: `kind: soul_module
protocol_version: 1
namespace: x
name: y
side_effects:
  - { service: a, file: /b }
spec:
  states:
    run: { description: x, input: {} }
`,
			wantCodes: []string{"multiple_resource_types_in_side_effect_entry"},
		},
		{
			name: "side_effect_unknown_type",
			yaml: `kind: soul_module
protocol_version: 1
namespace: x
name: y
side_effects:
  - { kafka_topic: foo }
spec:
  states:
    run: { description: x, input: {} }
`,
			wantCodes: []string{"side_effect_type_unknown"},
		},
		{
			name: "input_type_unknown",
			yaml: `kind: soul_module
protocol_version: 1
namespace: x
name: y
spec:
  states:
    run:
      description: x
      input:
        port: { type: float }
`,
			wantCodes: []string{"input_type_unknown"},
		},
		{
			name: "input_type_missing",
			yaml: `kind: soul_module
protocol_version: 1
namespace: x
name: y
spec:
  states:
    run:
      description: x
      input:
        port: { required: true }
`,
			wantCodes: []string{"input_type_missing"},
		},
		{
			name: "secret_without_vault_pattern",
			yaml: `kind: soul_module
protocol_version: 1
namespace: x
name: y
spec:
  states:
    run:
      description: x
      input:
        password: { type: string, secret: true }
`,
			wantCodes: []string{"input_secret_without_vault_pattern"},
		},
		{
			name: "secret_bad_pattern",
			yaml: `kind: soul_module
protocol_version: 1
namespace: x
name: y
spec:
  states:
    run:
      description: x
      input:
        password: { type: string, secret: true, pattern: "^.*$" }
`,
			wantCodes: []string{"input_secret_pattern_invalid"},
		},
		{
			name: "cloud_driver_missing_profile_schema",
			yaml: `kind: cloud_driver
protocol_version: 1
namespace: cloud
name: aws
spec:
  provider_kind: aws
`,
			wantCodes: []string{"spec_profile_schema_missing"},
		},
		{
			name: "ssh_provider_missing_provider_kind",
			yaml: `kind: ssh_provider
protocol_version: 1
namespace: ssh
name: vault-ssh
spec:
  params_schema:
    type: object
`,
			wantCodes: []string{"spec_provider_kind_missing"},
		},
		{
			name: "cloud_driver_with_states_rejected",
			yaml: `kind: cloud_driver
protocol_version: 1
namespace: cloud
name: aws
spec:
  provider_kind: aws
  profile_schema: { type: object }
  states:
    run: { description: x, input: {} }
`,
			wantCodes: []string{"spec_states_not_allowed"},
		},
		{
			name: "unknown_top_level_key",
			yaml: `kind: soul_module
protocol_version: 1
namespace: x
name: y
spec:
  states:
    run: { description: x, input: {} }
mystery_field: 42
`,
			wantCodes: []string{"unknown_key"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, diags := LoadFromBytes("test.yaml", []byte(tc.yaml))
			got := diagCodes(diags)
			for _, want := range tc.wantCodes {
				if !containsStr(got, want) {
					t.Errorf("expected code %q in %v", want, got)
				}
			}
		})
	}
}

func TestCapabilityFromString(t *testing.T) {
	cases := map[string]pluginv1.Capability{
		"run_as_root":      pluginv1.Capability_CAPABILITY_RUN_AS_ROOT,
		"network_outbound": pluginv1.Capability_CAPABILITY_NETWORK_OUTBOUND,
		"network_inbound":  pluginv1.Capability_CAPABILITY_NETWORK_INBOUND,
		"vault_access":     pluginv1.Capability_CAPABILITY_VAULT_ACCESS,
		"fs_write_root":    pluginv1.Capability_CAPABILITY_FS_WRITE_ROOT,
		"exec_subprocess":  pluginv1.Capability_CAPABILITY_EXEC_SUBPROCESS,
	}
	for s, want := range cases {
		got, ok := CapabilityFromString(s)
		if !ok {
			t.Errorf("%s not recognized", s)
			continue
		}
		if got != want {
			t.Errorf("%s → %v, want %v", s, got, want)
		}
	}
	if _, ok := CapabilityFromString("magic"); ok {
		t.Errorf("expected magic to be unknown")
	}
}

func TestLoad_IOError(t *testing.T) {
	_, diags, err := Load("/no/such/file.yaml")
	if err == nil {
		t.Fatal("expected IO error")
	}
	if len(diags) == 0 || diags[0].Code != "io_error" {
		t.Errorf("expected io_error, got %v", diagCodes(diags))
	}
}

func TestLoadFromBytes_BOMStripped(t *testing.T) {
	raw := []byte("\xEF\xBB\xBFkind: soul_module\nprotocol_version: 1\nnamespace: x\nname: y\nspec:\n  states:\n    run:\n      description: x\n      input: {}\n")
	_, diags := LoadFromBytes("bom.yaml", raw)
	if diag.HasErrors(diags) {
		t.Errorf("BOM-prefixed manifest should validate, got %v", diagCodes(diags))
	}
}

func TestLoadFromBytes_ParseError(t *testing.T) {
	raw := []byte("kind: soul_module\n  bad: [unclosed\n")
	_, diags := LoadFromBytes("bad.yaml", raw)
	if !diag.HasErrors(diags) {
		t.Fatal("expected parse error")
	}
	if diags[0].Phase != diag.PhaseParse {
		t.Errorf("expected PhaseParse, got %v", diags[0].Phase)
	}
}

func TestLoadFromBytes_EmptyDocument(t *testing.T) {
	_, diags := LoadFromBytes("empty.yaml", []byte("   \n\n"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected empty_document error")
	}
	if !containsStr(diagCodes(diags), "empty_document") {
		t.Errorf("expected empty_document, got %v", diagCodes(diags))
	}
}

func TestLoadFromBytes_MultiDocumentRejected(t *testing.T) {
	raw := []byte("kind: soul_module\nprotocol_version: 1\nnamespace: x\nname: y\nspec:\n  states:\n    run:\n      description: x\n      input: {}\n---\nkind: soul_module\n")
	_, diags := LoadFromBytes("multi.yaml", raw)
	if !containsStr(diagCodes(diags), "multi_document_not_allowed") {
		t.Errorf("expected multi_document_not_allowed, got %v", diagCodes(diags))
	}
}

func TestValidateSimple_ReturnsFirstError(t *testing.T) {
	m := &Manifest{Kind: "lambda"}
	if err := m.ValidateSimple(); err == nil {
		t.Fatal("expected error for unknown kind")
	}
	good := &Manifest{
		Kind: KindSoulModule, ProtocolVersion: 1,
		Namespace: "x", Name: "y",
		Spec: ManifestSpec{States: map[string]StateDef{
			"run": {Description: "x"},
		}},
	}
	if err := good.ValidateSimple(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// diagCodes — a sorted list of diagnostic codes; for predictable comparison in
// table-driven tests.
func diagCodes(ds []diag.Diagnostic) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Code)
	}
	sort.Strings(out)
	return out
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// TestFormFieldsValid — a manifest with form fields (enum/format:sid/source) is
// valid (ADR-045 S1).
func TestFormFieldsValid(t *testing.T) {
	raw := []byte(`kind: soul_module
protocol_version: 1
namespace: official
name: redis
spec:
  states:
    promoted:
      description: Promote
      input:
        mode:
          type: string
          enum: ["master", "replica"]
        leader:
          type: string
          format: sid
        target:
          type: string
          source:
            incarnation_hosts: true
`)
	_, diags := LoadFromBytes("m.yaml", raw)
	if diag.HasErrors(diags) {
		t.Fatalf("expected no errors, got %v", diagCodes(diags))
	}
}

// TestFormFieldsForwardCompat — an old manifest without form fields stays valid
// (new code on an old manifest; only-add, ADR-020).
func TestFormFieldsForwardCompat(t *testing.T) {
	raw := []byte(`kind: soul_module
protocol_version: 1
namespace: acme
name: redis-failover
spec:
  states:
    promoted:
      description: Promote Redis replica to master.
      input:
        new_master_sid:
          type: string
          required: true
`)
	_, diags := LoadFromBytes("m.yaml", raw)
	if diag.HasErrors(diags) {
		t.Fatalf("legacy manifest must stay valid, got %v", diagCodes(diags))
	}
}

// TestFormFieldsInvalid — structural errors in form fields are caught.
func TestFormFieldsInvalid(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty_enum", "type: string\n          enum: []", "input_enum_empty"},
		{"enum_type_mismatch", "type: int\n          enum: [\"a\"]", "input_enum_type_mismatch"},
		{"format_unknown", "type: string\n          format: zzz", "input_format_invalid"},
		{"source_two_active", "type: string\n          source:\n            incarnation_hosts: true\n            choir: redis", "input_source_invalid"},
		{"source_zero_active", "type: string\n          source:\n            incarnation_hosts: false", "input_source_invalid"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`kind: soul_module
protocol_version: 1
namespace: official
name: redis
spec:
  states:
    promoted:
      description: Promote
      input:
        field:
          ` + tc.body + "\n")
			_, diags := LoadFromBytes("m.yaml", raw)
			if !containsStr(diagCodes(diags), tc.want) {
				t.Fatalf("expected code %q, got %v", tc.want, diagCodes(diags))
			}
		})
	}
}

// Ensure strings import is used (linter satisfaction).
var _ = strings.Contains
