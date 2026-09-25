package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// A published MCP schema and the Go struct that decodes against it are two
// statements of one contract, and nothing in the compiler relates them. Args are
// read with [strictUnmarshal], which sets `DisallowUnknownFields`, so the failure
// when they disagree is total rather than partial: a caller that builds the call
// from the advertised schema — which is what an agent does, MCP being a PRIMARY
// operator surface (ADR-004) — gets `malformed-request: unknown field` and the
// tool is simply dead for it.
//
// This guard covers every tool of a registry the `name` -> `id` rename
// ([ADR-0085], NIM-729) has converted. It exists because they DID break, twice:
// first `keeper.incarnation.label-set`, the one label-set tool of ten that
// decodes into its own struct rather than the shared [labelSetArgs], and then
// nine more across the provider/profile/push-provider batch, where the Go json
// tags moved and the published schemas did not. Both times `go build` and
// `go vet` were clean and the whole unit suite was green: nothing in Go relates
// a `json.RawMessage` schema to the struct that decodes against it.
//
// A batch adds its rows to [idRenamedTools] in the same change that renames the
// registry. The identifier key each row expects is `id` — that is the point of
// the ticket — so a schema left behind fails here rather than in an operator's
// agent.
//
// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md

// labelSetDecoders maps a label-set tool to a fresh instance of the struct its
// handler actually decodes into. Nine share [labelSetArgs]; `incarnation` is
// hand-written and does not, which is the whole reason this table is not a
// single assertion.
//
// A tool missing from here is caught by the completeness check below rather than
// silently skipped — the set on the left is read out of the manifest.
var labelSetDecoders = map[string]func() any{
	"keeper.incarnation.label-set":  func() any { return &incarnationLabelSetArgs{} },
	"keeper.service.label-set":      func() any { return &labelSetArgs{} },
	"keeper.augur.omen.label-set":   func() any { return &labelSetArgs{} },
	"keeper.oracle.vigil.label-set": func() any { return &labelSetArgs{} },
	"keeper.oracle.decree.label-set": func() any {
		return &labelSetArgs{}
	},
	"keeper.push-provider.label-set": func() any { return &labelSetArgs{} },
	"keeper.herald.label-set":        func() any { return &labelSetArgs{} },
	"keeper.tiding.label-set":        func() any { return &labelSetArgs{} },
}

// labelSetTools returns every published `.label-set` tool and its input schema,
// read out of the manifest so the guard's scope follows the catalog.
func labelSetTools(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	for _, e := range catalogManifest {
		if strings.HasSuffix(e.decl.Name, ".label-set") {
			out[e.decl.Name] = e.decl.InputSchema
		}
	}
	if len(out) == 0 {
		t.Fatal("the manifest publishes no `.label-set` tool — every case below would pass vacuously")
	}
	return out
}

// schemaShape is the part of a published input schema this guard reads.
type schemaShape struct {
	Required   []string                   `json:"required"`
	Properties map[string]json.RawMessage `json:"properties"`
}

// bodyFromSchema synthesizes the widest request an agent could build from the
// schema: EVERY declared property, not only the required ones. That is the
// strongest form of the question, because `DisallowUnknownFields` rejects on any
// key the struct does not know — an optional property the struct dropped is just
// as fatal as a renamed required one.
//
// The value sent for each property follows the type the schema DECLARES, so a
// type mismatch reported below is a real disagreement about the field and not an
// artefact of this helper sending a string at an object.
func bodyFromSchema(s schemaShape) ([]byte, error) {
	obj := map[string]any{}
	for name, raw := range s.Properties {
		var prop struct {
			Type any `json:"type"`
		}
		if err := json.Unmarshal(raw, &prop); err != nil {
			return nil, fmt.Errorf("property %q: %w", name, err)
		}
		v, err := sampleFor(prop.Type)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", name, err)
		}
		obj[name] = v
	}
	return json.Marshal(obj)
}

// sampleFor returns a value of the JSON kind the schema declares. `type` is
// either a string or an array of them (`["string","null"]`); a nullable type
// takes its non-null branch, since null would exercise nothing.
func sampleFor(t any) (any, error) {
	switch v := t.(type) {
	case string:
		switch v {
		case "string":
			return "x", nil
		case "object":
			return map[string]any{}, nil
		case "array":
			return []any{}, nil
		case "boolean":
			return true, nil
		case "integer", "number":
			return 1, nil
		case "null":
			return nil, nil
		}
		return nil, fmt.Errorf("unhandled schema type %q — teach sampleFor about it rather than "+
			"letting the guard report a false disagreement", v)
	case []any:
		for _, alt := range v {
			if alt == "null" {
				continue
			}
			return sampleFor(alt)
		}
		return nil, nil
	}
	return nil, fmt.Errorf("property declares no usable `type` (%v)", t)
}

