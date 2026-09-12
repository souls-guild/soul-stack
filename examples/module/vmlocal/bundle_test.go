// Guards on the schema document ↔ implementation contract, and on the one
// property this artifact exists for: that its address and param surface are
// `wb-cloud`'s.
//
// `modules[<object>].states.<action>.input` is the ONLY thing param-level
// strictness reads (ADR-0076, NIM-163/NIM-204): a key a state omits has no
// declaration, so a legitimate call carrying it FAILS with module.unknown_param.
// A prose promise in a description is not a declaration.
//
// The document is GENERATED from the Go value (`vmlocalBundle`), not written
// beside it: `soul-mod stamp` runs the artifact's `schema` subcommand and writes
// those bytes both into the binary and to `schema.json`.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// parseDoc reads the PUBLISHED document — the bytes soul-lint and `plugin.allow`
// see, not the Go value they are rendered from.
func parseDoc(t *testing.T) schema.Document {
	t.Helper()
	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatalf("read schema.json: %v", err)
	}
	doc, err := schema.Unmarshal(raw)
	if err != nil {
		t.Fatalf("unmarshal schema.json: %v", err)
	}
	if len(doc.Modules) != 1 || doc.Modules[0].Name != "vm" {
		t.Fatalf("modules=%+v, want exactly one named vm", doc.Modules)
	}
	return doc
}

// connParams — the params every action carries. The list is the constants
// themselves, so a rename breaks this test rather than silently leaving a state
// undeclared.
var connParams = []string{
	connKeyID, connSecret, connEndpoint,
	connNamespace, connNamespaceID,
	connCACertPEM, connClientCertPEM, connClientKeyPEM,
}

// secretParams — params carrying a credential or a PEM key. Declaring one without
// `secret: true` would leave it unmasked in logs, traces and the UI (ADR-010).
var secretParams = []string{connSecret, connCACertPEM, connClientCertPEM, connClientKeyPEM}

func TestPublishedSchemaMatchesTheBundle(t *testing.T) {
	published, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatalf("read schema.json: %v (regenerate it: see the README)", err)
	}
	rendered, err := vmlocalBundle(&VMLocal{}).Schema()
	if err != nil {
		t.Fatalf("render bundle schema: %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(published), bytes.TrimSpace(rendered)) {
		t.Error("schema.json and vmlocalBundle disagree — re-stamp the artifact and commit the result")
	}
}

// The published document must pass the SDK validator: one Keeper would refuse at
// `plugin.allow` must not pass here either.
func TestPublishedSchemaIsValid(t *testing.T) {
	doc := parseDoc(t)
	if issues := schema.Validate(doc); schema.HasErrors(issues) {
		for _, i := range issues {
			t.Errorf("%s %s: %s", i.Path, i.Code, i.Message)
		}
	}
	if doc.Kind != schema.KindSoulModule {
		t.Errorf("kind=%q, want %q", doc.Kind, schema.KindSoulModule)
	}
}

// ★ `side: keeper` is declared, and it is what NIM-758 routes by. A machine is
// created when no hosts exist yet, so this module runs on the Keeper against no
// host. Losing the line makes the artifact soul-side by default
// ([module.SideSoul] is the zero value), which fails at dispatch with nothing to
// point at.
func TestVmDeclaresSideKeeper(t *testing.T) {
	if got := vmDef(&VMLocal{}).Side; got != schema.SideKeeper {
		t.Fatalf("vm.side=%q, want %q", got, schema.SideKeeper)
	}
	if got := parseDoc(t).Modules[0].Side; got != schema.SideKeeper {
		t.Errorf("published vm.side=%q, want %q", got, schema.SideKeeper)
	}
}

// The object that SERVES a state is the one that DECLARES it. Split between a
// dispatch table and a Def, the two sets would only agree by convention.
func TestDeclaredStatesAreDispatched(t *testing.T) {
	dispatched := (&VMLocal{}).vm().states()
	declared := make([]string, 0, len(vmDef(&VMLocal{}).States))
	for name := range vmDef(&VMLocal{}).States {
		declared = append(declared, name)
	}
	slices.Sort(declared)
	if !slices.Equal(dispatched, declared) {
		t.Errorf("dispatched=%v, declared=%v", dispatched, declared)
	}
}

func TestEveryStateDeclaresTheConnectionParams(t *testing.T) {
	doc := parseDoc(t)
	for name, st := range doc.Modules[0].States {
		for _, key := range connParams {
			if _, ok := st.Input[key]; !ok {
				t.Errorf("state %q does not declare %q, which every action reads", name, key)
			}
		}
	}
}

