// Guards on this artifact's contract: the schema document against the Go value
// it is generated from, the parameter surface against drift, and the OUTPUT
// shape — which no document declares at all.
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
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
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

// spec is one param as this artifact promises to declare it. Everything
// param-level strictness and soul-lint read about a param is here, because
// everything here is a separate way the surface can move: a `Required` dropped
// silently accepts a call that is missing what the action needs, a `Type`
// widened lets a string reach a reader expecting a number, and a `Default`
// changed moves what a scenario gets without the scenario changing.
type spec struct {
	typ      schema.ParamType
	required bool
	def      any
}

// connParams — the params every action carries.
var connParams = map[string]spec{
	connEndpoint:  {typ: schema.String, required: true},
	connNamespace: {typ: schema.String},
}

// ownParams — what each action declares BEYOND the connection params.
//
// ★★ This table replaces the guard that compared this surface, key for key,
// against a vendored copy of a cloud provider's published document (NIM-873).
// That comparison was the only thing holding the parameters still, and dropping
// it without a replacement would have left them free to drift by accident.
//
// It is a better replacement for a plugin that answers to libvirt rather than to
// a foreign contract, because it states what vmlocal OFFERS rather than how far
// it has strayed from someone else. It has to pin the same FIVE dimensions the
// old one did, though — name, type, required, default, and (in
// [TestNoParamCarriesACredential]) secret/pattern — since a guard over names
// alone would let `Required: true` fall off `endpoint` in silence.
var ownParams = map[string]map[string]spec{
	"created": {
		"count":    {typ: schema.Int, def: 1},
		"name":     {typ: schema.String},
		"profile":  {typ: schema.Map, required: true},
		"userdata": {typ: schema.String},
	},
	"destroyed": {
		"vm_ids": {typ: schema.List, required: true},
	},
	"probed": {
		"vm_ids":    {typ: schema.List},
		"run_label": {typ: schema.String},
	},
	"resized": {
		"vm_ids":         {typ: schema.List, required: true},
		"cpu_cores":      {typ: schema.Int, def: 0},
		"ram_mb":         {typ: schema.Int, def: 0},
		"disk_gb":        {typ: schema.Int, def: 0},
		"allow_downtime": {typ: schema.Bool, def: false},
	},
}

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

// The PUBLISHED input surface is exactly what [ownParams] plus [connParams] say
// it is — every key, and every property of every key.
//
// No more, because a param an artifact declares and cannot use is a promise to
// the operator that nobody keeps; no less, because a key a state omits is
// refused on a call that was perfectly legitimate.
//
// ⚠ "cannot use" is a JUDGEMENT made when this table is edited, not a property
// checked here — nothing proves a declared step param is read by anything. The
// profile half below does check that, because it can: parseProfile fills a
// struct. A step param reaches four different functions.
func TestPublishedInputSurfaceIsExactlyDeclared(t *testing.T) {
	states := parseDoc(t).Modules[0].States
	if got := slices.Sorted(maps.Keys(states)); !slices.Equal(got, slices.Sorted(maps.Keys(ownParams))) {
		t.Fatalf("actions are %v, the table covers %v", got, slices.Sorted(maps.Keys(ownParams)))
	}
	for state, own := range ownParams {
		want := maps.Clone(own)
		maps.Copy(want, connParams)

		got := states[state].Input
		if a, b := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)); !slices.Equal(a, b) {
			t.Errorf("state %q declares %v, want %v", state, a, b)
			continue
		}
		for key, w := range want {
			p := got[key]
			if p.Type != w.typ {
				t.Errorf("state %q param %q: type=%q, declared %q", state, key, p.Type, w.typ)
			}
			if p.Required != w.required {
				t.Errorf("state %q param %q: required=%v, declared %v", state, key, p.Required, w.required)
			}
			// The document is JSON, so an int default comes back a float64.
			if !sameDefault(p.Default, w.def) {
				t.Errorf("state %q param %q: default=%#v, declared %#v", state, key, p.Default, w.def)
			}
		}
	}
}

