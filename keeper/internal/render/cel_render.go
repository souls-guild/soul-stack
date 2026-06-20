package render

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// renderParams — фаза CEL-render одной задачи на одном хосте ([ADR-010]).
// Рекурсивно обходит params, в каждой строковой ячейке вычисляет
// `${ … }`-интерполяцию через cel.Engine. Non-string значения проходят
// насквозь.
//
// Per-host: vars.SoulprintSelf — факты именно хоста host, поэтому одна и та же
// задача на разных хостах может дать разные params (например, `${
// soulprint.self.os.family }`). Caller вызывает renderParams для каждого
// targeted-хоста (см. dispatch).
//
// Результат — `*structpb.Struct` (прямая стыковка с proto RenderedTask.params).
// Возможные ошибки cel.Engine ([ErrCompile]/[ErrEval]/[ErrUnsupported])
// пробрасываются с контекстом ключа.
func renderParams(engine *cel.Engine, params map[string]any, vars cel.Vars) (*structpb.Struct, error) {
	rendered, err := renderValue(engine, params, vars, "")
	if err != nil {
		return nil, err
	}
	m, _ := rendered.(map[string]any)
	st, err := structpb.NewStruct(m)
	if err != nil {
		return nil, fmt.Errorf("render: params → structpb: %w", err)
	}
	return st, nil
}

// renderValue рекурсивно рендерит произвольное YAML-значение. path —
// человекочитаемый путь до ячейки (для диагностики, например `acl` или
// `users[0].name`).
func renderValue(engine *cel.Engine, v any, vars cel.Vars, path string) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			rv, err := renderValue(engine, val, vars, joinKey(path, k))
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			rv, err := renderValue(engine, val, vars, joinIdx(path, i))
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	case string:
		res, err := engine.EvalInterpolation(t, vars)
		if err != nil {
			return nil, fmt.Errorf("render: ячейка %q: %w", path, err)
		}
		return res, nil
	default:
		return v, nil
	}
}

