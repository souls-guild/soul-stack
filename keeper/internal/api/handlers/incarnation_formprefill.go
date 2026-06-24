package handlers

// Form-prefill-handler Operator API (`POST /v1/incarnations/{name}/scenarios/
// {scenario}/form-prefill`) — day-2 pre-fill UI-формы сценария ТЕКУЩИМИ
// значениями incarnation.state (docs/input.md → «Pre-fill из state»).
//
// Поля схемы сценария, объявившие `prefill_from_state: state.<path>`, в форме
// должны открываться не пустыми, а с текущим значением соответствующего
// state-поля (оператор правит дельту, не вводит всё заново). Этот эндпоинт —
// единственный резолвер таких prefill-hint-ов: читает state ОДНОЙ инкарнации
// {name} и отдаёт `{values: {field: current-value}}`.
//
// Инварианты безопасности (blocker):
//   - path-whitelist: резолвятся СТРОГО пути, объявленные `prefill_from_state` в
//     схеме сценария. Клиент путь НЕ передаёт — backend читает схему сценария,
//     строит множество объявленных путей и резолвит только их. Произвольный
//     state-доступ через эндпоинт невозможен;
//   - secret-исключение: поля, помеченные secret-схемой сервиса
//     (secretSchemaForIncarnation), из ответа ИСКЛЮЧАЮТСЯ полностью (pre-fill
//     маски бесполезен), а оставшиеся значения дополнительно прогоняются через
//     maskWithSchema (defense-in-depth: nested secret внутри prefill-значения
//     гасится тем же декларативным слоем ADR-010 §7.4).
//
// RBAC — incarnation.get (read одной инкарнации: кто видит инкарнацию, тот и
// получает prefill её формы). Новая permission НЕ заводится, scope-селектор —
// тот же inScope, что у GetTyped/HistoryTyped (ADR-047). Read-only, без audit.

import (
	"context"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/config"
)

// FormPrefillResult — NATIVE результат POST .../form-prefill (handler-native).
// Values — карта `field → current-value`: только поля схемы сценария, у которых
// объявлен `prefill_from_state` И путь покрыт текущим incarnation.state И поле
// НЕ secret. Поля с непокрытым путём / secret опускаются. Non-nil (пустая карта
// при отсутствии prefill-полей). Пакет api проецирует в native reply-DTO.
type FormPrefillResult struct {
	Values map[string]any
}

// FormPrefillTyped — доменная функция POST /v1/incarnations/{name}/scenarios/
// {scenario}/form-prefill (READ, без audit). inScope — RBAC scope-предикат
// (ADR-047, action=get): вне scope → 404 (как GetTyped, не палим чужую
// инкарнацию). ref — опц. override версии сервиса (схема той же версии, что
// форма); "" → ServiceVersion инкарнации.
//
// Ошибки — *problemError (422 невалидный path-сегмент / 404 нет инкарнации или
// вне scope / 500 сбой резолва сервиса). Best-effort по схеме: нет loader-а /
// сервис не зарегистрирован / scenario не парсится → пустой values (форма
// откроется без prefill, не 500 — prefill необязателен).
func (h *IncarnationHandler) FormPrefillTyped(ctx context.Context, name, scenarioName, ref string, inScope func(*incarnation.Incarnation) bool) (FormPrefillResult, error) {
	zero := FormPrefillResult{Values: map[string]any{}}

	if !incarnation.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'name' must match "+incarnation.NamePattern)}
	}
	if !scenario.ValidScenarioName(scenarioName) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'scenario' must match "+scenario.ScenarioNamePattern)}
	}

	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "incarnation "+name+" not found")}
		}
		h.logger.Error("incarnation.form-prefill: select failed", slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "select incarnation failed")}
	}
	if inScope == nil || !inScope(inc) {
		// Вне scope — 404 (parity GetTyped: не раскрываем существование чужой
		// инкарнации различием 403/404).
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "incarnation "+name+" not found")}
	}

	// path-whitelist: множество объявленных в схеме сценария prefill-путей.
	// Клиент путь не передаёт — берём из схемы ТОЙ версии, что форма (ref override
	// → ServiceVersion инкарнации). Best-effort: схема недоступна → пустой values.
	fields := h.prefillFieldsForScenario(ctx, inc, scenarioName, ref)
	if len(fields) == 0 {
		return zero, nil
	}

	// secret-исключение (blocker): secret-поля исключаются из prefill полностью.
	// Схема сервиса (state + create-input) материализуется единожды; nil →
	// деградация к vault+regex defense-in-depth ниже (maskWithSchema).
	secretSchema := h.secretSchemaForIncarnation(ctx, inc)

	values := make(map[string]any, len(fields))
	for field, path := range fields {
		val, ok := resolveStatePath(inc.State, path)
		if !ok {
			continue // путь не покрыт текущим state — поле опускается.
		}
		// secret-поле целиком исключаем (pre-fill маски бессмыслен). Пути
		// state-секретов в secretSchemaForIncarnation хранятся ОТНОСИТЕЛЬНО корня
		// state (collectStateSchemaSecrets: `admin_token`, `tls.key`) — без
		// `state.`-префикса; сверяем tail-форму пути.
		if secretSchema != nil && secretSchema.IsSecret(statePathTail(path)) {
			continue
		}
		values[field] = val
	}

	// defense-in-depth (ADR-010 §7.4): nested secret внутри prefill-значения
	// (map/list с secret-leaf) гасится декларативным+vault+regex-слоем, как
	// read-path GET incarnation. Top-level secret-поля уже исключены выше; здесь
	// страхуемся от вложенных секретов в составном значении. Замаскированный
	// leaf остаётся в форме плейсхолдером — лучше маска, чем утечка.
	masked := maskWithSchema(values, secretSchema)
	return FormPrefillResult{Values: masked}, nil
}

