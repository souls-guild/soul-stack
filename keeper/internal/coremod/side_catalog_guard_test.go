package coremod_test

import (
	"context"
	"reflect"
	"sort"
	"testing"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod"
	coremodbootstrap "github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// noopCertStore completes [everyKeeperModuleDeps]: `core.cert` is the one
// conditionally-registered module the existing stubs did not cover.
type noopCertStore struct{}

func (noopCertStore) SelectActive(_ context.Context, _ string, _ keepercert.Kind) (*keepercert.Warrant, error) {
	return nil, nil
}
func (noopCertStore) RegisterActive(_ context.Context, _ *keepercert.Warrant) error { return nil }

// everyKeeperModuleDeps is [baseDeps] with every CONDITIONAL dependency supplied,
// so [coremod.Default] registers the complete keeper-side set rather than the
// subset a given deployment happens to configure.
//
// Registration is deps-gated on purpose (a keeper without Vault serves fewer
// modules and says "unknown keeper-side module" for the rest), which is exactly
// why the guard below cannot read a real deployment's registry: it would compare
// the catalog against whatever this test happened to wire and pass while missing
// an address.
func everyKeeperModuleDeps() coremod.Deps {
	d := baseDeps()
	d.StateStore = noopStateStore{}
	d.ChoirStore = noopChoirStore{}
	d.CertStore = noopCertStore{}
	d.BootstrapIssuer = noopBootstrapIssuer{}
	return d
}

// TestSideCatalogMatchesTheKeeperRegistry — the catalog soul-lint and the render
// pipeline route by IS the set of modules the Keeper dispatches (NIM-749).
//
// Since the side is derived from the module address, this list is the routing
// table. A keeper-side module missing from it is dispatched to a HOST, which has
// no such module, so the run dies there — and the linter, reading the same list,
// green-lights the file that did it. That is the failure the comment on
// `keeper/internal/stateop.States` describes one level down, and the reason the
// catalog is one list rather than two.
//
// Read out of a fully-wired [coremod.Default] rather than a literal, so a new
// keeper-side module reddens this the moment it is registered, whether or not
// anyone remembered this file.
func TestSideCatalogMatchesTheKeeperRegistry(t *testing.T) {
	dispatched := coremod.Default(everyKeeperModuleDeps()).Names()
	sort.Strings(dispatched)
	if len(dispatched) < 6 {
		t.Fatalf("only %d modules registered (%v) — a dep gate closed and the comparison below would pass by being empty", len(dispatched), dispatched)
	}

	got := coremanifest.KeeperSideAddrs()
	if !reflect.DeepEqual(got, dispatched) {
		t.Fatalf("keeper-side catalog drifted from the dispatch set\n  catalog:    %v\n  dispatched: %v\n"+
			"a module the Keeper dispatches but the catalog omits is routed to a HOST, which has no such module",
			got, dispatched)
	}
}

// TestSideCatalogClassifiesEachDispatchedModule — the same fact read the way the
// engine reads it. The slices above could match while
// [coremanifest.IsKeeperSide] answered on some other key (a state suffix left on,
// a namespace dropped), and routing asks that function, not the slice.
func TestSideCatalogClassifiesEachDispatchedModule(t *testing.T) {
	for _, name := range coremod.Default(everyKeeperModuleDeps()).Names() {
		if !coremanifest.IsKeeperSide(name) {
			t.Errorf("IsKeeperSide(%q) = false — the Keeper dispatches it, so a task addressing it must route here", name)
		}
	}
	// `core.bootstrap` reaches the registry through a different gate than the
	// rest (issuer OR a delivery set), so it is the one most easily missed by a
	// deps-driven sweep. Named explicitly.
	if !coremanifest.IsKeeperSide(coremodbootstrap.Name) {
		t.Errorf("IsKeeperSide(%q) = false", coremodbootstrap.Name)
	}
}