// evalBoolExpr вычисляет top-level bool-предикат ([ADR-010]: вся строка = CEL)
// и приводит результат к bool. kind — человекочитаемая метка ключа («where»/
// «loop.when») для сообщений об ошибке. Пустой expr → true (нет предиката).
// Не-bool результат → ошибка: предикат обязан возвращать булево.
func evalBoolExpr(engine *cel.Engine, kind, expr string, vars cel.Vars) (bool, error) {
	if expr == "" {
		return true, nil
	}
	out, err := engine.EvalExpression(expr, vars)
	if err != nil {
		return false, fmt.Errorf("render: %s %q: %w", kind, expr, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("render: %s %q вернул %T, ожидался bool", kind, expr, out.Value())
	}
	return b, nil
}

// evalWhere вычисляет per-host предикат `where:` (orchestration.md §4).
func evalWhere(engine *cel.Engine, where string, vars cel.Vars) (bool, error) {
	return evalBoolExpr(engine, "where", where, vars)
}

// resolveTaskVars вычисляет task-level `vars:` (destiny/tasks.md §9) и возвращает
// контекст vars с заполненным полем Vars. Каждое значение vars[key] — CEL-
// выражение/интерполяция (`${ … }`) над контекстом задачи (input/incarnation/
// soulprint.self/essence/register/loop), резолвится через EvalInterpolation
// (нативный тип при ровно одном `${…}`-блоке, иначе строка).
//
// Scope per-task: vars: вычисляются ДО params/where в контексте base, где сами
// task-vars ещё НЕ видны (base.Vars пуст). Это запрещает ссылки vars→vars
// (взаимные/самоссылки), как зафиксировано в destiny/tasks.md §9 («не на свои же
// task-vars — нет циклических ссылок»): обращение `vars.<key>` внутри другого
// vars-значения даст no-such-key. Поэтому порядок вычисления между ключами
// безразличен (каждый видит только базовый контекст).
//
// Пустой/nil task-vars → base без изменений (поле Vars остаётся nil → штатный
// no-such-key на `vars.<key>`).
func resolveTaskVars(engine *cel.Engine, taskVars map[string]any, base cel.Vars) (cel.Vars, error) {
	if len(taskVars) == 0 {
		return base, nil
	}
	resolved := make(map[string]any, len(taskVars))
	for key, raw := range taskVars {
		s, ok := raw.(string)
		if !ok {
			// Non-string vars-значение (число/bool/коллекция) проходит как литерал —
			// CEL-фаза трогает только строки (renderValue), симметрично params.
			resolved[key] = raw
			continue
		}
		val, err := engine.EvalInterpolation(s, base)
		if err != nil {
			return cel.Vars{}, fmt.Errorf("render: vars.%s: %w", key, err)
		}
		resolved[key] = val
	}
	base.Vars = resolved
	return base, nil
}

// incarnationVars строит incarnation-map для CEL-контекста: name/service/
// service_version из IncarnationMeta + host_count (число targeted-хостов,
// scenario-предикаты используют его — add_user/main.yml).
func incarnationVars(in RenderInput, hostCount int) map[string]any {
	return map[string]any{
		"name":            in.Incarnation.Name,
		"service":         in.Incarnation.Service,
		"service_version": in.Incarnation.ServiceVersion,
		"host_count":      hostCount,
	}
}

// hostVars строит cel.Vars для конкретного хоста: общий контекст прогона
// (input/register/incarnation/essence) + soulprint.self именно этого хоста.
// Essence host-инвариантна (effective-слой incarnation), но кладётся в каждый
// per-host контекст — она доступна везде, где input.
//
// soulprint.hosts (+ .where) проецируется из in.Hosts ТОЛЬКО в scenario-проходе
// (in.destinyIsolated==false). В destiny-проходе host-аксессор отсекается:
// AllowHosts=false → обращение к soulprint.hosts — ошибка изоляции на compile.
func hostVars(in RenderInput, host *topology.HostFacts, hostCount int) cel.Vars {
	return cel.Vars{
		Input:          in.Input,
		Register:       hostRegister(in, host),
		Incarnation:    incarnationVars(in, hostCount),
		SoulprintSelf:  soulprintSelfMap(host),
		SoulprintHosts: soulprintHosts(in),
		Essence:        in.Essence,
		Ctx:            in.Ctx,
		AllowHosts:     !in.destinyIsolated,
	}
}

// hostRegister выбирает register-контекст для CEL-рендера задач конкретного
// хоста. Staged-render (ADR-056 §в.1): render Passage N подставляет register
// ПРЕДЫДУЩИХ Passage per-host — `register.<probe>.*` в `where:`/`apply:input:`/
// `params:`/`vars:` резолвится фактом, собранным этим хостом (probe роли вернул
// 'master' на одном хосте, 'slave' на другом). Источник — in.RegisterByHost[sid]
// (накопленный барьерами предыдущих Passage, прокинутый stage-loop-ом run.go).
//
// Backward-compat: если per-host карта для хоста пуста (первый Passage, N=1-
// прогон, либо не-staged путь) — возвращается flat in.Register (в пилоте пуст).
// Так N=1-прогон видит register=пусто как до staged-render (БИТ-В-БИТ), а
// keeper-side/destiny-проходы (свои register-контексты) не затрагиваются.
func hostRegister(in RenderInput, host *topology.HostFacts) map[string]any {
	if host != nil {
		if reg := in.RegisterByHost[host.SID]; len(reg) > 0 {
			return reg
		}
	}
	return in.Register
}

// buildRenderContext собирает корень text/template-контекста для шага
// core.file.rendered по нормативу templating.md §3.2: `{ vars, self, role,
// essence }`. Это per-host структура (self host-вариативен), которую Keeper
// кладёт в params.render_context и доставляет Soul-у; Soul передаёт её КОРНЕМ
// в text/template (rendered.go), поэтому шаблон видит `.vars.*`/`.self.*`/
// `.role`/`.essence.*`.
//
// self — ТА ЖЕ soulprintSelfMap, что в CEL-фазе (hostVars): единая точка правды
// (ADR-018, soulprint.self.<path> в CEL ≡ .self.<path> в шаблоне). vars —
// CEL-резолвленный `params.vars` шага (см. templating.md §6: автор поднимает
// нужные значения в `params.vars`, шаблон читает `.vars.<name>`); под ключ vars,
// НЕ плоским корнем. essence — effective-слой incarnation (host-инвариантный
// snapshot). role — declared-роль хоста из spec (bootstrap-create; может быть "").
//
// paramsVars — CEL-rendered значение `params.vars` (nil/отсутствует → пустой
// map: шаблон с `.vars.*` упадёт strict-mode, что корректно — обращение к
// незаявленной vars-переменной = ошибка автора).
func buildRenderContext(in RenderInput, host *topology.HostFacts, paramsVars map[string]any) map[string]any {
	vars := paramsVars
	if vars == nil {
		vars = map[string]any{}
	}
	return map[string]any{
		"vars":    vars,
		"self":    soulprintSelfMap(host),
		"role":    host.Role,
		"essence": in.Essence,
	}
}

// flowContextSelfKey — ключ host-вариативной секции flow_context (per-host
// soulprint.self). Вычитается из host-инвариантной сверки flow_context
// (flowContextHostInvariant): self host-вариантен по природе и закрыт отдельным
// regex-guard на текст предиката, а не сверкой снапшота.
const flowContextSelfKey = "self"

// buildFlowContext собирает литеральный per-host снапшот не-register части
// CEL-контекста flow-control-предикатов (when:/changed_when:/failed_when:,
// ADR-012(d)): `{ input, vars, essence, incarnation, self }`. Это ровно тот
// контекст, что доступен рендеру params данного хоста (vars cel.Vars), МИНУС
// soulprint.hosts (cross-host scenario-only — Soul его не имеет) и loop
// (loop-переменные в flow_context не кладутся; их семантика — render-time fan-out,
// не runtime-предикат). `self` = soulprintSelfMap(host) — та же проекция, что
// soulprint.self в CEL-фазе. register.* в flow_context НЕ кладётся — его Soul
// строит сам из результатов предыдущих задач.
//
// vars — task-level `vars:` уже CEL-резолвленные (vars.Vars); nil → пустой map.
// MVP: контекст ПОЛНЫЙ, без static-pruning (Soul получает весь снапшот, даже
// если предикат ссылается лишь на часть). Возврат — *structpb.Struct (прямая
// стыковка с proto RenderedTask.flow_context).
func buildFlowContext(in RenderInput, host *topology.HostFacts, vars cel.Vars, hostCount int) (*structpb.Struct, error) {
	fc := map[string]any{
		"input":            orEmptyMap(vars.Input),
		"vars":             orEmptyMap(vars.Vars),
		"essence":          orEmptyMap(vars.Essence),
		"incarnation":      incarnationVars(in, hostCount),
		flowContextSelfKey: soulprintSelfMap(host),
	}
	st, err := structpb.NewStruct(fc)
	if err != nil {
		return nil, fmt.Errorf("flow_context → structpb: %w", err)
	}
	return st, nil
}

// orEmptyMap — nil map → пустой (structpb.NewStruct не принимает nil-вложенность
// единообразно; пустой map даёт штатный no-such-key на Soul-е, не панику).
// Локальный дубль неэкспортированного shared/cel.orEmpty — экспортировать его
// ради одной cel-функции не стоит (узкая helper-семантика, разные пакеты).
func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// soulprintSelfMap строит soulprint.self для хоста: merge reported-фактов
// (os/network/kernel/cpu/memory — когда Soul их прислал) и авторитетных
// registry-данных roster (HostFacts).
//
// sid/covens — Keeper-registry-проекция (ADR-018, soulprint.md «Граница
// Soulprint ↔ souls-registry»): источник истины — registry, не Soul. Поэтому
// они кладутся ВСЕГДА (даже при NULL reported facts: authority sid — mTLS peer
// cert, не collected-факт) и ПЕРЕЗАПИСЫВАЮТ одноимённые reported-ключи, если те
// случайно затесались. role — declared-роль из spec (может быть ""). choirs —
// имена Choir-ов хоста (ADR-044, S-T4); registry-проекция, как covens.
//
// Симметрия с hostFactsToMap (soulprint.hosts): self и элемент hosts дают
// согласованные sid/covens/role/choirs. host.Soulprint не мутируется — строится
// новый верхнеуровневый map (значения подсекций reported шарятся read-only,
// render их не меняет).
func soulprintSelfMap(host *topology.HostFacts) map[string]any {
	self := make(map[string]any, len(host.Soulprint)+4)
	for k, v := range host.Soulprint {
		self[k] = v
	}
	self["sid"] = host.SID
	self["covens"] = covensList(host.Coven)
	self["role"] = host.Role
	self["choirs"] = covensList(host.Choirs)
	return self
}

// covensList копирует registry-список строк (covens/choirs) в []any (cel читает
// list как []any), не делясь backing-массивом с roster.
func covensList(items []string) []any {
	out := make([]any, len(items))
	for i, c := range items {
		out[i] = c
	}
	return out
}

// soulprintHosts проецирует in.Hosts в []map для cel-аксессора soulprint.hosts:
// стабильный слой (sid/role/covens/choirs/network/os, orchestration.md §4.1).
// network/os берутся из last-reported Soulprint-map хоста; covens/role/choirs —
// registry-данные (HostFacts.Coven/Role/Choirs). В destiny-проходе НЕ
// проецируется (nil) — там аксессор отсекается изоляцией.
func soulprintHosts(in RenderInput) []map[string]any {
	if in.destinyIsolated || len(in.Hosts) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(in.Hosts))
	for _, h := range in.Hosts {
		out = append(out, hostFactsToMap(h))
	}
	return out
}

