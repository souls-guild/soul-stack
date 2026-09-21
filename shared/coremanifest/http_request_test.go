package coremanifest

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// TestHTTP_RequestContract pins the manifest boundary that security and the
// Keeper form catalog consume: request is present, method is explicit and the
// module has network access only (no subprocess or filesystem-write power).
func TestHTTP_RequestContract(t *testing.T) {
	mod, ok := Default().Lookup("core.http")
	if !ok {
		t.Fatal("core.http missing")
	}
	if want := []schema.Capability{schema.NetworkOutbound}; !reflect.DeepEqual(mod.Capabilities, want) {
		t.Fatalf("capabilities=%v, want exactly %v", mod.Capabilities, want)
	}

	request, ok := mod.States["request"]
	if !ok {
		t.Fatal("core.http.request missing")
	}
	method, ok := request.Input["method"]
	if !ok || !method.Required {
		t.Fatalf("request.method must be required: %+v", method)
	}
	wantMethods := []any{"POST", "PUT", "PATCH", "DELETE"}
	if !reflect.DeepEqual(method.Enum, wantMethods) {
		t.Fatalf("request.method enum=%v, want %v", method.Enum, wantMethods)
	}
	for _, name := range []string{
		"url", "headers", "body", "content_type", "status_codes", "timeout",
		"allow_private", "allow_http", "insecure_skip_verify",
	} {
		if _, ok := request.Input[name]; !ok {
			t.Errorf("request param %q missing", name)
		}
	}
}
