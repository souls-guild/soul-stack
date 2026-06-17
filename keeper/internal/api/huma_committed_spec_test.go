package api

// Committed-снимок OpenAPI-спеки для UI-vendor + git-ревью. Источник правды —
// huma-агрегатор ([HumaFullSpecYAML]); docs/keeper/openapi.yaml — его ПРОИЗВОДНЫЙ
// дамп (НЕ рукопись). Два режима, симметрично protoc-gen-drift:
//
//   - make gen-openapi (GEN_OPENAPI=1): перезаписать committed-файл текущим huma-дампом.
//   - make check (по умолчанию): TestCommittedOpenAPI_NoDrift сверяет committed-файл
//     с huma-дампом байт-в-байт; расхождение = «забыли make gen-openapi после правки
//     huma-домена».
//
// Путь к committed-файлу — относительно директории пакета (keeper/internal/api):
// docs/ лежит в корне репо, отсюда ../../../docs/keeper/openapi.yaml.

import (
	"os"
	"path/filepath"
	"testing"
)

// committedOpenAPIPath — committed-снимок относительно директории пакета.
var committedOpenAPIPath = filepath.Join("..", "..", "..", "docs", "keeper", "openapi.yaml")

// TestCommittedOpenAPI_NoDrift — drift-guard: committed docs/keeper/openapi.yaml
// должен байт-в-байт совпасть с текущим huma-дампом. При GEN_OPENAPI=1 тест вместо
// сверки ПЕРЕЗАПИСЫВАЕТ файл (механизм make gen-openapi).
//
// Если source-tree недоступен (custom-сборка без docs/) — drift-проверка
// пропускается с t.Skip, как и прежний meta-drift-guard.
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
