package config

// roster-refresh passage-граница (ADR-0061 §S2, amends ADR-056).
//
// Зачем. Целевой сценарий ADR-0061 — единый create-прогон provision→онбординг→роль:
// шаг `core.cloud.provisioned` (keeper) создаёт N VM, шаг `core.soul.registered`
// (keeper) с `refresh_soulprint: true` регистрирует и дожидается их онбординга, а
// последующие задачи применяют роль к УЖЕ онбордившимся хостам через roster
// (`soulprint.hosts`, `on: [incarnation.name]`, `soulprint.self.*`). Roster прогона
// резолвится up-front перед первым Passage и стабилен В ПРЕДЕЛАХ Passage, но на
// refresh-границе пере-резолвится в свежий live-снимок online-набора (ADR-009 §7 в
// действующей редакции, ослаблено ADR-0061). Чтобы re-resolve (S3) проявился,
// потребители обновлённого roster ОБЯЗАНЫ оказаться в Passage СТРОГО ПОСЛЕ
// `refresh_soulprint`-шага — иначе их render (таргетинг + soulprint.hosts) увидел бы
// СТАРЫЙ (до-онбординга) roster.
//
// ★ БЛОКЕР (ADR-056 §риски, silent-wrong-target): без passage-границы redis-apply-
// шаг уехал бы в тот же Passage со старым (пустым) roster → разрушительная операция
// на неверном наборе хостов МОЛЧА. Поэтому `refresh_soulprint: true` — новый класс
// PASSAGE-ОПРЕДЕЛЯЮЩЕГО сигнала «roster-refreshed», симметрично probe-эмиттеру
// `register: X` (только сигнал — roster-ось, не register-ось).
//
// Механизм. Refresh-эмиттер — задача `core.soul.registered` с `refresh_soulprint:
// true` (литерал) в params. Refresh-потребитель — задача, статически читающая
// roster прогона:
//
//   - `on: [incarnation.name]` (литерал или `${ incarnation.name }`) — таргетинг по
//     корневой Coven-метке = весь incarnation; резолвится Keeper-side из roster (Hosts);
//   - опущенный `on:` (= весь incarnation, orchestration.md §3) — тоже roster-таргетинг;
//   - `soulprint.hosts` / `soulprint.where(...)` — список хостов прогона;
//   - `soulprint.self.*` — host-вариативный факт (зависит от того, какие хосты в roster).
//
// Любой refresh-потребитель после refresh-эмиттера (program-order) едет в Passage
// ≥ 1 + passage эмиттера. Граница активна ТОЛЬКО при наличии refresh-эмиттера в
// плане; без него — zero-cost, граф register-зависимости и Count БИТ-В-БИТ как до
// ADR-0061 (N=1 fast-path сохранён).
//
// Over-approximation в безопасную сторону: roster-чтение распознаётся консервативно
// (опущенный `on:` тоже считается). Лишний Passage безопасен; пропущенный =
// silent-wrong-target — поэтому при сомнении расщепляем. Не register-граф: refresh-
// граница НЕ добавляет register-ссылок, поэтому инвариант reads⊆refs ADR-056 не
// затрагивается (refresh — отдельная ось).

