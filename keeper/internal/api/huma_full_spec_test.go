// Proof-of-concept pilot gate for the unified huma-OpenAPI-spec aggregator (Teardown T4a).
//
// The tests prove four properties of the assembled spec ([buildFullOpenAPISpec] /
// [HumaFullSpecYAML]):
//
//	(a) NO (path+method) duplicates after prefixing — all operations are unique by
//	    full path (otherwise buildFullOpenAPISpec would return pathMethodCollisionError).
//	(b) NO schema-merge collisions: same-named schemas from different domains have
//	    identical bodies (HumaProblemError ×20, Voyage/VoyageSummary/VoyageTarget ×2 —
//	    dedup safely). A body mismatch → schemaCollisionError → needs_architect.
//	(c) the assembled spec is VALID OpenAPI 3.1: openapi==3.1.0 + non-empty paths +
//	    components.schemas, every path item carries ≥1 HTTP-method operation, YAML
//	    parses without errors.
//	(d) the spec CONTAINS all expected routes — the (path+method) set of the assembled
//	    spec matches the real chi routes of the Operator API (chi.Walk(buildRouter) ∪
//	    opt-in routes from pathAllowlist; health/meta outside /v1 excluded). Drift guard:
//	    the aggregator didn't forget a domain.
package api

import (
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	yaml "gopkg.in/yaml.v3"
)

// TestFullSpec_NoPathMethodCollision — gate (a). buildFullOpenAPISpec already fails with
// pathMethodCollisionError on a duplicate; here we confirm the build succeeds and the
// operation count matches the registered count (no silent loss).
func TestFullSpec_NoPathMethodCollision(t *testing.T) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	// Recount of operations: should be >100 (contract ~120). Any path+method duplicate
	// would already be caught inside buildFullOpenAPISpec — here we additionally pin
	// the order of magnitude so the test catches an accidental shrinkage of the group set.
	ops := 0
	for _, item := range spec.Paths {
		ops += len(pathItemOps(item))
	}
	if ops < 100 {
		t.Fatalf("got %d operations - expected ~120; possibly a group registration failed", ops)
	}
	t.Logf("gate (a): %d paths, %d operations, no path+method duplicates", len(spec.Paths), ops)
}

// TestFullSpec_NoSchemaCollision — gate (b), the main unknown. buildFullOpenAPISpec
// returns schemaCollisionError for same-named schemas with different bodies. The test
// explicitly iterates ALL groups and collects name→{domain→body}, proving that for any
// duplicate name the body is identical (so dedup is safe and needs_architect isn't needed).
func TestFullSpec_NoSchemaCollision(t *testing.T) {
	// The direct build must pass without a collision.
	if _, err := buildFullOpenAPISpec(); err != nil {
		t.Fatalf("schema-merge collision (gate b): %v\n-> needs_architect: how to namespace same-named schemas from different domains", err)
	}

	// Independent iteration over groups: for each schema name we collect the set of
	// distinct bodies. >1 distinct body under one name = a collision (must not
	// happen). Duplicate names with an identical body are listed for the record.
	installHumaErrorOverride()
	bodies := map[string]map[string]struct{}{} // name -> set(body)
	dupNames := map[string]int{}               // name -> how many groups produced it
	for i, g := range fullSpecGroups() {
		api := newHumaCadenceAPI(chi.NewRouter())
		if err := g.register(api); err != nil {
			t.Fatalf("group #%d register: %v", i, err)
		}
		for name, sch := range api.OpenAPI().Components.Schemas.Map() {
			body, err := yamlMarshalSchema(sch)
			if err != nil {
				t.Fatal(err)
			}
			if bodies[name] == nil {
				bodies[name] = map[string]struct{}{}
			}
			bodies[name][body] = struct{}{}
			dupNames[name]++
		}
	}

	var collided []string
	for name, set := range bodies {
		if len(set) > 1 {
			collided = append(collided, name)
		}
	}
	if len(collided) > 0 {
		sort.Strings(collided)
		t.Fatalf("gate (b) FAILED: schemas with the same name but DIFFERENT bodies between domains: %v\n-> needs_architect", collided)
	}

	var shared []string
	for name, n := range dupNames {
		if n > 1 {
			shared = append(shared, name)
		}
	}
	sort.Strings(shared)
	t.Logf("gate (b): 0 collisions; same-named schemas with identical body (safe dedup): %v", shared)
}

