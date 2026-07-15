package api

// A committed snapshot of the OpenAPI spec for the UI vendor + git review. Source of truth —
// the huma aggregator ([HumaFullSpecYAML]); docs/keeper/openapi.yaml — its DERIVED
// dump (NOT hand-written). Two modes, symmetric with protoc-gen-drift:
//
//   - make gen-openapi (GEN_OPENAPI=1): overwrite the committed file with the current huma dump.
//   - make check (default): TestCommittedOpenAPI_NoDrift compares the committed file
//     with the huma dump byte-for-byte; a mismatch = "forgot make gen-openapi after editing
//     a huma domain".
//
// The path to the committed file is relative to the package directory (keeper/internal/api):
// docs/ is at the repo root, hence ../../../docs/keeper/openapi.yaml.

import (
	"os"
	"path/filepath"
	"testing"
)

// committedOpenAPIPath — the committed snapshot relative to the package directory.
var committedOpenAPIPath = filepath.Join("..", "..", "..", "docs", "keeper", "openapi.yaml")

// TestCommittedOpenAPI_NoDrift — drift-guard: committed docs/keeper/openapi.yaml
// must match the current huma dump byte-for-byte. With GEN_OPENAPI=1 the test, instead of
// comparing, OVERWRITES the file (the make gen-openapi mechanism).
//
// If the source tree is unavailable (a custom build without docs/) — the drift check
// is skipped with t.Skip, like the former meta-drift-guard.
func TestCommittedOpenAPI_NoDrift(t *testing.T) {
	dump, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}

	if os.Getenv("GEN_OPENAPI") != "" {
		if err := os.WriteFile(committedOpenAPIPath, []byte(dump), 0o644); err != nil {
			t.Fatalf("запись committed openapi.yaml (%s): %v", committedOpenAPIPath, err)
		}
		t.Logf("gen-openapi: записан huma-дамп → %s (%d байт)", committedOpenAPIPath, len(dump))
		return
	}

	committed, err := os.ReadFile(committedOpenAPIPath)
	if err != nil {
		t.Skipf("committed openapi.yaml недоступен (%v); drift-проверка пропущена", err)
	}
	if string(committed) != dump {
		t.Errorf("openapi.yaml drift: docs/keeper/openapi.yaml расходится с huma-дампом — " +
			"запустите `make gen-openapi` (committed-файл = производный huma-генерат, не рукопись)")
	}
}
