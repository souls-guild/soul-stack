package config

import "testing"

// Vendor-extension `x-*` в input-схеме (конвенция OpenAPI/JSON-Schema): backend
// прокидывает такие ключи в сырой DTO input_schema как аннотации UI (NIM-76:
// `x-directives: redis`). Валидатор их не трогает (passthrough), но НЕ-x-ключи —
// по-прежнему unknown_key (error).

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
		t.Fatalf("x-* vendor-extension key не должен давать unknown_key")
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
		t.Fatalf("неизвестный не-x- ключ обязан давать unknown_key")
	}
}