// TestFullSpec_ValidOpenAPI31 — gate (c). Parses the YAML of the assembled spec and checks
// the required 3.1 fields: openapi==3.1.0, non-empty paths + components.schemas, every
// path item has ≥1 HTTP-method operation, schema $refs resolve within the document.
func TestFullSpec_ValidOpenAPI31(t *testing.T) {
	y, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("assembled spec does not parse as YAML: %v", err)
	}

	if v, _ := doc["openapi"].(string); v != "3.1.0" {
		t.Errorf("openapi=%q, expected 3.1.0", v)
	}
	if _, ok := doc["info"]; !ok {
		t.Error("required field info is missing")
	}

	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatal("paths is empty or not a map - not a valid 3.1 spec")
	}
	comp, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatal("components is missing")
	}
	schemas, ok := comp["schemas"].(map[string]any)
	if !ok || len(schemas) == 0 {
		t.Fatal("components.schemas is empty - operations without bodies?")
	}

	validMethods := map[string]struct{}{
		"get": {}, "put": {}, "post": {}, "delete": {},
		"options": {}, "head": {}, "patch": {}, "trace": {},
	}
	for p, item := range paths {
		pi, ok := item.(map[string]any)
		if !ok {
			t.Errorf("path-item %q is not a map", p)
			continue
		}
		hasOp := false
		for k := range pi {
			if _, isMethod := validMethods[strings.ToLower(k)]; isMethod {
				hasOp = true
				break
			}
		}
		if !hasOp {
			t.Errorf("path %q has no single HTTP operation", p)
		}
	}

	// $ref integrity: every #/components/schemas/<Name> in the document must resolve.
	refs := collectSchemaRefs(doc)
	for ref := range refs {
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		if name == ref {
			continue // not a local schemas-ref
		}
		if _, ok := schemas[name]; !ok {
			t.Errorf("$ref %q does not resolve - schema %q is missing from components.schemas (broken merge)", ref, name)
		}
	}

	t.Logf("gate (c): valid 3.1 spec - %d paths, %d schemas, all $ref resolve", len(paths), len(schemas))
}

// TestFullSpec_CoversAllRoutes — gate (d), drift guard. The (method, path) set of the
// assembled spec must match the real routes of the Operator API:
// chi.Walk(buildRouter) gives routes for non-opt-in domains; opt-in domains in the
// drift-test router = nil, their routes live in pathAllowlist — added from there. Health/meta
// (/healthz, /readyz, /openapi.yaml, /openapi.json) — outside /v1, not huma domains, excluded.
func TestFullSpec_CoversAllRoutes(t *testing.T) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	specSet := map[route]struct{}{}
	for path, item := range spec.Paths {
		for method := range pathItemOps(item) {
			specSet[route{method: method, path: normalizePath(path)}] = struct{}{}
		}
	}

	// Real routes: non-opt-in from chi.Walk + opt-in from pathAllowlist.
	realSet := map[route]struct{}{}
	for r := range collectRoutes(t) {
		if strings.HasSuffix(r.path, wildcardSuffix) {
			continue // chi catch-all 404
		}
		if isHealthMetaRoute(r) {
			continue // outside /v1, not a huma domain
		}
		realSet[r] = struct{}{}
	}
	for r := range pathAllowlist {
		realSet[r] = struct{}{}
	}

	var inSpecNotReal, inRealNotSpec []string
	for r := range specSet {
		if _, ok := realSet[r]; !ok {
			inSpecNotReal = append(inSpecNotReal, r.String())
		}
	}
	for r := range realSet {
		if _, ok := specSet[r]; !ok {
			inRealNotSpec = append(inRealNotSpec, r.String())
		}
	}
	sort.Strings(inSpecNotReal)
	sort.Strings(inRealNotSpec)

	if len(inRealNotSpec) > 0 {
		t.Errorf("ROUTE EXISTS, compiled spec does NOT (aggregator forgot a domain/group) - %d:\n  %s",
			len(inRealNotSpec), strings.Join(inRealNotSpec, "\n  "))
	}
	if len(inSpecNotReal) > 0 {
		t.Errorf("IN SPEC, real route does NOT exist (extra/incorrectly-prefixed operation) - %d:\n  %s",
			len(inSpecNotReal), strings.Join(inSpecNotReal, "\n  "))
	}

	t.Logf("gate (d): spec and routes match - %d routes covered", len(specSet))
}

