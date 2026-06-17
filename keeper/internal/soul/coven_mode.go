package soul

import "sort"

// CovenMode — режим применения Coven-метки(меток) к набору хоста. Единый
// словарь для обоих путей мутации coven: keeper-side core-модуль
// `core.soul.registered` (scenario-путь) и bulk-API `POST /v1/souls/coven`.
//
// Значения совпадают с docs/keeper/modules.md → семантика mode. Bulk-API
// в пилоте принимает только append/remove (одна метка за вызов);
// replace-набор — отдельный будущий слайс, но enum здесь полный, чтобы
// core.soul.registered и bulk делили один источник истины.
type CovenMode string

const (
	// CovenAppend — existing ∪ переданные (idempotent).
	CovenAppend CovenMode = "append"
	// CovenReplace — переданные набор целиком (footgun: пустой = ошибка на
	// границе caller-а, ApplyCovenMode её не форсит — это чистая функция).
	CovenReplace CovenMode = "replace"
	// CovenRemove — existing \ переданные.
	CovenRemove CovenMode = "remove"
)

// CovenLabelValidator — хук валидации назначаемой Coven-метки за пределами
// формата (под будущий справочник окружений Q1 — ADR-008 amend). Вызывается
// Service-слоем bulk coven-assign ДО UPDATE. В пилоте — [NoopCovenLabelValidator]
// (format-only: формат уже проверяет [ValidCoven]). Когда появится справочник —
// его реализация подменяется без изменения API.
type CovenLabelValidator interface {
	Validate(label string) error
}

// NoopCovenLabelValidator — пилотная no-op-имплементация [CovenLabelValidator].
// Формат метки проверяется отдельно [ValidCoven]; здесь — место под справочник.
type NoopCovenLabelValidator struct{}

// Validate всегда пропускает (format-only — формат уже проверен ValidCoven).
func (NoopCovenLabelValidator) Validate(string) error { return nil }

// ValidCovenMode — closed-enum проверка режима.
func ValidCovenMode(m CovenMode) bool {
	switch m {
	case CovenAppend, CovenReplace, CovenRemove:
		return true
	}
	return false
}

// ApplyCovenMode — чистая функция применения режима к существующему набору
// Coven-меток. Возвращает (final, removed):
//
//   - final   — итоговый набор, отсортированный и дедуплицированный;
//   - removed — метки, фактически убранные (только для CovenRemove;
//     отсортированы; nil для append/replace).
//
// Единый источник set-семантики для core.soul.registered (per-host) и
// bulk-API. Вынесен из coremod/soul/registered.go (рефактор ТЗ-пилота),
// чтобы оба пути не расходились в трактовке режимов. На неизвестный режим
// возвращает (existing, nil) — валидацию режима делает caller до вызова.
func ApplyCovenMode(existing, wanted []string, mode CovenMode) (final, removed []string) {
	switch mode {
	case CovenAppend:
		set := covenSetOf(existing)
		for _, c := range wanted {
			set[c] = struct{}{}
		}
		return covenSortedKeys(set), nil
	case CovenReplace:
		return covenUniqueSorted(wanted), nil
	case CovenRemove:
		set := covenSetOf(existing)
		var rem []string
		for _, c := range wanted {
			if _, has := set[c]; has {
				delete(set, c)
				rem = append(rem, c)
			}
		}
		sort.Strings(rem)
		return covenSortedKeys(set), rem
	}
	return existing, nil
}

// CovenSetEqual — порядок-независимое сравнение двух наборов Coven-меток
// (после дедупликации). Используется, чтобы избежать лишнего UPDATE и
// эфемерного changed=true, когда новый набор совпадает с текущим.
func CovenSetEqual(a, b []string) bool {
	sa, sb := covenSetOf(a), covenSetOf(b)
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if _, ok := sb[k]; !ok {
			return false
		}
	}
	return true
}

func covenSetOf(xs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		out[x] = struct{}{}
	}
	return out
}

func covenSortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func covenUniqueSorted(xs []string) []string {
	return covenSortedKeys(covenSetOf(xs))
}
