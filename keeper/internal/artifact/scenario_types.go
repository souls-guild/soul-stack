package artifact

import (
	"errors"
	"io/fs"
	"log/slog"

	yaml "gopkg.in/yaml.v3"
)

// typesCatalogFile — каталог переиспространяемых именованных типов в корне
// Service-репо (`types.yml`), сиблинг service.yml/scenario/. Параллель
// scenarioMainFile/serviceManifestFile.
const typesCatalogFile = "types.yml"

// typesSectionKey — единственный top-level ключ в types.yml.
const typesSectionKey = "types"

// typeRefKey — ключ-дискриминатор ссылки на именованный тип в input-DSL.
const typeRefKey = "$type"

// typeAnnotationKey — forward-compat аннотация, которую backend кладёт рядом с
// резолвнутым узлом: `x-type: <Имя>` — оригинальное имя типа, чтобы UI отрисовал
// виджет/подпись «это значение типа X». БЕЗ резолва UI получил бы сырой `$type`
// и сломался бы молча — поэтому резолв строго backend-side ДО проекции.
const typeAnnotationKey = "x-type"

// typeRefResolveDepthLimit — страховочный предел глубины подстановки (cycle-
// detection ловит патологию раньше; этот лимит — second line для runaway).
const typeRefResolveDepthLimit = 64

// typeCatalog — сырой (untyped) каталог типов: имя → тело схемы как map[string]any
// (форма из types.yml). DTO-сторона работает с raw-map InputSchema (UI рисует
// форму без серверной типизации), поэтому каталог тоже raw — резолв = подстановка
// тела типа на место узла-ссылки, аннотированная x-type. Cycle-detection — на
// прогоне резолва (loadTypeCatalog не разворачивает вложенность, только парсит).
type typeCatalog map[string]map[string]any

// loadTypeCatalog читает `<serviceRoot>/types.yml` и возвращает сырой каталог
// типов (имя → тело схемы). Отсутствие файла → пустой каталог без ошибки (типы
// опциональны). Невалидный YAML / неожиданная форма → warning в logger + пустой
// каталог (как partial-success ListScenarios: каталог не валит весь listing,
// просто $type-ссылки останутся неразрешёнными и UI это переживёт лучше, чем
// 500). Полную валидацию каталога (duplicate/cycle/unknown) делает soul-lint и
// render-pipeline — здесь best-effort проекция для UI.
func loadTypeCatalog(serviceRoot string, logger *slog.Logger) typeCatalog {
	data, err := readSnapshotFile(serviceRoot, typesCatalogFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("artifact: types.yml пропущен — ошибка чтения",
				slog.Any("error", err))
		}
		return typeCatalog{}
	}

	var raw struct {
		Types map[string]map[string]any `yaml:"types"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		logger.Warn("artifact: types.yml пропущен — невалидный YAML",
			slog.Any("error", err))
		return typeCatalog{}
	}
	if raw.Types == nil {
		return typeCatalog{}
	}
	return typeCatalog(raw.Types)
}

// resolveScenarioTypeRefs резолвит `$type`-ссылки в raw-map InputSchema сценария
// по каталогу типов: каждый узел `{$type: T}` заменяется телом типа T из
// каталога + аннотацией `x-type: T`. Резолв рекурсивен (items/properties/
// additional_properties) и cycle-safe: повторный заход в тип на текущей ветке
// обхода обрывается (узел остаётся как есть с `$type` — UI это переживёт; полную
// ошибку cycle поднимает soul-lint/render). Возвращает НОВУЮ map (исходник не
// мутируется). nil-каталог / nil-схема → схема как есть.
func resolveScenarioTypeRefs(schema map[string]any, catalog typeCatalog) map[string]any {
	if schema == nil {
		return nil
	}
	out := make(map[string]any, len(schema))
	for name, node := range schema {
		out[name] = resolveTypeNode(node, catalog, map[string]bool{}, 0)
	}
	return out
}

// resolveTypeNode резолвит один input-узел. `stack` — множество имён типов на
// текущей ветке обхода (cycle-detection). `depth` — страховка от runaway.
func resolveTypeNode(node any, catalog typeCatalog, stack map[string]bool, depth int) any {
	m, ok := node.(map[string]any)
	if !ok || depth > typeRefResolveDepthLimit {
		return node
	}

	// Узел-ссылка: подставляем тело типа + аннотация x-type.
	if ref, isRef := stringValue(m[typeRefKey]); isRef {
		if stack[ref] {
			// Цикл — оставляем узел как есть (best-effort; soul-lint поднимет
			// input_type_cycle). Не уходим в бесконечную рекурсию.
			return cloneNode(m)
		}
		body, found := catalog[ref]
		if !found {
			// Неизвестный тип — узел как есть (soul-lint поднимет input_type_unknown).
			return cloneNode(m)
		}
		stack[ref] = true
		resolved := resolveTypeNode(cloneNode(body), catalog, stack, depth+1)
		delete(stack, ref)

		rm, _ := resolved.(map[string]any)
		if rm == nil {
			rm = map[string]any{}
		}
		// Аннотация имени типа для UI + presentational overlay узла-ссылки поверх
		// тела типа. field-level `required: <bool>` НЕ переносим: DTO-ключ `required`
		// уже занят object-level списком обязательных детей типа (массив) — плоский
		// контракт не выражает разом «поле обязательно» и «дети обязательны» одним
		// ключом (представленческий gap, NIM-72; контракт DTO не меняем). description/
		// required_when — отдельные ключи, безопасны, если тип их не задал.
		rm[typeAnnotationKey] = ref
		if d, ok := stringValue(m["description"]); ok && d != "" {
			rm["description"] = d
		}
		if _, taken := rm["required_when"]; !taken {
			if rw, ok := stringValue(m["required_when"]); ok && rw != "" {
				rm["required_when"] = rw
			}
		}
		return rm
	}

	// Обычный узел: рекурсия в items/properties/additional_properties.
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch k {
		case "items":
			out[k] = resolveTypeNode(v, catalog, stack, depth+1)
		case "properties", "additional_properties":
			if pm, ok := v.(map[string]any); ok {
				resolvedProps := make(map[string]any, len(pm))
				for pn, pv := range pm {
					resolvedProps[pn] = resolveTypeNode(pv, catalog, stack, depth+1)
				}
				out[k] = resolvedProps
			} else {
				out[k] = v
			}
		default:
			out[k] = v
		}
	}
	return out
}

// stringValue — безопасное извлечение строки из any.
func stringValue(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// cloneNode — глубокая копия raw-map узла (map/slice рекурсивно), чтобы резолв не
// мутировал каталог и общий тип, использованный дважды, не «портился» между
// потребителями. Скаляры копируются по значению.
func cloneNode(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = cloneNode(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = cloneNode(val)
		}
		return out
	default:
		return v
	}
}
