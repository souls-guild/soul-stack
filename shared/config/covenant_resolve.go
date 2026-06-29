package config

// Covenant FS-резолв (Resolved-слой). `*FromBytes` остаются NO-I/O: covenant.yml
// читается ТОЛЬКО здесь, по serviceRoot снапшота. Этот файл — единый источник
// правды о слиянии covenant-фрагмента в манифест для ВСЕХ потребителей (keeper
// runtime-load, trial-harness, soul-lint scenario-путь): раньше логика жила в
// keeper/internal/artifact, теперь — здесь, остальные зовут [ResolveScenarioCovenant].
//
// Почему не в schema/semantic-фазе `*FromBytes`: form-валидация (`form` ⊆ эффективный
// `input`) корректна лишь над СМЕРЖЕННЫМ input, а эффективный input существует только
// пост-merge (нужен ФС). Поэтому form covenant-сценария проверяется ЗДЕСЬ, после
// MergeCovenant, тем же ядром [validateFormAgainstInputKeys], что non-extends-путь
// гоняет в semantic-фазе.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// covenantFileExt — расширение файла covenant-фрагмента в корне service-репо.
// Имя файла = `<extends>.yml` (имя из `extends:` + это расширение), сиблинг
// types.yml/service.yml/scenario/.
const covenantFileExt = ".yml"

// ResolveScenarioCovenant сливает covenant-фрагмент в манифест сценария ПО МЕСТУ
// (MergeCovenant мутирует *m) и валидирует `form` против СМЕРЖЕННОГО input. Вызов —
// строго на манифесте, рождённом из байтов в этом же ходе load (а не на расшаренном
// кэш-объекте): MergeCovenant копирует input-схемы фрагмента по указателю, поэтому
// fragment обязан остаться read-only — он локален этому вызову и нигде не reuse-ится.
//
// No-op, когда `m.Extends` пуст (сценарий без наследования — forward-compat бит-в-бит,
// без ФС-обращения). Иначе: путь covenant.yml в КОРНЕ снапшота (`<serviceRoot>/
// <extends>.yml`) строится из имени extends, фрагмент читается securejoin-ридером
// (traversal-кламп: имя — single-segment kebab по [ValidExtendsName], securejoin
// клампит дополнительно), декодируется [LoadCovenantFragmentFromBytes] и сливается
// add-only. После merge — пост-merge form-валидация на смерженном `m.Input`.
//
// serviceRoot — абсолютный путь к корню снапшота сервиса (keeper: art.LocalDir;
// trial: корень тестового дерева; soul-lint: корень линтуемого репо). doc — Document,
// рождённый тем же `*FromBytes`-вызовом: из его AST берётся узел `form:` для пост-merge
// проверки (позиции диагностик указывают на реальные строки исходника). doc==nil
// ИЛИ отсутствие `form:` → пост-merge form-проверка пропускается (нечего проверять).
//
// Диагностики (все diag.LevelError, оператор чинит по одному):
//   - covenant_extends_invalid          — имя extends не прошло [ValidExtendsName];
//   - covenant_extends_target_not_found — covenant.yml по имени отсутствует;
//   - io_error                          — covenant.yml есть, но не читается;
//   - <fragment-diag>                   — сам covenant.yml невалиден (его decode/schema
//     ошибки прокинуты как есть, помечены File covenant-а);
//   - section_key_conflict              — один ключ секции в covenant И в сценарии
//     (add-only merge запрещает override);
//   - state_changes_form_mismatch       — covenant и сценарий объявили state_changes
//     в разных формах (list vs deprecated map);
//   - covenant_merge_failed             — прочие (не ожидаемые) ошибки merge;
//   - form_field_unknown/duplicate/…    — пост-merge form-проверка (см. ядро).
func ResolveScenarioCovenant(m *ScenarioManifest, doc *Document, serviceRoot string) []diag.Diagnostic {
	if m == nil || m.Extends == "" {
		return nil
	}
	name := m.Extends
	scenarioPath := docPath(doc)

	if !ValidExtendsName(name) {
		return []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			File: scenarioPath, Code: "covenant_extends_invalid",
			Message: fmt.Sprintf("extends: %q — недопустимое имя covenant-фрагмента", name),
			Hint:    "single-segment kebab-case (^[a-z][a-z0-9-]*$), без разделителей пути",
		}}
	}

	covenantFile := name + covenantFileExt
	data, err := readCovenantFile(serviceRoot, covenantFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []diag.Diagnostic{{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				File: scenarioPath, Code: "covenant_extends_target_not_found",
				Message: fmt.Sprintf("extends: %q — covenant-файл %s в корне сервиса не найден", name, covenantFile),
				Hint:    "covenant.yml-семейство (<extends>.yml) лежит в корне service-репо, сиблинг service.yml/types.yml",
			}}
		}
		return []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File: covenantFile, Code: "io_error", Message: err.Error(),
			Hint: "covenant-файл присутствует, но не читается — extends не резолвится",
		}}
	}

	// СВЕЖИЙ fragment на каждый scenario-резолв (не кэшируется между сценариями):
	// MergeCovenant копирует input-схемы фрагмента по УКАЗАТЕЛЮ, потому fragment
	// обязан остаться READ-ONLY после merge — гарантировано тем, что он локален
	// этому вызову.
	fragment, _, fdiags := LoadCovenantFragmentFromBytes(covenantFile, data, ValidateOptions{})
	if diag.HasErrors(fdiags) {
		// covenant.yml невалиден: прокидываем его собственные ошибки как есть
		// (File уже = covenantFile из decode), merge не делаем (фрагмент битый).
		return fdiags
	}

	// Cross-form state_changes: MergeCovenant при несовпадении IsList не детектит
	// конфликты по `set <поле>` (берёт форму local). Смешение list↔map — разные
	// грамматики; отвергаем явно ДО merge, иначе covenant-set-ы другой формы молча
	// потерялись бы.
	if fragment.StateChanges != nil && m.StateChanges != nil &&
		fragment.StateChanges.IsList != m.StateChanges.IsList {
		return append(fdiags, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			File: scenarioPath, Code: "state_changes_form_mismatch",
			Message: fmt.Sprintf("extends: %q — covenant и сценарий объявили state_changes в разных формах (list vs map)", name),
			Hint:    "приведите обе стороны к list-форме state_changes (map-форма deprecated)",
		})
	}

	if err := MergeCovenant(*fragment, m); err != nil {
		var conflict *SectionKeyConflict
		if errors.As(err, &conflict) {
			return append(fdiags, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				File: scenarioPath, Code: conflict.Code(),
				Message: fmt.Sprintf("extends: %q — секция %s.%s объявлена и в covenant, и в сценарии (add-only merge запрещает override)",
					name, conflict.Section, conflict.Key),
				Hint: "уберите дубль ключа из одной из сторон — covenant задаёт общий контракт, сценарий добавляет дельту",
			})
		}
		// Прочие ошибки merge (не ожидаются для уже провалидированных секций) —
		// прокидываем generic-диагностикой, не теряя их.
		return append(fdiags, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			File: scenarioPath, Code: "covenant_merge_failed",
			Message: fmt.Sprintf("extends: %q — слияние covenant не удалось: %s", name, err.Error()),
		})
	}

	// ПОСТ-MERGE form-валидация: `form` ⊆ ЭФФЕКТИВНЫЙ (смерженный) `input`. До merge
	// этого нельзя — covenant-поля отсутствовали бы в m.Input (ложный form_field_unknown);
	// потому form covenant-сценария гейтнут из semantic-фазы (scenario.go) и проверяется
	// здесь. Тем же ядром, что non-extends-путь, на том же AST (узел form: из doc).
	fdiags = append(fdiags, resolveCovenantFormDiags(m, doc, scenarioPath)...)
	return fdiags
}

