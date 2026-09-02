package schema

import (
	"strings"
	"testing"
)

// minimalSoulModule is the smallest document that must validate clean.
func minimalSoulModule() Document {
	return Document{
		Kind:            KindSoulModule,
		ProtocolVersion: 1,
		Modules: []Module{{
			Name: "acl",
			States: map[string]State{
				"present": {Description: "The ACL user exists"},
			},
		}},
	}
}

func codes(issues []Issue) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.Code)
	}
	return out
}

func hasCode(issues []Issue, code string) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

func TestValidate_HappyPaths(t *testing.T) {
	docs := map[string]Document{
		"soul_module": sampleDocument(),
		"cloud_driver": {
			Kind: KindCloudDriver, ProtocolVersion: 1,
			ProfileSchema: map[string]any{"type": "object"},
		},
		"ssh_provider": {
			Kind: KindSSHProvider, ProtocolVersion: 1,
			ProviderKind: "vault_ssh_ca",
			ParamsSchema: map[string]any{"type": "object"},
		},
		"soul_beacon_without_params": {
			Kind: KindSoulBeacon, ProtocolVersion: 1,
		},
		// NIM-747: both declarable sides pass, and so does saying nothing —
		// which is the case every plugin written before the field is in, and the
		// one that must keep meaning "soul".
		"module_declaring_side_soul": func() Document {
			d := minimalSoulModule()
			d.Modules[0].Side = SideSoul
			return d
		}(),
		"module_declaring_side_keeper": func() Document {
			d := minimalSoulModule()
			d.Modules[0].Side = SideKeeper
			return d
		}(),
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			if issues := Validate(doc); HasErrors(issues) {
				t.Fatalf("expected a clean document, got %v", codes(issues))
			}
		})
	}
}

