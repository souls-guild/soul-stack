package coremanifest_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// ★ GUARD (NIM-905, NIM-863). An address the catalog does not know routes
// Soul-side without an error, so `core.ssh.apply` has to be proven keeper-side
// from the address an author writes, split the way dispatch splits it, and its
// state has to be declared — otherwise soul-lint has no schema to check its
// params against and a destiny ships to a host as a module that does not exist.
//
// Mutation: drop the `apply` state from modSSH, or `core.ssh` from
// keeperSideCore, and this reddens.
func TestSSHApplyAddressIsKeeperSideAndDeclared(t *testing.T) {
	base, state, ok := config.SplitModuleAddr("core.ssh.apply")
	if !ok || base != "core.ssh" || state != "apply" {
		t.Fatalf("SplitModuleAddr(core.ssh.apply) = %q, %q, %v", base, state, ok)
	}
	if !coremanifest.IsKeeperSide(base) {
		t.Fatalf("%s routes %s — a step reaching hosts with no agent must run on the Keeper", base, coremanifest.SideOf(base))
	}
	def, ok := coremanifest.Default().State(base, state)
	if !ok {
		t.Fatalf("%s.%s is not declared in the core catalog", base, state)
	}
	for _, required := range []string{"destiny", "hosts", "ssh_provider"} {
		if p, ok := def.Input[required]; !ok || !p.Required {
			t.Errorf("param %q: declared=%v required=%v, want a required param", required, ok, p.Required)
		}
	}
}