// RefreshBoundaries возвращает на КАЖДЫЙ Passage P (0..passage.Count-1) признак
// «перед render-ом Passage P scenario-runner обязан ПЕРЕ-резолвить roster» (S3,
// ADR-0061). Граница стоит перед Passage P, если в Passage P-1 завершился хотя бы
// один успешный refresh-эмиттер (`core.soul.registered` с `refresh_soulprint:
// true`) — его барьер сошёлся → онбордившиеся хосты записаны в souls+coven → live-
// снимок roster изменился → потребители Passage P (стратифицированные S2 строго
// ПОСЛЕ refresh-шага) обязаны увидеть актуальный набор.
//
// Семантика re-resolve — СВЕЖИЙ LIVE-СНИМОК roster incarnation на границе (run.go:
// resolveRoster → LoadIncarnationHosts → filterAlive): отражает ТЕКУЩИЙ online-
// набор. Он растёт по мере онбординга провиженных хостов, но это НЕ монотонная
// операция — хост, ушедший offline к границе, из снимка исключается (таргетинг идёт
// на реально-online набор).
//
// out[0] всегда false (перед первым Passage roster уже резолвнут up-front).
// Длина out == passage.Count. Если refresh-эмиттеров нет — все false (re-resolve
// не нужен, поведение БИТ-В-БИТ как до ADR-0061). N=1 → []bool{false}.
//
// Привязка к P-1 (а не «к любому Passage < P»): барьер Passage P-1 — ближайшая
// точка, где refresh-эмиттер этого Passage гарантированно завершился, поэтому ОДИН
// re-resolve на границе достаточен; несколько refresh-эмиттеров в разных Passage
// дают несколько границ (по одной на Passage-после-каждого). passage — результат
// [Stratify] того же tasks.
func RefreshBoundaries(tasks []Task, passage Passage) []bool {
	out := make([]bool, passage.Count)
	if passage.Count <= 1 || len(passage.TaskPassage) != len(tasks) {
		return out // один Passage / рассинхрон — границ нет.
	}
	for i := range tasks {
		if !taskIsRefreshEmitter(&tasks[i]) {
			continue
		}
		// refresh-эмиттер в Passage E → re-resolve перед Passage E+1.
		if next := passage.TaskPassage[i] + 1; next < passage.Count {
			out[next] = true
		}
	}
	return out
}

// refreshModuleAddr — единственный модуль-носитель `refresh_soulprint` (ADR-0061:
// способность 2 живёт на keeper-side core `core.soul.registered`, не отдельная
// сущность). Author-форма адреса задачи — base+state.
const refreshModuleAddr = "core.soul.registered"

// taskIsRefreshEmitter — задача эмитит сигнал «roster-refreshed»: это
// `core.soul.registered` с params.refresh_soulprint == true (литерал bool).
//
// Только литеральный true. `${ … }`-выражение в refresh_soulprint статически
// неопределимо (ADR-010: ${…}-значение не типизируется), поэтому НЕ считается
// эмиттером — приемлемо: refresh_soulprint всегда пишется литералом true (это
// статический флаг поведения, не данные). false / отсутствие → не эмиттер.
func taskIsRefreshEmitter(t *Task) bool {
	if t.Module == nil || t.Module.Module != refreshModuleAddr {
		return false
	}
	v, ok := t.Module.Params["refresh_soulprint"]
	if !ok {
		return false
	}
	b, isBool := v.(bool)
	return isBool && b
}

// taskReadsRoster — задача статически читает roster прогона (см. doc выше):
// on:[incarnation.name] / опущенный on: / soulprint.hosts / soulprint.self.*.
// Рекурсивно через block: (block — атомарная единица Passage; roster-чтение
// любого потомка делает контейнер refresh-потребителем).
//
// Keeper-side задачи (`on: keeper`) НЕ читают roster (у них нет хостов прогона —
// keeperVars без soulprint, render_host.go), поэтому исключаются: refresh-эмиттер
// сам `on: keeper` и НЕ должен зависеть от refresh-границы рекурсивно.
func taskReadsRoster(t *Task) bool {
	if onTargetsRoster(t.On) {
		return true
	}
	// soulprint.* (hosts/where/self) в любом keeper-рендеримом CEL-поле задачи.
	if exprReadsSoulprint(t.Where) {
		return true
	}
	if t.Loop != nil && (exprReadsSoulprint(t.Loop.When) || valueReadsSoulprint(t.Loop.Items)) {
		return true
	}
	if mapReadsSoulprint(t.Vars) || mapReadsSoulprint(t.Output) {
		return true
	}
	if t.Module != nil && mapReadsSoulprint(t.Module.Params) {
		return true
	}
	if t.Apply != nil && mapReadsSoulprint(t.Apply.Input) {
		return true
	}
	if t.Assert != nil {
		for _, that := range t.Assert.That {
			if exprReadsSoulprint(that) {
				return true
			}
		}
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			if taskReadsRoster(&t.Block.Block[i]) {
				return true
			}
		}
	}
	return false
}

