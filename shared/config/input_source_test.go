package config

import "testing"

// Tests of typed input ADR-044 S-T1: format:sid + source:.
// Schema validation (source / format shape) — via LoadDestinyManifestFromBytes;
// value validation (FQDN value format, min_items/max_items) — via
// ResolveInputValues + schemaFromInput.

// --- format: sid (schema) ---

func TestInputSchema_FormatSID_Valid(t *testing.T) {
	src := `name: x
input:
  host:
    type: string
    format: sid
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "input_format_invalid") {
		dump(t, diags)
		t.Fatalf("format: sid must be a known format")
	}
}

// --- source: applicability + structure (schema) ---

func TestInputSource_ValidOnString(t *testing.T) {
	src := `name: x
input:
  host:
    type: string
    source: { incarnation_hosts: true }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "input_key_invalid_for_type") || hasCode(diags, "input_source_invalid") {
		dump(t, diags)
		t.Fatalf("source on type=string with incarnation_hosts must be valid")
	}
}

func TestInputSource_ValidOnStringArray(t *testing.T) {
	src := `name: x
input:
  hosts:
    type: array
    items:
      type: string
      format: sid
    source: { choir: redis_primary }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "input_key_invalid_for_type") ||
		hasCode(diags, "input_source_invalid") ||
		hasCode(diags, "input_source_invalid_for_type") {
		dump(t, diags)
		t.Fatalf("source on type=array(items.type=string) must be valid")
	}
}

func TestInputSource_InvalidOnInteger(t *testing.T) {
	src := `name: x
input:
  n:
    type: integer
    source: { incarnation_hosts: true }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_key_invalid_for_type") {
		dump(t, diags)
		t.Fatalf("source must be invalid on type=integer")
	}
}

func TestInputSource_InvalidOnBoolean(t *testing.T) {
	src := `name: x
input:
  b:
    type: boolean
    source: { incarnation_hosts: true }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_key_invalid_for_type") {
		dump(t, diags)
		t.Fatalf("source must be invalid on type=boolean")
	}
}

func TestInputSource_InvalidWhenScalar(t *testing.T) {
	// source as a scalar (not a mapping) is structurally invalid. goccy catches the
	// type mismatch on decode (type_mismatch) before validateSource; we accept
	// either of the two codes as a signal "source shape rejected".
	src := `name: x
input:
  host:
    type: string
    source: incarnation_hosts
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_source_invalid") && !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf("source as scalar must be rejected (input_source_invalid or type_mismatch)")
	}
}

func TestInputSource_UnknownSubKey(t *testing.T) {
	src := `name: x
input:
  host:
    type: string
    source: { bogus_catalog: true }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("unknown source sub-key must raise unknown_key")
	}
}

func TestInputSource_IncarnationHostsWrongType(t *testing.T) {
	src := `name: x
input:
  host:
    type: string
    source: { incarnation_hosts: "yes" }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf("source.incarnation_hosts must be bool")
	}
}

func TestInputSource_ArrayRequiresStringItems(t *testing.T) {
	src := `name: x
input:
  hosts:
    type: array
    items:
      type: integer
    source: { incarnation_hosts: true }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_source_invalid_for_type") {
		dump(t, diags)
		t.Fatalf("source on type=array requires items.type=string")
	}
}

// --- invariant "exactly one active source" (schema) ---

func TestInputSource_EmptyObject(t *testing.T) {
	// source: {} — 0 active sources → error.
	src := `name: x
input:
  host:
    type: string
    source: {}
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_source_invalid") {
		dump(t, diags)
		t.Fatalf("empty source must raise input_source_invalid (0 active catalogs)")
	}
}

func TestInputSource_TwoActive(t *testing.T) {
	// Two specified sources → error (the discriminator allows exactly one).
	src := `name: x
input:
  host:
    type: string
    source: { incarnation_hosts: true, choir: redis_primary }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_source_invalid") {
		dump(t, diags)
		t.Fatalf("two active source catalogs must raise input_source_invalid")
	}
}

func TestInputSource_IncarnationHostsFalse(t *testing.T) {
	// incarnation_hosts: false → source disabled → 0 active → error.
	src := `name: x
input:
  host:
    type: string
    source: { incarnation_hosts: false }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_source_invalid") {
		dump(t, diags)
		t.Fatalf("incarnation_hosts: false must raise input_source_invalid (0 active catalogs)")
	}
}

func TestInputSource_ChoirEmptyString(t *testing.T) {
	// choir: "" — an empty string is invalid (and inactive) → input_source_invalid.
	src := `name: x
input:
  host:
    type: string
    source: { choir: "" }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_source_invalid") {
		dump(t, diags)
		t.Fatalf("empty choir must raise input_source_invalid")
	}
}

func TestInputSource_ExactlyOne_IncarnationHosts(t *testing.T) {
	// Exactly one active (incarnation_hosts: true) → ok.
	src := `name: x
input:
  host:
    type: string
    source: { incarnation_hosts: true }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "input_source_invalid") {
		dump(t, diags)
		t.Fatalf("exactly one active catalog (incarnation_hosts: true) must be valid")
	}
}

func TestInputSource_ExactlyOne_Choir(t *testing.T) {
	// Exactly one active (choir: <name>) → ok.
	src := `name: x
input:
  host:
    type: string
    source: { choir: redis-primary }
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "input_source_invalid") {
		dump(t, diags)
		t.Fatalf("exactly one active catalog (choir) must be valid")
	}
}

// --- format: sid value validation ---

func TestInputValue_FormatSID_OK(t *testing.T) {
	schema := schemaFromInput(t, `host:
  type: string
  format: sid
`)
	if _, err := ResolveInputValues(schema, map[string]any{"host": "web-01.example.com"}); err != nil {
		t.Fatalf("valid SID must pass: %v", err)
	}
}

func TestInputValue_FormatSID_Garbage(t *testing.T) {
	schema := schemaFromInput(t, `host:
  type: string
  format: sid
`)
	if _, err := ResolveInputValues(schema, map[string]any{"host": "WEB 01!"}); err == nil {
		t.Fatalf("garbage SID must fail validation")
	}
}

func TestInputValue_FormatSID_ArrayElements(t *testing.T) {
	schema := schemaFromInput(t, `hosts:
  type: array
  items:
    type: string
    format: sid
`)
	if _, err := ResolveInputValues(schema, map[string]any{"hosts": []any{"a.example.com", "BAD HOST"}}); err == nil {
		t.Fatalf("bad SID element must fail validation")
	}
	if _, err := ResolveInputValues(schema, map[string]any{"hosts": []any{"a.example.com", "b.example.com"}}); err != nil {
		t.Fatalf("valid SID elements must pass: %v", err)
	}
}

// --- min_items/max_items on a sid-list ---

func TestInputValue_SIDList_MinMaxItems(t *testing.T) {
	schema := schemaFromInput(t, `hosts:
  type: array
  min_items: 1
  max_items: 2
  items:
    type: string
    format: sid
`)
	if _, err := ResolveInputValues(schema, map[string]any{"hosts": []any{}}); err == nil {
		t.Fatalf("empty list must violate min_items: 1")
	}
	if _, err := ResolveInputValues(schema, map[string]any{"hosts": []any{"a.com", "b.com", "c.com"}}); err == nil {
		t.Fatalf("3 elements must violate max_items: 2")
	}
	if _, err := ResolveInputValues(schema, map[string]any{"hosts": []any{"a.com", "b.com"}}); err != nil {
		t.Fatalf("2 elements within [1,2] must pass: %v", err)
	}
}