func TestValidate_Failures(t *testing.T) {
	tests := []struct {
		name string
		doc  Document
		want string
	}{
		{
			name: "empty_kind",
			doc:  Document{ProtocolVersion: 1},
			want: "missing_required_field",
		},
		{
			name: "unknown_kind",
			doc:  Document{Kind: "wizardry", ProtocolVersion: 1},
			want: "kind_invalid",
		},
		{
			name: "zero_protocol_version",
			doc:  func() Document { d := minimalSoulModule(); d.ProtocolVersion = 0; return d }(),
			want: "protocol_version_invalid",
		},
		{
			name: "unsupported_protocol_version",
			doc:  func() Document { d := minimalSoulModule(); d.ProtocolVersion = 99; return d }(),
			want: "protocol_version_unsupported",
		},
		{
			name: "no_modules",
			doc:  Document{Kind: KindSoulModule, ProtocolVersion: 1},
			want: "modules_empty",
		},
		{
			name: "module_without_name",
			doc: Document{Kind: KindSoulModule, ProtocolVersion: 1, Modules: []Module{{
				States: map[string]State{"present": {Description: "d"}},
			}}},
			want: "missing_required_field",
		},
		{
			name: "module_name_uppercase",
			doc:  func() Document { d := minimalSoulModule(); d.Modules[0].Name = "ACL"; return d }(),
			want: "module_name_invalid",
		},
		{
			name: "module_name_leading_digit",
			doc:  func() Document { d := minimalSoulModule(); d.Modules[0].Name = "1-bad"; return d }(),
			want: "module_name_invalid",
		},
		{
			name: "module_name_is_the_schema_subcommand",
			doc:  func() Document { d := minimalSoulModule(); d.Modules[0].Name = "schema"; return d }(),
			want: "module_name_reserved",
		},
		{
			name: "duplicate_module_names",
			doc: func() Document {
				d := minimalSoulModule()
				d.Modules = append(d.Modules, d.Modules[0])
				return d
			}(),
			want: "module_name_duplicate",
		},
		{
			name: "module_without_states",
			doc:  Document{Kind: KindSoulModule, ProtocolVersion: 1, Modules: []Module{{Name: "acl"}}},
			want: "module_states_empty",
		},
		{
			name: "bad_state_name",
			doc: func() Document {
				d := minimalSoulModule()
				d.Modules[0].States = map[string]State{"Present": {Description: "d"}}
				return d
			}(),
			want: "state_name_invalid",
		},
		{
			// NIM-747: `side:` is an enum, and the empty string is the declared
			// default (soul) rather than a missing value. Anything else is refused
			// instead of folded into that default — `side: Keeper` quietly meaning
			// "runs on every host" is the failure the check exists for.
			name: "module_side_invalid",
			doc: func() Document {
				d := minimalSoulModule()
				d.Modules[0].Side = "Keeper"
				return d
			}(),
			want: "module_side_invalid",
		},
		{
			name: "unknown_capability",
			doc: func() Document {
				d := minimalSoulModule()
				d.Modules[0].Capabilities = []Capability{"become_root"}
				return d
			}(),
			want: "capability_unknown",
		},
		{
			name: "side_effect_with_no_resource",
			doc: func() Document {
				d := minimalSoulModule()
				d.Modules[0].SideEffects = []SideEffect{{}}
				return d
			}(),
			want: "side_effect_empty_entry",
		},
		{
			name: "side_effect_with_two_resources",
			doc: func() Document {
				d := minimalSoulModule()
				d.Modules[0].SideEffects = []SideEffect{{User: "redis", Service: "redis"}}
				return d
			}(),
			want: "multiple_resource_types_in_side_effect_entry",
		},
		{
			name: "input_type_missing",
			doc:  withInput(Param{Required: true}),
			want: "input_type_missing",
		},
		{
			name: "input_type_unknown",
			doc:  withInput(Param{Type: "duration"}),
			want: "input_type_unknown",
		},
		{
			name: "secret_without_pattern",
			doc:  withInput(Param{Type: String, Secret: true}),
			want: "input_secret_without_vault_pattern",
		},
		{
			name: "secret_with_wrong_pattern",
			doc:  withInput(Param{Type: String, Secret: true, Pattern: "^.*$"}),
			want: "input_secret_pattern_invalid",
		},
		{
			name: "empty_enum",
			doc:  withInput(Param{Type: String, Enum: []any{}}),
			want: "input_enum_empty",
		},
		{
			name: "enum_type_mismatch",
			doc:  withInput(Param{Type: Int, Enum: []any{"one"}}),
			want: "input_enum_type_mismatch",
		},
		{
			name: "unknown_format",
			doc:  withInput(Param{Type: String, Format: "hostname-ish"}),
			want: "input_format_invalid",
		},
		{
			name: "items_on_a_scalar",
			doc:  withInput(Param{Type: String, Items: &Param{Type: String}}),
			want: "input_items_invalid_for_type",
		},
		{
			name: "items_without_type",
			doc:  withInput(Param{Type: List, Items: &Param{}}),
			want: "input_items_type_missing",
		},
		{
			name: "items_with_unknown_type",
			doc:  withInput(Param{Type: Map, Items: &Param{Type: "duration"}}),
			want: "input_items_type_unknown",
		},
		{
			name: "source_with_no_catalog",
			doc:  withInput(Param{Type: String, Source: &InputSource{}}),
			want: "input_source_invalid",
		},
		{
			name: "source_with_two_catalogs",
			doc:  withInput(Param{Type: String, Source: &InputSource{IncarnationHosts: true, Choir: "masters"}}),
			want: "input_source_invalid",
		},
		{
			name: "bad_introduced_in",
			doc:  withInput(Param{Type: String, IntroducedIn: "v0.3"}),
			want: "introduced_in_invalid",
		},
		{
			name: "cloud_driver_without_profile_schema",
			doc:  Document{Kind: KindCloudDriver, ProtocolVersion: 1},
			want: "profile_schema_missing",
		},
		{
			name: "ssh_provider_without_provider_kind",
			doc:  Document{Kind: KindSSHProvider, ProtocolVersion: 1},
			want: "provider_kind_missing",
		},
		{
			name: "cloud_driver_with_modules",
			doc: Document{Kind: KindCloudDriver, ProtocolVersion: 1,
				ProfileSchema: map[string]any{"type": "object"},
				Modules:       minimalSoulModule().Modules},
			want: "modules_not_allowed",
		},
		{
			name: "beacon_with_profile_schema",
			doc: Document{Kind: KindSoulBeacon, ProtocolVersion: 1,
				ProfileSchema: map[string]any{"type": "object"}},
			want: "profile_schema_not_allowed",
		},
		{
			name: "soul_module_with_provider_kind",
			doc:  func() Document { d := minimalSoulModule(); d.ProviderKind = "static_key"; return d }(),
			want: "provider_kind_not_allowed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issues := Validate(tc.doc)
			if !hasCode(issues, tc.want) {
				t.Fatalf("want code %q, got %v", tc.want, codes(issues))
			}
			if !HasErrors(issues) {
				t.Fatalf("code %q was reported but not at error level", tc.want)
			}
		})
	}
}

// withInput builds a minimal module whose single state declares one parameter.
func withInput(p Param) Document {
	d := minimalSoulModule()
	d.Modules[0].States = map[string]State{
		"present": {Description: "d", Input: Input{"host": p}},
	}
	return d
}

func TestValidate_OutputIsCheckedLikeInput(t *testing.T) {
	d := minimalSoulModule()
	d.Modules[0].States = map[string]State{
		"present": {Description: "d", Output: Output{"users": {Type: "duration"}}},
	}
	issues := Validate(d)
	if !hasCode(issues, "input_type_unknown") {
		t.Fatalf("an output field with a bogus type must be rejected, got %v", codes(issues))
	}
	if !strings.Contains(issueWithCode(t, issues, "input_type_unknown").Path, ".output.users") {
		t.Fatal("the issue must point at the output block")
	}
}