// onTargetsRoster — `on:` таргетит весь roster incarnation:
//   - nil (опущенный on:) → весь incarnation (orchestration.md §3);
//   - `on: keeper` (строка) → НЕ roster (keeper-side, нет хостов);
//   - список, содержащий корневую Coven-метку `incarnation.name` (литерал или
//     `${ incarnation.name }`) → весь incarnation (rosterSQL `$1 = ANY(coven)`).
//
// Прочие coven-метки (sub-coven вроде `redis`/`prod`) НЕ считаются roster-чтением:
// они таргетят ПОДмножество, и хотя выросший roster мог бы добавить в него хосты,
// эмиттер refresh всегда вешает на новые SID именно `incarnation.name` (ADR-0061:
// `coven: ["${ incarnation.name }"]`). Для целевого сценария корневая метка —
// канонический способ адресовать выросший roster. (Sub-coven-таргетинг новых
// хостов в одном прогоне — вне S2/S3; при нужде расширяется отдельно.)
func onTargetsRoster(on any) bool {
	switch v := on.(type) {
	case nil:
		return true // опущенный on: = весь incarnation.
	case string:
		return false // `on: keeper` — единственная валидная строковая форма, не roster.
	case []any:
		for _, raw := range v {
			s, ok := raw.(string)
			if !ok {
				continue
			}
			if labelIsIncarnationName(s) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// labelIsIncarnationName — coven-метка ссылается на корневую `incarnation.name`:
// либо литерал `incarnation.name` (редко, но допустимо), либо CEL-обёртка
// `${ incarnation.name }` / `${incarnation.name}`. Распознаётся текстово: точный
// CEL-парс не нужен — форма корневой метки фиксирована грамматикой.
func labelIsIncarnationName(s string) bool {
	if !isCELWrapped(s) {
		return false
	}
	// Внутренность ${ … } — должна целиком быть `incarnation.name` (с возможными
	// пробелами). Содержимое более сложное (например `${ incarnation.name + "-x" }`)
	// сюда не относим: это уже sub-coven, не корневая метка.
	inner := s[2 : len(s)-1]
	return trimSpace(inner) == "incarnation.name"
}

// trimSpace — узкий trim ASCII-пробелов/табов по краям (без unicode-зависимостей,
// CEL-токены — ASCII). Локальная утилита, чтобы не тянуть strings ради одного места.
func trimSpace(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	j := len(s)
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}

// exprReadsSoulprint — CEL-строка ссылается на soulprint.* (hosts/where/self/...).
// Переиспользует существующий канон-парсер reSoulprintRef (`\bsoulprint\b`),
// зеркало keeper render.reFlowControlSoulprint — один источник правды грамматики
// «host-вариативный/roster предикат». Любой soulprint-доступ = roster-чтение:
// soulprint.hosts/where — список хостов прогона, soulprint.self — host-вариативный
// факт (оба зависят от состава roster).
func exprReadsSoulprint(expr string) bool {
	if expr == "" {
		return false
	}
	// Вырезаем строковые литералы CEL, чтобы `'soulprint'` внутри данных не давал
	// ложного срабатывания (как extractSoulprintRefs/ExtractRegisterRefs).
	stripped := celStringLiteral.ReplaceAllString(expr, `""`)
	return reSoulprintRef.MatchString(stripped)
}

// mapReadsSoulprint — любое строковое значение map (vars/params/apply.input/output),
// рекурсивно по вложенным map/seq, читает soulprint.* в `${ … }`-интерполяции.
func mapReadsSoulprint(m map[string]any) bool {
	for _, v := range m {
		if valueReadsSoulprint(v) {
			return true
		}
	}
	return false
}

// valueReadsSoulprint рекурсивно обходит any-значение (string / map / seq).
func valueReadsSoulprint(v any) bool {
	switch t := v.(type) {
	case string:
		return exprReadsSoulprint(t)
	case map[string]any:
		for _, sub := range t {
			if valueReadsSoulprint(sub) {
				return true
			}
		}
	case []any:
		for _, sub := range t {
			if valueReadsSoulprint(sub) {
				return true
			}
		}
	}
	return false
}
