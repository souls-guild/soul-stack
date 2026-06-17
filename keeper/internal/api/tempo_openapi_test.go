// Guard, что OpenAPI-спека документирует Tempo-429 (ADR-050(d)/S-R4) на обоих
// resolver-тяжёлых write-путях, каждый под СВОИМ bucket-ом (create→voyage_create,
// preview→voyage_preview, ADR-050 amendment 2026-06-17): `POST /v1/voyages`
// (create) и `POST /v1/voyages/preview` (ADR-043 amendment §4). Источник — huma-
// агрегатор в коде (HumaFullSpecYAML), как и served /openapi.yaml: рукописи больше нет.
//
// ★ huma-форма 429: huma эмитит ответ INLINE на операции (content
// application/problem+json + description), а НЕ через reusable
// `$ref #/components/responses/Problem429` и без отдельного Retry-After-header-узла
// в спеке (header выставляется рантаймом RateLimit-middleware, не из аннотации).
// Поэтому гейт проверяет именно НАЛИЧИЕ ответа 429 на обоих путях — инвариант
// «Tempo-429 задокументирован» в той форме, в которой его описывает источник-код.
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

	// paths оставляем сырыми (yaml.Node), чтобы строгий тип не падал на
	// path-item-сиблингах вроде `parameters:` (seq) у других путей.
	var doc struct {
		Paths map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal([]byte(dump), &doc); err != nil {
		t.Fatalf("разбор huma-спеки: %v", err)
	}

	// Оба пути под Tempo, каждый под своим bucket-ом (create→voyage_create,
	// preview→voyage_preview, ADR-050 amendment), обязаны объявлять 429.
	for _, path := range []string{"/v1/voyages", "/v1/voyages/preview"} {
		pathNode, ok := doc.Paths[path]
		if !ok {
			t.Fatalf("в спеке нет пути %s", path)
		}
		var pathItem map[string]yaml.Node
		if err := pathNode.Decode(&pathItem); err != nil {
			t.Fatalf("decode path-item %s: %v", path, err)
		}
		postNode, ok := pathItem["post"]
		if !ok {
			t.Fatalf("в спеке нет операции POST %s", path)
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
			t.Errorf("POST %s не объявляет ответ 429 (Tempo, ADR-050(d))", path)
		}
	}
}