func TestSecretParamsAreDeclaredSecret(t *testing.T) {
	doc := parseDoc(t)
	for name, st := range doc.Modules[0].States {
		for _, key := range secretParams {
			p, ok := st.Input[key]
			if !ok {
				continue
			}
			if !p.Secret {
				t.Errorf("state %q: param %q must be secret: true", name, key)
			}
		}
	}
}

// ★★ THE POINT OF THIS ARTIFACT.
//
// A plugin document carries no name of its own — address level 1 is the alias an
// operator registers it under — so registering this binary as `wb-cloud` makes an
// unmodified WB service scenario provision against libvirt. That only holds while
// the OBJECT, the ACTIONS and the PARAM SURFACE are identical, because param-level
// strictness refuses a call carrying a key the state does not declare.
//
// testdata/wb-cloud.schema.json is a vendored copy of what that artifact
// publishes. Both sides are compared as PARSED JSON so the numeric types match
// (a Go `int` default renders as a JSON number either way).
//
// Descriptions are deliberately NOT compared: they are what an operator reads,
// and a WB description would be a lie here.
//
// When this reddens, the contract moved. Re-copy the fixture and decide whether
// vmlocal follows — do not delete the test.
func TestParamSurfaceMatchesWBCloud(t *testing.T) {
	ours := parseDoc(t)

	raw, err := os.ReadFile("testdata/wb-cloud.schema.json")
	if err != nil {
		t.Fatalf("read the vendored wb-cloud document: %v", err)
	}
	theirs, err := schema.Unmarshal(raw)
	if err != nil {
		t.Fatalf("unmarshal the vendored wb-cloud document: %v", err)
	}

	if theirs.Modules[0].Name != ours.Modules[0].Name {
		t.Fatalf("object name is %q, wb-cloud's is %q: the address must match or the scenario does not resolve",
			ours.Modules[0].Name, theirs.Modules[0].Name)
	}
	if theirs.Modules[0].Side != ours.Modules[0].Side {
		t.Errorf("side is %q, wb-cloud's is %q", ours.Modules[0].Side, theirs.Modules[0].Side)
	}

	wantStates := make([]string, 0, len(theirs.Modules[0].States))
	for n := range theirs.Modules[0].States {
		wantStates = append(wantStates, n)
	}
	gotStates := make([]string, 0, len(ours.Modules[0].States))
	for n := range ours.Modules[0].States {
		gotStates = append(gotStates, n)
	}
	slices.Sort(wantStates)
	slices.Sort(gotStates)
	if !slices.Equal(wantStates, gotStates) {
		t.Fatalf("actions are %v, wb-cloud's are %v", gotStates, wantStates)
	}

	for _, state := range wantStates {
		want, got := theirs.Modules[0].States[state], ours.Modules[0].States[state]
		for key, wp := range want.Input {
			gp, ok := got.Input[key]
			if !ok {
				t.Errorf("state %q does not declare %q, which wb-cloud declares: a scenario passing it is refused as module.unknown_param", state, key)
				continue
			}
			if wp.Type != gp.Type {
				t.Errorf("state %q param %q: type=%q, wb-cloud's is %q", state, key, gp.Type, wp.Type)
			}
			if wp.Required != gp.Required {
				t.Errorf("state %q param %q: required=%v, wb-cloud's is %v", state, key, gp.Required, wp.Required)
			}
			if wp.Secret != gp.Secret {
				t.Errorf("state %q param %q: secret=%v, wb-cloud's is %v", state, key, gp.Secret, wp.Secret)
			}
			if wp.Pattern != gp.Pattern {
				t.Errorf("state %q param %q: pattern=%q, wb-cloud's is %q", state, key, gp.Pattern, wp.Pattern)
			}
			if !reflect.DeepEqual(wp.Default, gp.Default) {
				t.Errorf("state %q param %q: default=%#v, wb-cloud's is %#v", state, key, gp.Default, wp.Default)
			}
		}
		for key := range got.Input {
			if _, ok := want.Input[key]; !ok {
				t.Errorf("state %q declares %q, which wb-cloud does not: a scenario using it cannot run against the cloud", state, key)
			}
		}
	}
}

// A parameter surface identical to wb-cloud's is only half of it: the batch
// identity label the two artifacts filter on has to be the same string too, or an
// adopted batch here is an orphan there.
func TestRunLabelKeyMatchesWBCloud(t *testing.T) {
	raw, err := os.ReadFile("testdata/wb-cloud.schema.json")
	if err != nil {
		t.Fatalf("read the vendored wb-cloud document: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The key is named in wb-cloud's own prose rather than in a field, so this is
	// the honest check available: the string must appear in the document it filters by.
	if !bytes.Contains(raw, []byte(runLabelKey)) {
		t.Errorf("wb-cloud's document does not mention %q — the batch identity label diverged", runLabelKey)
	}
}
