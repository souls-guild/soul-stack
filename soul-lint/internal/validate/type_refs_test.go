package validate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Офлайн-проверка $type-ссылок против каталога типов сервиса. Фикстура — мини-
// сервис на диске: <root>/types.yml + <root>/scenario/<name>/main.yml. Линтуем
// main.yml, проверяем, что unknown/cycle/duplicate из каталога и резолва ловятся.

// writeMiniService раскладывает мини-сервис: types.yml в корне + scenario/<name>/
// main.yml. typesYAML=="" → types.yml не создаётся (каталог отсутствует).
// Возвращает путь к main.yml.
func writeMiniService(t *testing.T, typesYAML, scenarioName, mainYAML string) string {
	t.Helper()
	root := t.TempDir()
	if typesYAML != "" {
		if err := os.WriteFile(filepath.Join(root, "types.yml"), []byte(typesYAML), 0o600); err != nil {
			t.Fatalf("write types.yml: %v", err)
		}
	}
	scnDir := filepath.Join(root, "scenario", scenarioName)
	if err := os.MkdirAll(scnDir, 0o755); err != nil {
		t.Fatalf("mkdir scenario: %v", err)
	}
	mainPath := filepath.Join(scnDir, "main.yml")
	if err := os.WriteFile(mainPath, []byte(mainYAML), 0o600); err != nil {
		t.Fatalf("write main.yml: %v", err)
	}
	return mainPath
}

// runJSON прогоняет линт сценария в JSON-режиме, возвращает диагностики.
func runJSON(t *testing.T, mainPath string) []diag.Diagnostic {
	t.Helper()
	var out, errOut bytes.Buffer
	Run(Options{Path: mainPath, Kind: KindScenario, JSON: true}, &out, &errOut)
	var diags []diag.Diagnostic
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	for dec.More() {
		var d diag.Diagnostic
		if err := dec.Decode(&d); err != nil {
			t.Fatalf("decode diag: %v\nstdout: %s\nstderr: %s", err, out.String(), errOut.String())
		}
		diags = append(diags, d)
	}
	return diags
}

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// Валидная ссылка на существующий тип — без ошибок типов.
func TestTypeRefs_ValidRef_OK(t *testing.T) {
	types := `types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
`
	main := `name: deploy
input:
  target:
    $type: Endpoint
tasks: []
`
	p := writeMiniService(t, types, "deploy", main)
	diags := runJSON(t, p)
	for _, c := range []string{"input_type_unknown", "input_type_cycle", "input_type_duplicate", "input_type_ref_conflict"} {
		if hasCode(diags, c) {
			t.Fatalf("валидная ссылка не должна давать %s: %+v", c, diags)
		}
	}
}

// Ссылка на отсутствующий тип → input_type_unknown.
func TestTypeRefs_Unknown(t *testing.T) {
	types := `types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
`
	main := `name: deploy
input:
  target:
    $type: Ghost
tasks: []
`
	p := writeMiniService(t, types, "deploy", main)
	diags := runJSON(t, p)
	if !hasCode(diags, "input_type_unknown") {
		t.Fatalf("ссылка на отсутствующий тип → input_type_unknown: %+v", diags)
	}
}

// Цикл между типами в каталоге → input_type_cycle (НЕ зависание линтера).
func TestTypeRefs_CatalogCycle(t *testing.T) {
	types := `types:
  A:
    type: object
    properties:
      b:
        $type: B
  B:
    type: object
    properties:
      a:
        $type: A
`
	main := `name: deploy
input:
  target:
    $type: A
tasks: []
`
	p := writeMiniService(t, types, "deploy", main)
	diags := runJSON(t, p)
	if !hasCode(diags, "input_type_cycle") {
		t.Fatalf("цикл между типами → input_type_cycle: %+v", diags)
	}
}

// Дубликат имени типа в каталоге → input_type_duplicate.
func TestTypeRefs_Duplicate(t *testing.T) {
	types := `types:
  Dup:
    type: string
  Dup:
    type: integer
`
	main := `name: deploy
input:
  target:
    $type: Dup
tasks: []
`
	p := writeMiniService(t, types, "deploy", main)
	diags := runJSON(t, p)
	if !hasCode(diags, "input_type_duplicate") {
		t.Fatalf("дубликат типа → input_type_duplicate: %+v", diags)
	}
}

// $type + inline type на узле → input_type_ref_conflict (ловит парс сценария,
// каталог не нужен).
func TestTypeRefs_Conflict(t *testing.T) {
	main := `name: deploy
input:
  target:
    $type: Endpoint
    type: object
tasks: []
`
	p := writeMiniService(t, "", "deploy", main)
	diags := runJSON(t, p)
	if !hasCode(diags, "input_type_ref_conflict") {
		t.Fatalf("$type + type: → input_type_ref_conflict: %+v", diags)
	}
}

// Back-compat: сценарий без $type не требует types.yml и не ломается.
func TestTypeRefs_NoRef_BackCompat(t *testing.T) {
	main := `name: deploy
input:
  port:
    type: integer
tasks: []
`
	p := writeMiniService(t, "", "deploy", main)
	diags := runJSON(t, p)
	for _, c := range []string{"input_type_unknown", "input_type_cycle", "io_error"} {
		if hasCode(diags, c) {
			t.Fatalf("сценарий без $type не должен давать %s: %+v", c, diags)
		}
	}
}