// hostFactsToMap строит элемент soulprint.hosts из HostFacts: стабильные поля.
// covens/choirs — копии срезов (cel читает как list); network/os — подмапы
// Soulprint (отсутствуют → пустой map, обращение к полю даёт штатный
// no-such-key).
func hostFactsToMap(h *topology.HostFacts) map[string]any {
	return map[string]any{
		"sid":     h.SID,
		"role":    h.Role,
		"covens":  covensList(h.Coven),
		"choirs":  covensList(h.Choirs),
		"network": soulprintSection(h.Soulprint, "network"),
		"os":      soulprintSection(h.Soulprint, "os"),
	}
}

// soulprintSection извлекает подсекцию (network/os) из Soulprint-map хоста.
// Отсутствие/неверный тип → пустой map (обращение к полю → штатный no-such-key,
// не паника).
func soulprintSection(soulprint map[string]any, key string) map[string]any {
	if soulprint == nil {
		return map[string]any{}
	}
	if sec, ok := soulprint[key].(map[string]any); ok {
		return sec
	}
	return map[string]any{}
}

// hostLoopVars — hostVars + переменные текущей `loop:`-итерации (`<as>`/
// `<index_as>`, destiny/tasks.md §7). loop=nil → эквивалент hostVars (без
// loop-переменных). Используется renderLoopTask: итерация рендерит params в
// контексте конкретного хоста с активной loop-переменной.
func hostLoopVars(in RenderInput, host *topology.HostFacts, hostCount int, loop map[string]any) cel.Vars {
	v := hostVars(in, host, hostCount)
	v.Loop = loop
	return v
}

