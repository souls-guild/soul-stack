package coremanifest

import "testing"

// TestBootstrapDeliveredIsNotInTheCatalog is the offline half of the
// `core.bootstrap.delivered` removal (NIM-834): soul-lint resolves a core
// address against this catalog, so a state left here — even as a deprecated
// stub with an empty input — would let a scenario still carrying that address
// validate clean and go on to fail only on a live keeper, at the onboarding
// barrier rather than at the step.
//
// `issued` is asserted alongside it so a mistake that empties the module rather
// than the one state cannot pass as a success.
func TestBootstrapDeliveredIsNotInTheCatalog(t *testing.T) {
	r := Default()

	if _, ok := r.State("core.bootstrap", "delivered"); ok {
		t.Error("core.bootstrap.delivered is still declared: soul-lint would accept a scenario step that no keeper can execute")
	}
	if _, ok := r.State("core.bootstrap", "issued"); !ok {
		t.Fatal("core.bootstrap.issued is gone: minting is keeper-only and nothing else can produce a bootstrap token")
	}
}
