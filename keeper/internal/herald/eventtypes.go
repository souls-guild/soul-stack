package herald

import (
	"fmt"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// runScopeAreas — закрытый список областей событий ПРОГОНОВ, на которые
// допускается подписка Tiding в MVP (ADR-052(b), scope «ТОЛЬКО события
// прогонов»). area-glob (`scenario_run.*`) и точные типы (`command_run.invoked`)
// валидируются по этому множеству: beacon-события хостов (Portent/Oracle),
// CRUD-аудит реестров и прочий keeper-internal-журнал в scope НЕ входят.
//
// Закрытый список (не весь audit.EventType-каталог), потому что scope ADR-052(b)
// — это именно области прогона, а не «любой известный audit-тип». Расширение
// scope — amend ADR-052 + правка этого множества.
var runScopeAreas = map[string]struct{}{
	"scenario_run": {},
	"command_run":  {},
	"voyage":       {},
	"cadence":      {},
}

// runScopePointEvents — точечные event-types вне area-glob-областей, явно
// входящие в scope прогонов (ADR-052(b)). `incarnation.*` целиком в scope НЕ
// входит (область несёт CRUD/lifecycle-события инкарнации), но конкретные
// события прогона допускаются точечно:
//   - drift-чек — событие прогона Scry (ADR-031);
//   - run_completed — терминал scenario-run одной инкарнации (ADR-052 §k,
//     status ∈ {success, failed}): несущий слой T4a/T4b-подписок (алерт на
//     таску, уведомления регулярного запуска). Область incarnation.* при этом
//     остаётся вне scope (CRUD-шум), открыт только этот точечный тип.
var runScopePointEvents = map[string]struct{}{
	"incarnation.drift_checked":                {},
	string(audit.EventIncarnationRunCompleted): {},
}

// heraldArea — область собственных терминалов доставки (`herald.delivered` /
// `herald.failed`, ADR-052(d)). Вынесена в константу — единая точка для
// loop-guard-а dispatcher-а ([isHeraldOwnEvent]) и потенциального расширения.
const heraldArea = "herald"

// isHeraldOwnEvent — true, если событие принадлежит собственной области Herald-
// доставки (`herald.*`). Loop-guard: терминалы доставки сами идут через
// audit-writer → tap, поэтому подписка на них породила бы «уведомление об
// уведомлении» и потенциально бесконечную петлю при шторме.
//
// Двойная защита (defence in depth): подписка на `herald.*` уже невозможна на
// CRUD-этапе ([validateEventType] не пускает область вне runScopeAreas), но
// этот guard в dispatcher-е отсекает событие ДО матча правил даже если scope
// когда-нибудь расширят — петля не должна появляться от расширения подписок.
func isHeraldOwnEvent(et audit.EventType) bool {
	return eventArea(et) == heraldArea
}

// ValidateEventTypes проверяет список подписки Tiding (ADR-052(b)). Список
// обязан быть непустым (дублирует CHECK tidings_event_types_nonempty —
// defence in depth + дружелюбная ошибка до похода в БД). Каждый элемент —
// либо area-glob `<area>.*` с `<area>` из scope прогонов, либо точный
// `<area>.<action>` с тем же `<area>`, либо явно разрешённый точечный тип
// (`incarnation.drift_checked`). Всё прочее (произвольный wildcard `*`,
// неизвестная область, точечный тип вне scope) — отклоняется.
func ValidateEventTypes(eventTypes []string) error {
	if len(eventTypes) == 0 {
		return fmt.Errorf("herald: tiding event_types must be non-empty")
	}
	for _, et := range eventTypes {
		if err := validateEventType(et); err != nil {
			return err
		}
	}
	return nil
}

func validateEventType(et string) error {
	if et == "" {
		return fmt.Errorf("herald: empty event_type")
	}
	if et == "*" || strings.HasPrefix(et, "*") {
		return fmt.Errorf("herald: bare wildcard %q not allowed (use area-glob like scenario_run.*)", et)
	}

	dot := strings.IndexByte(et, '.')
	if dot <= 0 {
		return fmt.Errorf("herald: invalid event_type %q (expected <area>.<action> or <area>.*)", et)
	}
	area := et[:dot]
	rest := et[dot+1:]

	// area-glob `<area>.*` — область целиком (только scope прогонов).
	if rest == "*" {
		if _, ok := runScopeAreas[area]; !ok {
			return fmt.Errorf("herald: event_type area %q is out of run-scope (allowed: scenario_run/command_run/voyage/cadence)", area)
		}
		return nil
	}
	if strings.Contains(rest, "*") {
		return fmt.Errorf("herald: invalid event_type %q (wildcard only as whole-area suffix `.*`)", et)
	}

	// Точный тип: либо из scope-области, либо явно разрешённый точечный.
	if _, ok := runScopeAreas[area]; ok {
		return nil
	}
	if _, ok := runScopePointEvents[et]; ok {
		return nil
	}
	return fmt.Errorf("herald: event_type %q is out of run-scope (allowed areas: scenario_run/command_run/voyage/cadence; point: %s)", et, pointEventsList())
}

// pointEventsList — отсортированный список разрешённых точечных event-type-ов
// (runScopePointEvents) для текста ошибки. Map-driven, чтобы перечень в
// сообщении не расходился с фактически допустимыми типами при их расширении.
func pointEventsList() string {
	out := make([]string, 0, len(runScopePointEvents))
	for et := range runScopePointEvents {
		out = append(out, et)
	}
	sort.Strings(out)
	return strings.Join(out, "/")
}

// RunScopeAreas — отсортированный список имён областей прогона (без `.*`-суффикса),
// допустимых для area-glob-подписки Tiding (ADR-052(b)). Источник правды каталога
// `GET /v1/event-types` (EventTypeCatalogHandler) — тот же runScopeAreas, что
// валидирует CRUD, рассинхрон каталога и валидатора невозможен.
func RunScopeAreas() []string {
	out := make([]string, 0, len(runScopeAreas))
	for area := range runScopeAreas {
		out = append(out, area)
	}
	sort.Strings(out)
	return out
}

// RunScopePointEvents — отсортированный список точечных event-types вне area-glob
// (`incarnation.drift_checked`/`incarnation.run_completed`), допустимых для
// подписки Tiding целиком (ADR-052(b)). Источник правды каталога `GET /v1/event-types`.
func RunScopePointEvents() []string {
	out := make([]string, 0, len(runScopePointEvents))
	for et := range runScopePointEvents {
		out = append(out, et)
	}
	sort.Strings(out)
	return out
}