func TestValidate_DeprecationWindow(t *testing.T) {
	tests := map[string]struct {
		dep  *Deprecated
		want string
	}{
		"missing_since":     {&Deprecated{RemovedIn: "0.6.0"}, "deprecated_bound_missing"},
		"missing_removed":   {&Deprecated{Since: "0.4.0"}, "deprecated_bound_missing"},
		"bad_grammar":       {&Deprecated{Since: "v0.4.0", RemovedIn: "0.6.0"}, "deprecated_version_invalid"},
		"window_too_short":  {&Deprecated{Since: "0.4.0", RemovedIn: "0.5.0"}, "deprecation_window_too_short"},
		"unknown_successor": {&Deprecated{Since: "0.4.0", RemovedIn: "0.6.0", Use: "nowhere"}, "deprecated_replacement_unknown"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			issues := Validate(withInput(Param{Type: String, Deprecated: tc.dep}))
			if !hasCode(issues, tc.want) {
				t.Fatalf("want %q, got %v", tc.want, codes(issues))
			}
		})
	}

	// A window of exactly the policy minimum, and a major bump, are both fine.
	for _, ok := range []*Deprecated{
		{Since: "0.4.0", RemovedIn: "0.6.0"},
		{Since: "0.4.0", RemovedIn: "1.0.0"},
	} {
		if issues := Validate(withInput(Param{Type: String, Deprecated: ok})); HasErrors(issues) {
			t.Fatalf("deprecation %+v should be accepted, got %v", ok, codes(issues))
		}
	}
}

func TestValidate_DeprecationSuccessorInSameState(t *testing.T) {
	d := minimalSoulModule()
	d.Modules[0].States = map[string]State{
		"present": {Description: "d", Input: Input{
			"old": {Type: String, Deprecated: &Deprecated{Since: "0.4.0", RemovedIn: "0.6.0", Use: "new"}},
			"new": {Type: String},
		}},
	}
	if issues := Validate(d); HasErrors(issues) {
		t.Fatalf("a successor declared in the same state must be accepted, got %v", codes(issues))
	}
}

func TestValidate_MissingStateDescriptionIsAWarning(t *testing.T) {
	d := minimalSoulModule()
	d.Modules[0].States = map[string]State{"present": {}}
	issues := Validate(d)
	if HasErrors(issues) {
		t.Fatalf("a missing description must not block, got %v", codes(issues))
	}
	if !hasCode(issues, "state_description_missing") {
		t.Fatalf("expected a warning about the description, got %v", codes(issues))
	}
}

func TestValidate_IssueOrderIsStable(t *testing.T) {
	d := minimalSoulModule()
	d.Modules[0].States = map[string]State{
		"present": {Description: "d", Input: Input{
			"zeta": {Type: "bogus"}, "alpha": {Type: "bogus"}, "mid": {Type: "bogus"},
		}},
	}
	first := paths(Validate(d))
	for range 50 {
		if got := paths(Validate(d)); !equalStrings(first, got) {
			t.Fatalf("issue order is not stable:\nfirst: %v\ngot:   %v", first, got)
		}
	}
}

func TestDeprecated_Notice(t *testing.T) {
	d := &Deprecated{Since: "0.4.0", RemovedIn: "0.6.0", Use: "host"}
	got := d.Notice("hostname")
	for _, want := range []string{`"hostname"`, "0.4.0", "0.6.0", `"host"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("notice %q is missing %q", got, want)
		}
	}
	if (*Deprecated)(nil).Notice("x") != "" {
		t.Fatal("a nil deprecation must render as an empty notice")
	}
}

func TestSideEffect_Resource(t *testing.T) {
	if rt, v, ok := (SideEffect{User: "redis_acl_user"}).Resource(); !ok || rt != "user" || v != "redis_acl_user" {
		t.Fatalf("Resource() = %q,%q,%v", rt, v, ok)
	}
	if _, _, ok := (SideEffect{}).Resource(); ok {
		t.Fatal("an empty side effect must not report a resource")
	}
	if _, _, ok := (SideEffect{User: "a", Port: "6379"}).Resource(); ok {
		t.Fatal("a two-resource side effect must not report a resource")
	}
}

func issueWithCode(t *testing.T, issues []Issue, code string) Issue {
	t.Helper()
	for _, i := range issues {
		if i.Code == code {
			return i
		}
	}
	t.Fatalf("no issue with code %q in %v", code, codes(issues))
	return Issue{}
}

func paths(issues []Issue) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.Path)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// [ADR-0083] §8 makes `secret:` on an OUTPUT field load-bearing. Its §1 puts
// `type: secret` inside `items:` for a state_schema field, which is a plausible
// shape to carry over to a manifest — and one nothing reads. Accepting it silently
// would reproduce the exact failure the retired `no_log:` was retired for.
func TestValidate_ItemsSecretIsRejected(t *testing.T) {
	d := minimalSoulModule()
	d.Modules[0].States = map[string]State{
		"present": {Description: "d", Output: Output{"users": {Type: List, Items: &Param{Type: Map, Secret: true}}}},
	}
	issues := Validate(d)
	if !hasCode(issues, "items_secret_not_supported") {
		t.Fatalf("a marking nothing reads must not pass in silence, got %v", codes(issues))
	}
	got := issueWithCode(t, issues, "items_secret_not_supported")
	if got.Level != LevelError {
		t.Errorf("level = %v, want an error: a warning still ships an unmasked secret", got.Level)
	}
	if !strings.Contains(got.Hint, "output.users.secret") {
		t.Errorf("hint = %q, want it to name the containing field to mark instead", got.Hint)
	}
}
