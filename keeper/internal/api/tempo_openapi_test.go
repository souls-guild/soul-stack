// Guard that the OpenAPI spec documents Tempo-429 (ADR-050(d)/S-R4) on both
// resolver-heavy write paths, each under ITS OWN bucket (create→voyage_create,
// preview→voyage_preview, ADR-050 amendment 2026-06-17): `POST /v1/voyages`
// (create) and `POST /v1/voyages/preview` (ADR-043 amendment §4). Source — the huma
// aggregator in code (HumaFullSpecYAML), like the served /openapi.yaml: the hand-written spec is gone.
//
// ★ huma 429 form: huma emits the response INLINE on the operation (content
// application/problem+json + description), NOT via a reusable
// `$ref #/components/responses/Problem429` and without a separate Retry-After header node
// in the spec (the header is set at runtime by the RateLimit middleware, not from an annotation).
// So the gate checks exactly the PRESENCE of a 429 response on both paths — the invariant
// "Tempo-429 is documented" in the form the source code describes it.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

func TestOpenAPI_VoyageWrite_Has429Tempo(t *testing.T) {
	dump, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}

	// Keep paths raw (yaml.Node) so a strict type doesn't fail on
	// path-item siblings like `parameters:` (seq) on other paths.
	var doc struct {
		Paths map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal([]byte(dump), &doc); err != nil {
		t.Fatalf("parsing huma spec: %v", err)
	}

	// Both paths are under Tempo, each under its own bucket (create→voyage_create,
	// preview→voyage_preview, ADR-050 amendment), and must declare 429.
	for _, path := range []string{"/v1/voyages", "/v1/voyages/preview"} {
		pathNode, ok := doc.Paths[path]
		if !ok {
			t.Fatalf("spec has no path %s", path)
		}
		var pathItem map[string]yaml.Node
		if err := pathNode.Decode(&pathItem); err != nil {
			t.Fatalf("decode path-item %s: %v", path, err)
		}
		postNode, ok := pathItem["post"]
		if !ok {
			t.Fatalf("spec has no POST operation %s", path)
		}
		var post struct {
			Responses map[string]struct {
				Description string `yaml:"description"`
			} `yaml:"responses"`
		}
		if err := postNode.Decode(&post); err != nil {
			t.Fatalf("decode POST %s: %v", path, err)
		}
		if _, ok := post.Responses["429"]; !ok {
			t.Errorf("POST %s does not declare 429 response (Tempo, ADR-050(d))", path)
		}
	}
}
