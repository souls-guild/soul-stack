package config

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Статпроверка ссылок `soulprint.<...>` внутри CEL-предикатов сценария
// (`where:`/`when:`/`changed_when:`/`failed_when:`/`until:`/`loop.when:`).
// Зеркальна validateTaskRefs для register.* — но schema-driven, не cross-task.
//
// Что проверяем:
//
//  1. soulprint_naked — голый `soulprint.<x>` без `.self`/`.hosts`/`.where(`
//     является ошибкой каноники ([docs/soul/soulprint.md]: «голая форма
//     soulprint.<path> без .self — ошибка валидации soul-lint»).
//  2. soulprint_unknown_path — `soulprint.self.<unknown>` (опечатка типа
//     `soulprint.self.os.familly`). Сверка идёт по [soulprintSelfTopLevel] +
//     [soulprintSelfSubPaths] (typed-схема ADR-018 + registry-проекции).
//     Динамический хвост за массивным/скалярным сегментом не валидируется
//     (interfaces[i].ipv4, .sid.startsWith(...) и т.п.) — линтер сознательно
//     грубый, ловит только опечатки в известных префиксах.
//
// Извлечение токенов делает [extractSoulprintRefs] — текстовое: содержимое
// строковых литералов CEL вырезается тем же celStringLiteral, что и для
// register. Это критично для предикатов формы `soulprint.hosts.where("role ==
// 'primary'")`: строковый CEL внутри `.where(...)` — это вложенный CEL,
// который интерпретатор парсит отдельно (см. shared/cel/hosts.go), и для
// поверхностной статпроверки мы НЕ хотим лезть в него (там `role`/`covens` —
// поля __host, не soulprint.self.*).
//
// Динамический доступ `soulprint["self"]["x"]` формой не покрывается (нет
// точечной записи) — безопасный пропуск, симметрично register-кейсу.