// TestLabelSetSchemasDecodeIntoTheirArgsStruct — the guard.
func TestLabelSetSchemasDecodeIntoTheirArgsStruct(t *testing.T) {
	for tool, raw := range labelSetTools(t) {
		t.Run(tool, func(t *testing.T) {
			newArgs, covered := labelSetDecoders[tool]
			if !covered {
				t.Fatalf("no decoder registered for %s — see the completeness test", tool)
			}

			var shape schemaShape
			if err := json.Unmarshal(raw, &shape); err != nil {
				t.Fatalf("the published input schema of %s is not JSON: %v", tool, err)
			}

			// The identifier key. [ADR-0085] / NIM-729 spells it `id` on every
			// registry, including the nine whose own `name` -> `id` rename has
			// not landed yet: this argument goes through the SHARED plumbing, so
			// it moved for all ten at once.
			if len(shape.Required) != 1 || shape.Required[0] != "id" {
				t.Errorf("%s declares required=%v, want exactly [id] — the label-set family addresses "+
					"its row by `id` on all ten registries ([ADR-0085], NIM-729)", tool, shape.Required)
			}

			body, err := bodyFromSchema(shape)
			if err != nil {
				t.Fatalf("building a request from the schema of %s: %v", tool, err)
			}
			if err := strictUnmarshal(body, newArgs()); err != nil {
				t.Errorf("%s: the published schema and the decoding struct disagree: %v\n"+
					"  schema-derived body: %s\n"+
					"args are read with DisallowUnknownFields, so an agent building the call from the "+
					"advertised schema gets `malformed-request` and the tool is dead for it. Move the "+
					"struct's json tags and the schema in the same change.", tool, err, body)
			}
		})
	}
}

