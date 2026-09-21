package config

import "testing"

// Tests of the roster source (NIM-371): `source: { roster: true }` declares which
// `input:` field carries the souls an incarnation is CREATED on. The key does double
// duty — the SID catalog for the create form, and the keeper's signal to bind that
// value into `incarnation_membership` before the bootstrap run — so both the schema
// shape and the field-resolution helper are load-bearing.

// --- schema shape ---

func TestInputSource_RosterValidOnStringArrayItems(t *testing.T) {
	src := `name: x
input:
  hosts:
    type: array
    required: true
    min_items: 1
    items:
      type: string
      format: sid
      source: { roster: true }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "input_source_invalid") || hasCode(diags, "unknown_key") ||
		hasCode(diags, "input_key_invalid_for_type") {
		dump(t, diags)
		t.Fatalf("source: { roster: true } on items of a string array must be valid")
	}
}

func TestInputSource_RosterValidOnString(t *testing.T) {
	src := `name: x
input:
  host:
    type: string
    format: sid
    source: { roster: true }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "input_source_invalid") || hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("source: { roster: true } on a single string field must be valid")
	}
}

func TestInputSource_RosterMustBeBool(t *testing.T) {
	src := `name: x
input:
  hosts:
    type: string
    source: { roster: "yes" }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf("source.roster must be bool")
	}
}

// A false roster counts as no active catalog, exactly as `incarnation_hosts: false`
// does: `source: {}` is not a way to declare "any value goes".
func TestInputSource_RosterFalseIsNotActive(t *testing.T) {
	src := `name: x
input:
  hosts:
    type: string
    source: { roster: false }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_source_invalid") {
		dump(t, diags)
		t.Fatalf("roster: false leaves 0 active catalogs — must be input_source_invalid")
	}
}

// The discriminator stays exclusive: roster is a THIRD variant, not an addition that
// can be combined with the incarnation-scoped ones.
func TestInputSource_RosterWithIncarnationHosts_TwoActive(t *testing.T) {
	src := `name: x
input:
  hosts:
    type: string
    source: { roster: true, incarnation_hosts: true }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_source_invalid") {
		dump(t, diags)
		t.Fatalf("roster + incarnation_hosts = 2 active catalogs — must be input_source_invalid")
	}
}

// Two roster fields have no defined meaning: the keeper binds ONE value as the
// composition of the incarnation. Rejected at authoring time, not at create time.
func TestInputSource_RosterDuplicate_Rejected(t *testing.T) {
	src := `name: x
input:
  hosts:
    type: array
    items:
      type: string
      format: sid
      source: { roster: true }
  spare_hosts:
    type: array
    items:
      type: string
      format: sid
      source: { roster: true }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_roster_source_duplicate") {
		dump(t, diags)
		t.Fatalf("two fields declaring source.roster must be input_roster_source_duplicate")
	}
}

func TestInputSource_SingleRosterField_NotFlaggedAsDuplicate(t *testing.T) {
	src := `name: x
input:
  hosts:
    type: array
    items:
      type: string
      format: sid
      source: { roster: true }
  version:
    type: string
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "input_roster_source_duplicate") {
		dump(t, diags)
		t.Fatalf("one roster field alongside unrelated fields must not be a duplicate")
	}
}

// --- RosterInputField resolution ---

func TestRosterInputField_FindsItemsForm(t *testing.T) {
	schema := InputSchemaMap{
		"version": {Type: "string"},
		"hosts": {
			Type:  "array",
			Items: &InputSchema{Type: "string", Format: "sid", Source: &InputSource{Roster: true}},
		},
	}
	if got := RosterInputField(schema); got != "hosts" {
		t.Fatalf("RosterInputField = %q, want \"hosts\"", got)
	}
}

func TestRosterInputField_FindsSingleStringForm(t *testing.T) {
	schema := InputSchemaMap{
		"host": {Type: "string", Format: "sid", Source: &InputSource{Roster: true}},
	}
	if got := RosterInputField(schema); got != "host" {
		t.Fatalf("RosterInputField = %q, want \"host\"", got)
	}
}

// A scenario needing no roster must resolve to "" — that is what tells the create
// path to bind nothing, so a false positive here would bind a roster the scenario
// never asked for.
func TestRosterInputField_AbsentWithoutDeclaration(t *testing.T) {
	schema := InputSchemaMap{
		"version": {Type: "string"},
		"peers": {
			Type:  "array",
			Items: &InputSchema{Type: "string", Format: "sid", Source: &InputSource{IncarnationHosts: true}},
		},
		"voices": {Type: "string", Source: &InputSource{Choir: "primary"}},
	}
	if got := RosterInputField(schema); got != "" {
		t.Fatalf("RosterInputField = %q, want empty (no roster declared)", got)
	}
}

// Deterministic pick if a duplicate ever slips past the validator: first by sorted
// name, never map order.
func TestRosterInputField_DuplicateIsDeterministic(t *testing.T) {
	rosterProp := func() *InputSchema {
		return &InputSchema{
			Type:  "array",
			Items: &InputSchema{Type: "string", Format: "sid", Source: &InputSource{Roster: true}},
		}
	}
	schema := InputSchemaMap{"hosts": rosterProp(), "aaa_hosts": rosterProp(), "zzz_hosts": rosterProp()}
	for i := 0; i < 20; i++ {
		if got := RosterInputField(schema); got != "aaa_hosts" {
			t.Fatalf("RosterInputField = %q, want \"aaa_hosts\" (first by sorted name)", got)
		}
	}
}

func TestRosterInputField_NilEntriesIgnored(t *testing.T) {
	schema := InputSchemaMap{"broken": nil}
	if got := RosterInputField(schema); got != "" {
		t.Fatalf("RosterInputField = %q, want empty", got)
	}
}
