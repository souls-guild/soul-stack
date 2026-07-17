package config

import "testing"

// Vendor-extension `x-*` in the input schema (OpenAPI/JSON-Schema convention): the
// backend passes such keys into the raw input_schema DTO as UI annotations (NIM-76:
// `x-directives: redis`). The validator leaves them alone (passthrough), but non-x-
// keys are still unknown_key (error).

func TestInputSchema_VendorExtensionKeyAllowed(t *testing.T) {
	src := `name: x
input:
  redis_settings:
    type: object
    x-directives: redis
    additional_properties:
      type: string
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("x-* vendor-extension key must not produce unknown_key")
	}
}

func TestInputSchema_NonVendorUnknownKeyStillErrors(t *testing.T) {
	src := `name: x
input:
  foo:
    type: string
    bogus_key: 1
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("an unknown non-x- key must produce unknown_key")
	}
}