// reSoulprintCELRef извлекает первый/второй сегмент из `soulprint.<top>(.<sub>)?`.
// Граница слева — начало строки или не-id/dot-символ; `soulprint` обязан быть
// корневым идентификатором (`foo.soulprint.x` не матчит, `mysoulprint.x` тоже).
// Группа 2 — top-сегмент (`self`/`hosts`/`os`/`familly`/…); группа 3 — sub
// (опционально), используется при top == "self" для двухсегментной проверки.
// Скобка `(` за third-сегментом сознательно не матчит (метод-вызов на скаляре
// типа `.startsWith(...)` — это валидный паттерн).
var reSoulprintCELRef = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.])soulprint\.([a-z][a-z0-9_]*)(?:\.([a-z][a-z0-9_]*))?`,
)

// soulprintRef — извлечённая ссылка `soulprint.<top>(.<sub>)?` для статпроверки.
type soulprintRef struct {
	top string // "self" / "hosts" / "<typo>"
	sub string // пустой если не было второго сегмента
}

// extractSoulprintRefs возвращает отсортированный набор уникальных пар (top,sub)
// для `soulprint.<top>(.<sub>)?` в CEL-строке. Содержимое строковых литералов
// вырезается перед извлечением: вложенный CEL-аргумент `.where("role == 'x'")`
// валидируется внутри shared/cel.rewriteHostsWhere, а здесь это просто данные.
// Сортировка делает диагностики детерминированными.
func extractSoulprintRefs(expr string) []soulprintRef {
	stripped := celStringLiteral.ReplaceAllString(expr, `""`)
	seen := map[soulprintRef]struct{}{}
	for _, m := range reSoulprintCELRef.FindAllStringSubmatch(stripped, -1) {
		ref := soulprintRef{top: m[2], sub: m[3]}
		// Если top — scalar (sid/hostname/covens/role), сегмент после точки
		// — метод/индекс/динамический доступ; игнорируем sub, чтобы
		// `.sid.startsWith(...)` и `.covens.exists(...)` не флагались.
		if soulprintScalarTopLevel[ref.top] {
			ref.sub = ""
		}
		// Спец-аксессоры hosts/where (`soulprint.hosts.where(...)`,
		// `soulprint.where(...)`): sub не проверяем, эту валидацию делает
		// shared/cel.rewriteHostsWhere в render-фазе.
		if ref.top == "hosts" || ref.top == "where" {
			ref.sub = ""
		}
		seen[ref] = struct{}{}
	}
	out := make([]soulprintRef, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].top != out[j].top {
			return out[i].top < out[j].top
		}
		return out[i].sub < out[j].sub
	})
	return out
}

// checkSoulprintRefs проверяет soulprint-ссылки в одном CEL-предикате (one of:
// `when`/`changed_when`/`failed_when`/`where`/`retry.until`/`loop.when`).
//
// bool-литерал/null (force-shortcut changed_when:/failed_when:) — не CEL-строка,
// пропускается. Диагностика — на позиции value-ноды (точное смещение внутри
// строки не извлекается, симметрично checkPredicateRefs).
func checkSoulprintRefs(kind string, value ast.Node, taskPath string) []diag.Diagnostic {
	sn, ok := value.(*ast.StringNode)
	if !ok {
		return nil
	}
	rt := sn.GetToken()
	line, col := 0, 0
	if rt != nil {
		line, col = rt.Position.Line, rt.Position.Column
	}
	var out []diag.Diagnostic
	for _, ref := range extractSoulprintRefs(sn.Value) {
		// 1. Каноника: голая форма `soulprint.<X>` запрещена, если X — не
		// scenario-аксессор (`hosts`/`where`) и не `self`.
		switch ref.top {
		case "self":
			// Допустимая форма, проверяем sub ниже.
		case "hosts", "where":
			// scenario-only аксессоры (orchestration.md §4.1); семантическая
			// проверка (изоляция destiny) делается в shared/cel,
			// здесь только верифицируем что это известный top-сегмент.
			continue
		default:
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code: "soulprint_naked_reference",
				Message: fmt.Sprintf(
					"%s references soulprint.%s — bare soulprint.<path> without .self is not allowed (canonical form: soulprint.self.<path>)",
					kind, ref.top,
				),
				Hint:     "soulprint.self.<path> for current host facts; soulprint.hosts / soulprint.hosts.where(...) for scenario-only host listing (see docs/soul/soulprint.md)",
				YAMLPath: fmt.Sprintf("%s.%s", taskPath, kind),
			}))
			continue
		}

		// 2. Второй сегмент `soulprint.self.<sub>`: должен быть в whitelist
		// верхнеуровневых полей SoulprintFacts (ADR-018). Пустой sub — это
		// форма `soulprint.self` без точечного спуска (например, `has(...)`),
		// допустимая.
		if ref.sub == "" {
			continue
		}
		if !soulprintSelfTopLevel[ref.sub] {
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code: "soulprint_unknown_path",
				Message: fmt.Sprintf(
					"%s references soulprint.self.%s — unknown field in SoulprintFacts (ADR-018)",
					kind, ref.sub,
				),
				Hint:     soulprintSelfTopHint(),
				YAMLPath: fmt.Sprintf("%s.%s", taskPath, kind),
			}))
		}
	}
	return out
}

// soulprintSelfTopHint — детерминированный hint со списком валидных
// top-сегментов под soulprint.self.*. Собирается из soulprintSelfTopLevel один
// раз lazy-инициализацией.
var soulprintSelfTopHintCache string

func soulprintSelfTopHint() string {
	if soulprintSelfTopHintCache != "" {
		return soulprintSelfTopHintCache
	}
	keys := make([]string, 0, len(soulprintSelfTopLevel))
	for k := range soulprintSelfTopLevel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	soulprintSelfTopHintCache = "known top-level fields under soulprint.self.* (ADR-018): " + joinComma(keys)
	return soulprintSelfTopHintCache
}

// checkSoulprintSubPath — расширенная проверка под `soulprint.self.<msg>.<field>`,
// сейчас не вызывается (см. extractSoulprintRefs grouping). Зарезервирована для
// слайса, который захочет ловить `os.familly` (вторая опечатка). Пока линтер
// флагает только первый сегмент; второй (поле вложенного сообщения) — отложен:
// требует обработки method-call/индекс-форм, а также позиций смешанных хвостов
// `network.interfaces[i].ipv4`. Включается отдельным слайсом по запросу PM.
//
// Зачем этот placeholder: явно зафиксировать, где живёт расширение, чтобы
// будущий developer не дублировал extraction-логику.
func checkSoulprintSubPath(_ string, _ string) bool { return true }

// joinComma — детерминированная склейка ["a","b","c"] → "a, b, c" без зависимостей.
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