// resolveCovenantFormDiags гоняет пост-merge form-проверку covenant-сценария, если
// в манифесте объявлен `form:`. Источник input-ключей — СМЕРЖЕННЫЙ m.Input; AST
// узла form: — из doc. Нет doc / нет ключа form: → ноль диагностик (нечего проверять,
// forward-compat бит-в-бит). Диагностикам без File проставляем путь сценария.
func resolveCovenantFormDiags(m *ScenarioManifest, doc *Document, scenarioPath string) []diag.Diagnostic {
	root := rootMapping(doc)
	if root == nil || !topLevelKeys(root)["form"] {
		return nil
	}
	inputKeys := make(map[string]bool, len(m.Input))
	for k := range m.Input {
		inputKeys[k] = true
	}
	out := validateFormAgainstInputKeys(root, inputKeys, "$.form")
	for i := range out {
		if out[i].File == "" {
			out[i].File = scenarioPath
		}
	}
	return out
}

// readCovenantFile читает covenant.yml из снапшота serviceRoot по имени файла
// (`<extends>.yml`). securejoin клампит выход за пределы serviceRoot (defence-in-
// depth поверх грамматики имени covenant). Возвращает fs.ErrNotExist прозрачно —
// caller различает «covenant не найден» от прочих I/O-ошибок.
func readCovenantFile(serviceRoot, name string) ([]byte, error) {
	// securejoin требует корень без `..`-компонент: caller-ы (trial/soul-lint) могут
	// передать относительный serviceRoot (`../examples/...`) — приводим к абсолютному.
	// Кламп выхода за serviceRoot это НЕ ослабляет (имя covenant — single-segment
	// kebab, securejoin клампит дополнительно).
	if abs, aerr := filepath.Abs(serviceRoot); aerr == nil {
		serviceRoot = abs
	}
	full, err := securejoin.SecureJoin(serviceRoot, name)
	if err != nil {
		return nil, fmt.Errorf("config: небезопасный путь covenant %q: %w", name, err)
	}
	// os.ReadFile оборачивает отсутствие файла в *PathError с fs.ErrNotExist —
	// errors.Is у caller ловит его, отличая «covenant нет» от прочих I/O-ошибок.
	return os.ReadFile(full)
}

// rootMapping достаёт корневой mapping-узел AST из opaque Document (пакет-приватный
// доступ к doc.file). nil-безопасен: nil doc / пустой/не-mapping корень → nil
// (caller трактует как «AST недоступен», пост-merge form-проверка пропускается).
func rootMapping(doc *Document) *ast.MappingNode {
	if doc == nil || doc.file == nil || len(doc.file.Docs) == 0 {
		return nil
	}
	body := doc.file.Docs[0].Body
	if mm, ok := body.(*ast.MappingNode); ok {
		return mm
	}
	return nil
}

// docPath — путь файла, ассоциированный с Document (для метки File-диагностик).
// nil-безопасен (nil doc → "").
func docPath(doc *Document) string {
	if doc == nil {
		return ""
	}
	return doc.path
}