// sameDefault compares a default across the JSON round trip the document makes.
func sameDefault(published, declared any) bool {
	if published == nil || declared == nil {
		return published == nil && declared == nil
	}
	return fmt.Sprintf("%v", published) == fmt.Sprintf("%v", declared)
}

// ★ The batch-identity label is a STRING the descriptions name in prose, and
// `probed`'s `run_label` filter is only usable if the two agree. Renaming the
// constant without the prose leaves an operator filtering on a label nothing
// stamps — a silent empty result, not an error.
func TestTheRunLabelKeyIsNamedInTheDocument(t *testing.T) {
	doc := parseDoc(t)
	for _, state := range []string{"created", "probed"} {
		if !strings.Contains(doc.Modules[0].States[state].Description+fmt.Sprint(doc.Modules[0].States[state].Input), runLabelKey) {
			t.Errorf("state %q never names %q, the label it stamps and filters by", state, runLabelKey)
		}
	}
	if !slices.Contains(hostAttrKeys, "run_label") {
		t.Error("hosts[].attributes no longer echoes run_label; a scenario cannot read back the identity it filtered by")
	}
}

// ★ No param of this artifact carries a credential, and that is a property
// rather than an accident: a libvirtd over its unix socket authenticates by the
// permissions on that socket, and TLS to a remote one is configured in libvirt's
// own client files. Re-introducing a `secret: true` param means this artifact
// has acquired something to authenticate WITH — which is a design decision, not
// a parameter addition.
func TestNoParamCarriesACredential(t *testing.T) {
	for name, st := range parseDoc(t).Modules[0].States {
		for key, p := range st.Input {
			if p.Secret || p.Pattern == "^vault:.*" {
				t.Errorf("state %q declares %q as a secret; vmlocal presents no credential to libvirt", name, key)
			}
		}
	}
}

// ★★ The profile is the half of the surface the PLATFORM CANNOT SEE.
//
// `profile` is declared [module.Map], so param-level strictness type-checks
// nothing inside it — the ten fields that decide what machine gets built are
// exactly the ten the schema does not reach. [profileVocabulary] closes that set
// in code, and this holds it to the parser: a name in the vocabulary that
// parseProfile does not read leaves its field at the zero value here and reds.
//
// ⚠ The other direction is NOT held, and cannot be cheaply: a reader added for a
// key left out of the vocabulary is dead rather than dangerous — the vocabulary
// refuses that key, so the reader never sees it. The failure it would cause is a
// refusal, which is loud.
func TestProfileVocabularyIsClosedAndFullyRead(t *testing.T) {
	full := map[string]any{
		"namespace":           "ns",
		"image_name":          "debian-12",
		"image_id":            "11111111-2222-3333-4444-555555555555",
		"network_id":          "66666666-7777-8888-9999-000000000000",
		"cpu_size":            2,
		"ram_size":            1 << 30,
		"boot_disk_size":      3 << 30,
		"boot_disk_name":      "disk-0",
		"deletion_protection": true,
		"labels":              map[string]any{runLabelKey: "batch"},
	}
	if got := slices.Sorted(maps.Keys(full)); !slices.Equal(got, slices.Sorted(slices.Values(profileVocabulary))) {
		t.Fatalf("this test populates %v, the vocabulary is %v", got, profileVocabulary)
	}

	// Every vocabulary key reaches the struct. A name nothing reads would leave
	// its field at the zero value here.
	p, errs := parseProfile(full)
	if len(errs) > 0 {
		t.Fatalf("a fully populated profile was refused: %v", errs)
	}
	for name, zero := range map[string]bool{
		"namespace": p.namespace == "", "image_id": p.imageID == "", "image_name": p.imageName == "",
		"network_id": p.networkID == "", "cpu_size": p.cpuSize == 0, "ram_size": p.ramSize == 0,
		"boot_disk_size": p.bootDiskSize == 0, "boot_disk_name": p.bootDiskName == "",
		"deletion_protection": !p.deletionProtection, "labels": len(p.labels) == 0,
		"labels[soulstack-run]": p.runLabel == "",
	} {
		if zero {
			t.Errorf("profile.%s is in the vocabulary but parseProfile did not read it", name)
		}
	}

	// And a key outside the set is refused rather than ignored — the five that
	// used to be refused by name because a cloud contract declared them, the two
	// this artifact itself dropped, and a plain typo.
	for _, key := range []string{
		"set_external_ip", "external_ip_id", "anti_affinity", "cluster", "image_version",
		"rm_external_id", "namespace_id",
		"cpu_sizes",
	} {
		one := maps.Clone(full)
		one[key] = "x"
		_, errs := parseProfile(one)
		if !hasError(errs, "profile."+key+" is not a vmlocal profile field") {
			t.Errorf("profile.%s was accepted; errors=%v", key, errs)
		}
	}
}