// TestFullSpec_NoTechnicalSchemaNames — FINAL END-TO-END CLEANLINESS GATE for the spec (batch N6,
// before the T4c served-switch). Assembles the aggregator spec and checks that NOT A SINGLE
// schema name carries a technical/drift marker:
//
//	(1) substring markers of huma Go names and envelope drifts: "HumaBody" (request/reply Go type
//	    not aligned), "Response" (reply drift instead of the contractual Reply), "PagedResponse"
//	    (un-aliased generic envelope), "DTO" (handler-DTO name);
//	(2) oapi capitalization drifts (the oapi generator emits ALLCAPS abbreviations —
//	    SSHTargetReply instead of SshTargetReply, etc.).
//
// The ONLY exception is "HumaProblemError" (huma's RFC 7807 error wrapper, not a domain
// schema, the name is set by the huma framework). Any other name with a marker = unfinished
// alignment → the test fails red with a list of the remaining ones.
func TestFullSpec_NoTechnicalSchemaNames(t *testing.T) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	// Substring markers of technical/drift names.
	substrMarkers := []string{"HumaBody", "Response", "PagedResponse", "DTO"}
	// ALLCAPS abbreviations from the oapi generator (capitalization drift from the contractual CamelCase).
	capsMarkers := []string{"SSH", "HTTP", "URL", "JSON", "ACL", "DNS", "TLS", "TTL"}

	const allowed = "HumaProblemError" // huma's RFC 7807 wrapper — allowed

	var offenders []string
	for name := range spec.Components.Schemas.Map() {
		if name == allowed {
			continue
		}
		for _, m := range substrMarkers {
			if strings.Contains(name, m) {
				offenders = append(offenders, name+" (marker "+m+")")
			}
		}
		for _, c := range capsMarkers {
			if strings.Contains(name, c) {
				offenders = append(offenders, name+" (ALLCAPS "+c+")")
			}
		}
	}
	sort.Strings(offenders)

	if len(offenders) > 0 {
		t.Fatalf("FINAL GATE FAILED: spec still has technical/drift schema names (%d) - alignment not complete:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}

	t.Logf("final gate: 0 technical names in spec (%d schemas; only exception - %s)",
		len(spec.Components.Schemas.Map()), allowed)
}

// isHealthMetaRoute — health/meta/docs endpoints outside /v1 (not part of the huma
// domains, absent from the aggregate spec). /docs/assets/* ends with /* → filtered
// out earlier as a wildcard.
func isHealthMetaRoute(r route) bool {
	switch r.path {
	case "/healthz", "/readyz", "/openapi.yaml", "/openapi.json", "/docs":
		return true
	}
	return false
}

// collectSchemaRefs recursively collects all string values of the "$ref" key in
// an arbitrary YAML tree (for the $ref integrity check of gate c).
func collectSchemaRefs(node any) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			for k, child := range v {
				if k == "$ref" {
					if s, ok := child.(string); ok {
						out[s] = struct{}{}
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(node)
	return out
}