// stateChangesVars строит cel.Vars для рендера state_changes.sets на хосте host
// (orchestration.md §7.1). Контекст — input/incarnation/soulprint.self плюс
// Register этого хоста (слайс 2 полной грамматики): register-данные probe-задач
// прогона, накопленные после барьера и резолвнутые по register-имени
// (in.RegisterByHost[host.SID]). nil-register у хоста (нет register: задач) →
// `register.*` в sets даст eval-ошибку "no such key", как и раньше.
func stateChangesVars(in RenderInput, host *topology.HostFacts) cel.Vars {
	return cel.Vars{
		Input:         in.Input,
		Register:      in.RegisterByHost[host.SID],
		Incarnation:   incarnationVars(in, len(in.Hosts)),
		SoulprintSelf: soulprintSelfMap(host),
		Essence:       in.Essence,
		Ctx:           in.Ctx,
	}
}

// sortedHostsBySID возвращает копию hosts, отсортированную лексикографически по
// SID (детерминизм last-wins-свёртки state_changes.sets, orchestration.md §7.1).
func sortedHostsBySID(hosts []*topology.HostFacts) []*topology.HostFacts {
	out := make([]*topology.HostFacts, len(hosts))
	copy(out, hosts)
	sort.Slice(out, func(i, j int) bool { return out[i].SID < out[j].SID })
	return out
}

func joinKey(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func joinIdx(path string, i int) string {
	return fmt.Sprintf("%s[%d]", path, i)
}