// TestLabelSetDecoderTableIsComplete keeps the table above honest in both
// directions: a new label-set tool with no decoder registered would otherwise be
// skipped by the guard rather than caught by it, and a stale entry would keep
// asserting about a tool that no longer exists.
func TestLabelSetDecoderTableIsComplete(t *testing.T) {
	published := labelSetTools(t)

	var missing, stale []string
	for tool := range published {
		if _, ok := labelSetDecoders[tool]; !ok {
			missing = append(missing, tool)
		}
	}
	for tool := range labelSetDecoders {
		if _, ok := published[tool]; !ok {
			stale = append(stale, tool)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	if len(missing) > 0 {
		t.Errorf("published label-set tools with no decoder in labelSetDecoders: %v — their schema and "+
			"their args struct are free to disagree unnoticed", missing)
	}
	if len(stale) > 0 {
		t.Errorf("labelSetDecoders names tools the manifest does not publish: %v", stale)
	}
}

// idRenamedTools — every OTHER tool of a registry NIM-729 has converted, with
// the struct its handler decodes into. Label-set is covered above; this is the
// create/get/update/delete surface, which is where the second round of drift
// happened.
//
// A registry whose rename has not landed yet is deliberately absent: its tools
// still take `name` and asserting `id` on them would be asserting the future.
var idRenamedTools = map[string]func() any{
	"keeper.push-provider.create":      func() any { return &pushProviderCreateArgs{} },
	"keeper.push-provider.update":      func() any { return &pushProviderUpdateArgs{} },
	"keeper.push-provider.read":        func() any { return &pushProviderByIDArgs{} },
	"keeper.push-provider.delete":      func() any { return &pushProviderByIDArgs{} },
	"keeper.augur.omen.create":         func() any { return &omenCreateArgs{} },
	"keeper.augur.omen.delete":         func() any { return &omenDeleteArgs{} },
	"keeper.herald.create":             func() any { return &heraldCreateArgs{} },
	"keeper.herald.update":             func() any { return &heraldUpdateArgs{} },
	"keeper.herald.read":               func() any { return &heraldByIDArgs{} },
	"keeper.herald.delete":             func() any { return &heraldByIDArgs{} },
	"keeper.tiding.create":             func() any { return &tidingCreateArgs{} },
	"keeper.tiding.update":             func() any { return &tidingUpdateArgs{} },
	"keeper.tiding.read":               func() any { return &tidingByIDArgs{} },
	"keeper.tiding.delete":             func() any { return &tidingByIDArgs{} },
	"keeper.incarnation.create":        func() any { return &incarnationCreateArgs{} },
	"keeper.incarnation.get":           func() any { return &incarnationGetArgs{} },
	"keeper.incarnation.run":           func() any { return &incarnationRunArgs{} },
	"keeper.incarnation.unlock":        func() any { return &incarnationUnlockArgs{} },
	"keeper.incarnation.destroy":       func() any { return &incarnationDestroyArgs{} },
	"keeper.service.register":          func() any { return &serviceRegisterArgs{} },
	"keeper.service.update":            func() any { return &serviceUpdateArgs{} },
	"keeper.service.deregister":        func() any { return &serviceDeregisterArgs{} },
	"keeper.incarnation.upgrade":       func() any { return &incarnationUpgradeArgs{} },
	"keeper.incarnation.history":       func() any { return &incarnationHistoryArgs{} },
	"keeper.incarnation.rerun-last":    func() any { return &incarnationRerunLastArgs{} },
	"keeper.incarnation.traits-set":    func() any { return &incarnationTraitsSetArgs{} },
	"keeper.incarnation.bind-member":   func() any { return &incarnationBindMemberArgs{} },
	"keeper.incarnation.unbind-member": func() any { return &incarnationUnbindMemberArgs{} },
	"keeper.incarnation.members":       func() any { return &incarnationMembersArgs{} },
	"keeper.oracle.vigil.create":       func() any { return &vigilCreateArgs{} },
	"keeper.oracle.vigil.delete":       func() any { return &vigilDeleteArgs{} },
	"keeper.oracle.decree.create":      func() any { return &decreeCreateArgs{} },
	"keeper.oracle.decree.delete":      func() any { return &decreeDeleteArgs{} },
}

// TestIDRenamedToolSchemasDecodeIntoTheirArgsStruct — the same question as
// [TestLabelSetSchemasDecodeIntoTheirArgsStruct], asked of the wider surface.
func TestIDRenamedToolSchemasDecodeIntoTheirArgsStruct(t *testing.T) {
	published := map[string]json.RawMessage{}
	for _, e := range catalogManifest {
		published[e.decl.Name] = e.decl.InputSchema
	}

	for tool, newArgs := range idRenamedTools {
		t.Run(tool, func(t *testing.T) {
			raw, ok := published[tool]
			if !ok {
				t.Fatalf("%s is in idRenamedTools but the manifest does not publish it", tool)
			}
			var shape schemaShape
			if err := json.Unmarshal(raw, &shape); err != nil {
				t.Fatalf("the published input schema of %s is not JSON: %v", tool, err)
			}
			// The schema must OFFER `id`, not necessarily require it:
			// `keeper.incarnation.create` deliberately leaves the identifier
			// optional because a scenario with an `id:` block composes it server-side
			// ([ADR-0079]), and demanding it here would assert the opposite of
			// what that design decided.
			if !hasProperty(shape, "id") {
				t.Errorf("%s advertises no `id` property (properties=%v) — this registry has been "+
					"converted by NIM-729 and its tools address a row by `id`", tool, propertyNames(shape))
			}
			if requires(shape, "name") || hasProperty(shape, "name") {
				t.Errorf("%s still advertises a `name` property (required=%v) after its registry was "+
					"converted; the schema and the struct have drifted", tool, shape.Required)
			}
			body, err := bodyFromSchema(shape)
			if err != nil {
				t.Fatalf("building a request from the schema of %s: %v", tool, err)
			}
			if err := strictUnmarshal(body, newArgs()); err != nil {
				t.Errorf("%s: the published schema and the decoding struct disagree: %v\n"+
					"  schema-derived body: %s\n"+
					"args are read with DisallowUnknownFields, so an agent building the call from the "+
					"advertised schema gets `malformed-request` and the tool is dead for it.", tool, err, body)
			}
		})
	}
}

func requires(shape schemaShape, want string) bool {
	for _, x := range shape.Required {
		if x == want {
			return true
		}
	}
	return false
}

func hasProperty(s schemaShape, want string) bool {
	_, ok := s.Properties[want]
	return ok
}

func propertyNames(s schemaShape) []string {
	out := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// convertedRegistryToolPrefixes — the tool-name prefixes of the ten registries
// [ADR-0085] / NIM-729 converted. A tool under one of these addresses, returns
// or filters a row whose identifier is now `id`.
var convertedRegistryToolPrefixes = []string{
	"keeper.push-provider.",
	"keeper.augur.omen.",
	"keeper.herald.",
	"keeper.tiding.",
	"keeper.oracle.vigil.",
	"keeper.oracle.decree.",
	"keeper.incarnation.",
	"keeper.service.",
}

// idRenamedToolsExcused — tools under a converted prefix that legitimately do
// NOT take the row's identifier as an argument, with the reason. An excuse is a
// claim about the tool, so it is written here where the completeness test can
// see it rather than by silently leaving a row out of the table.
var idRenamedToolsExcused = map[string]string{
	"keeper.push-provider.list": "same; its filter is `id_pattern`, checked by the REST/MCP parity assertion",
	"keeper.herald.list":        "same",
	"keeper.tiding.list":        "same",
	"keeper.service.list":       "same",
	"keeper.incarnation.list":   "same",
	"keeper.augur.omen.list":    "same",
	"keeper.oracle.vigil.list":  "same",
	"keeper.oracle.decree.list": "same",
}

// TestIDRenamedToolTableIsComplete — the counterpart [idRenamedTools] was
// missing, and its absence is not academic: the table is hand-maintained, and a
// tool of a converted registry that nobody adds to it is a tool nothing checks.
//
// The required set is derived from the MANIFEST, so adding a tool to a converted
// registry fails this test until it is either covered or explicitly excused.
func TestIDRenamedToolTableIsComplete(t *testing.T) {
	var missing []string
	for _, e := range catalogManifest {
		name := e.decl.Name
		under := false
		for _, p := range convertedRegistryToolPrefixes {
			if strings.HasPrefix(name, p) {
				under = true
				break
			}
		}
		if !under {
			continue
		}
		if _, ok := idRenamedTools[name]; ok {
			continue
		}
		if _, ok := idRenamedToolsExcused[name]; ok {
			continue
		}
		// A `.label-set` tool is checked by labelSetDecoders above — the same
		// question asked by the other half of this file. Covered is covered.
		if _, ok := labelSetDecoders[name]; ok {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these tools belong to a registry NIM-729 converted but are neither covered by "+
			"idRenamedTools nor excused:\n  %s\nAdd each to the table with the struct that decodes "+
			"its arguments, or to idRenamedToolsExcused with the reason it takes no identifier.",
			strings.Join(missing, "\n  "))
	}
	for name := range idRenamedToolsExcused {
		if _, ok := idRenamedTools[name]; ok {
			t.Errorf("%s is both covered and excused — one of the two is wrong", name)
		}
	}
}

// walkObjectNodes calls fn for every object node in a JSON-Schema document that
// declares `properties`, recursing through `properties` and `items`. A registry
// row is often nested (`{"services":[{…}]}`), which is exactly why the earlier
// version of this file — which read only the top level of InputSchema — could
// not see the drift in a list's item schema.
func walkObjectNodes(raw json.RawMessage, fn func(props map[string]json.RawMessage, required []string)) {
	var node struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
		Items      json.RawMessage            `json:"items"`
	}
	if json.Unmarshal(raw, &node) != nil {
		return
	}
	if node.Properties != nil {
		fn(node.Properties, node.Required)
	}
	for _, sub := range node.Properties {
		walkObjectNodes(sub, fn)
	}
	if len(node.Items) > 0 {
		walkObjectNodes(node.Items, fn)
	}
}

// TestIDRenamedToolOutputSchemasSpellTheIdentifierID — the OUTPUT side, which
// nothing checked and which is where the drift survived: `keeper.service.list`
// published an item requiring `name` long after `serviceView` emitted `id`.
//
// A registry ROW is recognised by carrying both `created_at` and `updated_at` —
// every one of the ten does, and a nested shape that is not a row (a history
// entry, a run summary) does not carry the pair. On such a node the identifier
// must be `id`: an agent that validates the reply against the advertised schema
// otherwise drops or rejects the identifier of every row it is shown.
func TestIDRenamedToolOutputSchemasSpellTheIdentifierID(t *testing.T) {
	for _, e := range catalogManifest {
		name := e.decl.Name
		under := false
		for _, p := range convertedRegistryToolPrefixes {
			if strings.HasPrefix(name, p) {
				under = true
				break
			}
		}
		if !under || len(e.decl.OutputSchema) == 0 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			walkObjectNodes(e.decl.OutputSchema, func(props map[string]json.RawMessage, required []string) {
				_, hasCreated := props["created_at"]
				_, hasUpdated := props["updated_at"]
				if !hasCreated || !hasUpdated {
					return // not a registry row
				}
				if _, ok := props["name"]; ok {
					keys := make([]string, 0, len(props))
					for k := range props {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					t.Errorf("%s publishes a registry row whose identifier is still `name` "+
						"(properties=%v) — this registry was converted by NIM-729 and the handler "+
						"emits `id`", name, keys)
				}
				if _, ok := props["id"]; !ok {
					keys := make([]string, 0, len(props))
					for k := range props {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					t.Errorf("%s publishes a registry row with no `id` property (properties=%v)", name, keys)
				}
			})
		})
	}
}
