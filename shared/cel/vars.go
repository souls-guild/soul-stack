package cel

import (
	"context"
	"sort"
)

// Vars — переменные контекста, передаваемые в CEL-вычисление. Типизированная
// форма activation: caller заполняет осмысленные поля, а не сырой
// map[string]any. Все поля опциональны; nil превращается в пустой map,
// чтобы обращение к отсутствующему контексту давало штатный CEL-результат
// (no such key), а не панику.
//
// Значения внутри полей — обычные Go-данные (map[string]any, срезы,
// скаляры), полученные из YAML/Postgres. CEL читает их через адаптер
// cel-go.
//
// Pilot-scope ([ADR-010]):
//   - Input        — блок input: сценария/destiny (input.<path>).
//   - Register     — результаты register: предыдущих шагов
//     (register.<name>.<path>, register.self.*).
//   - Incarnation  — поля incarnation (name, service_version, spec.*).
//   - SoulprintSelf — стабильные факты текущего хоста; в CEL доступны как
//     soulprint.self.<path> ([soulprint.md], каноническая форма).
//   - SoulprintHosts — список хостов прогона со стабильными фактами; в CEL
//     доступен как soulprint.hosts (+ .where(<predicate>)). Scenario-only:
//     заполняется только в scenario-проходе render-а. nil/пустой ⇒
//     soulprint.hosts даёт пустой список (а в destiny-проходе обращение к нему
//     — ошибка изоляции, см. [Vars.allowHosts]). [orchestration.md §4.1].
//   - Essence       — effective-слой essence (essence.<path>); host-инвариантен
//     (значения души incarnation, не per-host данные).
//   - Vars         — task-level `vars:` (destiny/tasks.md §9): локальные
//     переменные задачи, уже вычисленные render-ом (CEL-выражения над прочим
//     контекстом, резолвятся ДО params/where). В CEL доступны как `vars.<key>`
//     (expression-keys) и `${ vars.<key> }` (строки). Scope — одна задача (и её
//     loop-итерации); пробрасывается только в per-task контекст. nil/пустой ⇒
//     обращение к `vars.<key>` даёт штатный no-such-key.
//
// Loop — переменные итерации `loop:` (destiny/tasks.md §7): имя из `as:`
// (default `item`) → текущий элемент, опционально имя из `index_as:` →
// индекс/ключ. Имена произвольны (заданы автором), поэтому при непустом Loop
// [Engine.EvalExpression]/[Engine.EvalInterpolation] компилируют выражение
// против дочернего env с этими именами (см. [Engine.loopEnv]); резолвятся
// голой формой `<as>.*` в expression-keys / `${ <as>.* }` в строках
// ([ADR-010]). nil/пустой Loop → обычное вычисление без loop-переменных.
//
// [ADR-010]: docs/adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов
// [soulprint.md]: docs/soul/soulprint.md
type Vars struct {
	Input          map[string]any
	Register       map[string]any
	Incarnation    map[string]any
	SoulprintSelf  map[string]any
	SoulprintHosts []map[string]any
	Essence        map[string]any
	Vars           map[string]any
	Loop           map[string]any

	// State — корень incarnation.state в migration-режиме ([NewMigration],
	// [ADR-019]): в CEL доступен как `state.<path>` (мутируемый по ходу
	// операций миграции). Используется ТОЛЬКО Engine-ом, собранным через
	// [NewMigration]; в обычном (scenario/destiny) Engine это поле
	// игнорируется (активация его не читает). nil ⇒ пустой map (обращение к
	// `state.<key>` даёт штатный no-such-key).
	State map[string]any

	// Ctx — request-scoped контекст для CEL-функции vault() (отмена/таймаут
	// ReadKV). Используется только когда Engine собран с KVReader (New + WithVault)
	// и выражение зовёт vault(); иначе игнорируется. nil ⇒ context.Background()
	// (vault() без отмены — допустимо для офлайн-режимов soul-lint/Trial).
	Ctx context.Context

	// AllowHosts разрешает soulprint.hosts/soulprint.where(...) в выражении.
	// true — scenario-проход (host-аксессор виден); false (zero-value) —
	// destiny-проход и прочие контексты без хостов прогона: обращение к
	// soulprint.hosts → ошибка изоляции ([orchestration.md §4.1]). Часть ключа
	// compile-cache (исход компиляции зависит от флага).
	AllowHosts bool
}

// activation строит map для cel.NewActivation. soulprint обёрнут в
// {"self": …, "hosts": […]}, чтобы каноничная форма soulprint.self.<path> и
// scenario-аксессор soulprint.hosts резолвились, а голая soulprint.<path>
// давала отсутствие ключа (валидатором это ловится отдельно — [soulprint.md]).
// vars — task-level `vars:` ([Vars.Vars]): nil ⇒ пустой map (обращение к
// `vars.<key>` даёт штатный no-such-key). Значения уже вычислены render-ом ДО
// сборки активации (vars: резолвятся перед params/where).
//
// soulprint.hosts — list(map(string,dyn)); nil SoulprintHosts ⇒ пустой список
// (обращение в destiny-проходе отсекается ещё на compile, [Vars.AllowHosts]).
//
// Loop-переменные кладутся на верхний уровень активации под своими именами
// (голая форма `<as>.*`). Имена итерации не конфликтуют с фиксированным
// контекстом: config-валидатор запрещает `as:`/`index_as:`, совпадающие с
// зарезервированными именами ([scenario_task.go]).
func (v Vars) activation(migration bool) map[string]any {
	var act map[string]any
	if migration {
		// migration-режим ([NewMigration]): объявлена только `state`. Прочие
		// контекст-имена в активацию НЕ кладутся — они и не объявлены в env
		// ([migrationVars]), обращение к ним отсекается на compile.
		act = map[string]any{"state": orEmpty(v.State)}
	} else {
		act = map[string]any{
			"input":       orEmpty(v.Input),
			"register":    orEmpty(v.Register),
			"incarnation": orEmpty(v.Incarnation),
			"soulprint":   map[string]any{"self": orEmpty(v.SoulprintSelf), "hosts": orEmptyHosts(v.SoulprintHosts)},
			"essence":     orEmpty(v.Essence),
			"vars":        orEmpty(v.Vars),
		}
	}
	for name, val := range v.Loop {
		act[name] = val
	}
	return act
}

// loopNames возвращает отсортированный список имён loop-переменных (ключ
// дочернего env и его кеша). Пустой Loop → nil.
func (v Vars) loopNames() []string {
	if len(v.Loop) == 0 {
		return nil
	}
	names := make([]string, 0, len(v.Loop))
	for name := range v.Loop {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// orEmptyHosts конвертирует []map[string]any в []any для cel-адаптера
// (list-элементы — dyn). nil ⇒ пустой list (soulprint.hosts без хостов).
func orEmptyHosts(hosts []map[string]any) []any {
	out := make([]any, len(hosts))
	for i, h := range hosts {
		out[i] = h
	}
	return out
}
