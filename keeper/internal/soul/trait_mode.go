package soul

import (
	"fmt"
	"regexp"
	"sort"
)

// TraitMode — режим применения операторских trait-меток (ADR-060) к набору
// хостов через bulk-API `POST /v1/souls/traits`. Traits — это map (key → scalar
// | list-of-scalar), в отличие от плоского списка Coven-меток, поэтому словарь
// режимов адаптирован под map-семантику (Coven-аналог — [CovenMode]):
//
//   - merge   — set/overwrite переданные ключи, остальные сохранить;
//   - replace — заменить ВЕСЬ traits-map целиком (footgun: пустой = очистить);
//   - remove  — удалить переданные ключи (по списку имён).
type TraitMode string

const (
	// TraitMerge — existing-map ⊕ переданные ключи (overwrite по ключу,
	// остальные ключи сохранены). Дефолтный режим bulk-API.
	TraitMerge TraitMode = "merge"
	// TraitReplace — переданный map целиком (пустой = очистить все traits).
	TraitReplace TraitMode = "replace"
	// TraitRemove — existing-map без переданных ключей (по списку имён).
	TraitRemove TraitMode = "remove"
)

// ValidTraitMode — closed-enum проверка режима.
func ValidTraitMode(m TraitMode) bool {
	switch m {
	case TraitMerge, TraitReplace, TraitRemove:
		return true
	}
	return false
}

// TraitKeyPattern — форма ключа trait-метки: kebab-ИЛИ-snake-case (`-`/`_` как
// внутренние разделители). Отличие от [CovenPattern] (только `-`): trait-ключ —
// свободное имя атрибута оператора (`owner_team`), `_` разрешён (NIM-67, ADR-060
// «произвольные строковые ключи»). Грамматика = reScenarioName.
const TraitKeyPattern = `^[a-z][a-z0-9]*([_-][a-z0-9]+)*$`

var traitKeyRe = regexp.MustCompile(TraitKeyPattern)

const traitKeyMaxLen = covenMaxLen

// ValidTraitKey проверяет один ключ trait-метки (kebab/snake-case, 1..63 символа).
func ValidTraitKey(key string) bool {
	if len(key) == 0 || len(key) > traitKeyMaxLen {
		return false
	}
	return traitKeyRe.MatchString(key)
}

// ValidTraitValue проверяет значение trait-метки: допустим скаляр
// (string/number/bool) либо список скаляров (depth ≤ 1). Вложенные map и
// списки-в-списках отвергаются — trait-значение остаётся «плоским» (read/target
// пилот проецирует его в `soulprint.self.traits.<key>` для CEL-таргетинга, где
// вложенность не нужна и усложнила бы предикаты).
//
// JSON-декодер huma/encoding отдаёт числа как float64, поэтому числовой случай
// покрыт ветками float64/int/int64; nil-значение под ключом отвергается
// (двусмысленно с «удалить» — для удаления есть mode=remove).
func ValidTraitValue(v any) error {
	switch val := v.(type) {
	case string, bool, float64, int, int64:
		return nil
	case []any:
		for i, elem := range val {
			if !isScalar(elem) {
				return fmt.Errorf("soul: trait value list element %d must be scalar (string/number/bool), got %T", i, elem)
			}
		}
		return nil
	case nil:
		return fmt.Errorf("soul: trait value must not be null (use mode=remove to delete a key)")
	default:
		return fmt.Errorf("soul: trait value must be scalar or list of scalars, got %T (nested objects/arrays are not allowed)", v)
	}
}

// isScalar — допустимый элемент списка-значения (скаляр; вложенные list/map
// запрещены, depth ≤ 1).
func isScalar(v any) bool {
	switch v.(type) {
	case string, bool, float64, int, int64:
		return true
	}
	return false
}

// ValidateTraitDelta проверяет набор (ключ → значение) для mode=merge/replace:
// каждый ключ — [ValidTraitKey], каждое значение — [ValidTraitValue]. Возвращает
// первую найденную ошибку; nil-map допустим (для replace = «очистить»).
func ValidateTraitDelta(delta map[string]any) error {
	for _, key := range sortedTraitKeys(delta) {
		if !ValidTraitKey(key) {
			return fmt.Errorf("soul: invalid trait key %q (must match %s)", key, TraitKeyPattern)
		}
		if err := ValidTraitValue(delta[key]); err != nil {
			return err
		}
	}
	return nil
}

// ValidateTraitKeys проверяет список ключей для mode=remove: каждый —
// [ValidTraitKey]. Возвращает первую ошибку.
func ValidateTraitKeys(keys []string) error {
	for _, key := range keys {
		if !ValidTraitKey(key) {
			return fmt.Errorf("soul: invalid trait key %q (must match %s)", key, TraitKeyPattern)
		}
	}
	return nil
}

// sortedTraitKeys — детерминированный обход ключей map (стабильные сообщения об
// ошибках валидации).
func sortedTraitKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
