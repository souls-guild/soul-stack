package handlers

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// servedCoreAddrs is the full served core catalog, sorted. The two halves are held
// against the binaries' registries by `soul/internal/coremod/side_disjoint_guard_test.go`
// and `keeper/internal/coremod/side_catalog_guard_test.go`, which is what makes it a
// derived expectation here rather than a third list.
func servedCoreAddrs() []string {
	out := append(coremanifest.SoulSideAddrs(), coremanifest.KeeperSideAddrs()...)
	sort.Strings(out)
	return out
}

// ★ GUARD (NIM-890). `GET /v1/modules` lists exactly the modules the engine serves,
// read through the handler, with each declared module's states as declared.
//
// The previous assertion compared the response with the table that produced it, so
// it held for any table: six served modules were missing and three listed modules
// had lost states (`core.git` pulled, `core.service` disabled/masked, `core.sysctl`
// applied) with the suite green.
func TestModuleCatalog_CoreItemsAreTheServedCatalog(t *testing.T) {
	resp, err := NewModuleCatalogHandler(nil, nil).ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if len(coremanifest.SoulSideAddrs()) == 0 || len(coremanifest.KeeperSideAddrs()) == 0 {
		t.Fatal("a half of the served catalog is empty — the comparison would pass by shrinking")
	}
	served := servedCoreAddrs()
	var got []string
	for _, it := range resp.Items {
		got = append(got, it.Name)
		if !sort.StringsAreSorted(it.States) {
			t.Errorf("%s: states %v are not sorted", it.Name, it.States)
		}
		m, declared := coremanifest.Default().Lookup(it.Name)
		if !declared {
			continue
		}
		var want []string
		for state := range m.States {
			want = append(want, state)
		}
		sort.Strings(want)
		if !reflect.DeepEqual(it.States, want) {
			t.Errorf("%s: catalog states %v, declaration %v", it.Name, it.States, want)
		}
	}
	if !reflect.DeepEqual(got, served) {
		t.Fatalf("catalog drifted from the served set\n  catalog: %v\n  served:  %v", got, served)
	}

	if _, err := NewModuleCatalogHandler(nil, nil).GetTyped(context.Background(), "core.directory"); err != nil {
		t.Errorf("GET /v1/modules/core.directory: %v", err)
	}
}

// ★ GUARD (NIM-890). The editorial table covers the served catalog in both
// directions. A served module with no entry is still listed — with no description —
// and an entry for a module nobody serves is silently never read, so neither shows
// up in the response test above.
func TestCoreModuleDocs_CoverTheServedCatalog(t *testing.T) {
	served := servedCoreAddrs()
	for _, name := range served {
		doc, ok := coreModuleDocs[name]
		if !ok {
			t.Errorf("%s is served but has no catalog entry — the operator sees it with no description", name)
			continue
		}
		if doc.Description == "" {
			t.Errorf("%s: empty description", name)
		}
	}
	for name := range coreModuleDocs {
		if !slices.Contains(served, name) {
			t.Errorf("catalog entry %s names a module no binary serves", name)
		}
	}
}

// ★ GUARD (NIM-890). States are written by hand only where nothing can derive them,
// and errand-safe states are states the module has.
func TestCoreModuleDocs_StatesOnlyWhereThereIsNoDeclaration(t *testing.T) {
	items := map[string]ModuleCatalogItem{}
	for _, it := range coreCatalogItems() {
		items[it.Name] = it
	}
	for name, doc := range coreModuleDocs {
		_, declared := coremanifest.Default().Lookup(name)
		switch {
		case declared && len(doc.States) > 0:
			t.Errorf("%s: hand-written States %v beside a declaration — the copy is what drifted before", name, doc.States)
		case !declared && len(doc.States) == 0:
			t.Errorf("%s has no declaration and no States — the catalog would publish it with none", name)
		}
		for _, s := range doc.ErrandSafeStates {
			if !slices.Contains(items[name].States, s) {
				t.Errorf("%s: errand-safe state %q is not one of its states %v", name, s, items[name].States)
			}
		}
	}
}