// ★★ THE OUTPUT SHAPE, which no document declares.
//
// A module document declares INPUTS only, so `register.<task>.*` is the one
// dimension of this contract that can move under a consumer with every other
// test still green. It had already moved before this guard existed: the state
// descriptions promised `external_ip`, which this artifact has never emitted,
// and omitted `state`, which it always has.
//
// The emitters are checked against the lists the descriptions quote, so the two
// cannot part company again in silence.
func TestOutputShapeIsDeclared(t *testing.T) {
	d := domainInfo{UUID: "vm-1", Name: "web-0", Namespace: "ns"}

	for _, tc := range []struct {
		what string
		got  map[string]any
		want []string
	}{
		{"created/probed hosts[] entry", hostEntry(d, "10.0.0.2", "web-0.ns"), hostEntryKeys},
		{"hosts[] attributes", hostEntry(d, "10.0.0.2", "web-0.ns")["attributes"].(map[string]any), hostAttrKeys},
		{"hosts[] entry for a machine that never came up", hostStub("vm-1"), hostStubKeys},
		{"resized results[] entry", resizeResult("vm-1", true, true, nil), resizeKeys},
		{"resized results[] entry, failed", resizeResult("vm-1", false, true, errors.New("boom")), append(slices.Clone(resizeKeys), "error")},
	} {
		if got := slices.Sorted(maps.Keys(tc.got)); !slices.Equal(got, slices.Sorted(slices.Values(tc.want))) {
			t.Errorf("%s emits %v, declared %v", tc.what, got, tc.want)
		}
	}

	// ★ The descriptions an operator reads are part of the declaration, and the
	// fragment they must contain is DERIVED from the same lists — so reordering or
	// renaming a key forces the prose to move with it. A literal here would let the
	// code and the document drift apart together, which is the failure this guard
	// exists for: the descriptions promised `external_ip` for as long as they did
	// because nothing tied them to what `hostEntry` returns.
	hostShape := "{" + strings.Join(hostEntryKeys, ", ") + "}"
	attrShape := "{" + strings.Join(hostAttrKeys, ", ") + "}"
	stubShape := "{" + strings.Join(hostStubKeys, ", ") + "}"
	resizeShape := "{" + strings.Join(resizeKeys, ", ") + ", error?}"

	states := parseDoc(t).Modules[0].States
	for _, tc := range []struct{ state, want string }{
		{"created", hostShape},
		{"created", attrShape},
		{"created", stubShape},
		{"probed", hostShape},
		{"probed", attrShape},
		{"destroyed", stubShape},
		{"resized", resizeShape},
	} {
		if !strings.Contains(states[tc.state].Description, tc.want) {
			t.Errorf("state %q does not describe its output as %s", tc.state, tc.want)
		}
	}
}