// prefillFieldsForScenario строит множество объявленных prefill-полей схемы
// сценария: `field → state.<path>` (path-whitelist). Материализует снапшот
// сервиса на версии инкарнации (или ref-override), читает `scenario/<name>/
// main.yml`, парсит input-схему и собирает поля с непустым `prefill_from_state`.
// Best-effort (parity collectCreateInputSecrets): любой сбой → nil (форма без
// prefill, не ошибка). state-предикат-доступ к произвольным путям невозможен —
// только объявленное автором схемы.
func (h *IncarnationHandler) prefillFieldsForScenario(ctx context.Context, inc *incarnation.Incarnation, scenarioName, ref string) map[string]string {
	if h.loader == nil || h.services == nil || inc == nil {
		return nil
	}
	serviceRef, ok := h.services.Resolve(inc.Service)
	if !ok {
		return nil
	}
	if ref != "" {
		// Явный override версии сервиса (форма строилась по той же).
		serviceRef.Ref = ref
	} else if inc.ServiceVersion != "" {
		// По умолчанию — версия, на которой инкарнация создана/мигрирована
		// (та же схема, что отдал ListScenarios форме).
		serviceRef.Ref = inc.ServiceVersion
	}
	art, err := h.loader.Load(ctx, serviceRef)
	if err != nil || art == nil {
		return nil
	}
	data, err := h.loader.ReadFile(art, "scenario/"+scenarioName+"/main.yml")
	if err != nil || len(data) == 0 {
		return nil
	}
	scn, _, _, perr := config.LoadScenarioManifestFromBytes("scenario/"+scenarioName+"/main.yml", data, config.ValidateOptions{})
	if perr != nil || scn == nil {
		return nil
	}
	var out map[string]string
	for fieldName, s := range scn.Input {
		if s == nil || s.PrefillFromState == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[fieldName] = s.PrefillFromState
	}
	return out
}

// resolveStatePath навигирует incarnation.state по dot-пути `state.<seg>[.<seg>…]`
// (форма провалидирована схемой rePrefillFromStatePath). Возвращает (значение,
// найдено). Промежуточный не-map сегмент / отсутствие ключа → (nil, false) —
// fail-closed (поле опускается из prefill, parity statepredicate no-such-key).
// Корневой токен `state` отбрасывается (путь стартует в самой incarnation.state).
func resolveStatePath(state map[string]any, path string) (any, bool) {
	segs := statePathSegments(path)
	if len(segs) == 0 {
		return nil, false
	}
	cur := state
	for i, seg := range segs {
		v, ok := cur[seg]
		if !ok {
			return nil, false
		}
		if i == len(segs)-1 {
			return v, true
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return nil, false
}

// statePathSegments разбивает `state.<seg>[.<seg>…]` в список сегментов БЕЗ
// корневого `state` (путь адресует поля внутри самой incarnation.state).
func statePathSegments(path string) []string {
	tail := statePathTail(path)
	if tail == "" {
		return nil
	}
	return splitDot(tail)
}

// statePathTail отбрасывает корневой `state.` префикс, возвращая адрес внутри
// state (`state.redis_users` → `redis_users`). path провалидирован схемой как
// `state.<...>`, поэтому префикс всегда присутствует; defensive: без префикса —
// пустая строка (resolveStatePath вернёт not-found, поле опустится).
func statePathTail(path string) string {
	const root = "state."
	if len(path) <= len(root) || path[:len(root)] != root {
		return ""
	}
	return path[len(root):]
}

// splitDot — дешёвый split по `.` без аллокации regexp (сегменты уже
// провалидированы snake_case-формой схемой).
func splitDot(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
