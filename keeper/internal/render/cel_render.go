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

// resolveTaskVars собирает финальный слой `vars.*` для рендера одной задачи на
// одном хосте: БАЗА — резолвленные destiny-локалы `vars.yml` (fileVars,
// docs/destiny/vars.md), ПОВЕРХ — task-level `vars:` (taskVars, destiny/tasks.md
// §9). Вариант A (vars.md «Слияние file-vars ↔ task-vars»): task-var переопределяет
// одноимённый file-var.
//
// fileVars уже резолвлены ОДИН раз на destiny-проход (resolveDestinyVars над
// destiny-env input+soulprint.self+incarnation, изолированно от scenario-scope) —
// здесь они только подкладываются базой. В scenario-проходе fileVars пуст (vars.yml
// — destiny-сущность); поведение scenario-задач БИТ-В-БИТ как до фичи.
//
// taskVars резолвятся через resolveVarLayer над base, где base.Vars ПУСТ на старте
// слоя — task-var видит ТОЛЬКО task-var того же слоя (var→var разрешён внутри слоя,
// eager-topological), но НЕ file-var (межслойная изоляция: `${ vars.<file_var> }` в
// task-var → ErrVarUnknownRef). Это сознательно: file-vars в base.Vars НЕ кладутся
// ДО резолва task-vars, иначе task-var мог бы сослаться на file-var и нарушить
// межслойную границу. Цикл task-var→task-var → ErrVarCycle, ссылка на несуществующий
// task-var → ErrVarUnknownRef (зеркало resolveDestinyVars). Порядок ключей в taskVars
// безразличен.
//
// file-vars подкладываются под task-vars ПОСЛЕ резолва (override, Вариант A):
// одноимённый task-var перетирает file-var в финальной карте.
//
// Оба пусты → base без изменений (Vars=nil → штатный no-such-key на `vars.<key>`).
func resolveTaskVars(engine *cel.Engine, fileVars, taskVars map[string]any, base cel.Vars) (cel.Vars, error) {
	if len(fileVars) == 0 && len(taskVars) == 0 {
		return base, nil
	}
	// task-vars резолвятся СВОИМ слоем над base с пустым base.Vars (resolveVarLayer
	// накапливает их сам) — file-vars не видны, var→var живёт внутри task-слоя.
	resolvedTask, err := resolveVarLayer(engine, taskVars, base)
	if err != nil {
		return cel.Vars{}, err
	}
	resolved := make(map[string]any, len(fileVars)+len(resolvedTask))
	// База: file-vars уже резолвлены — копируются как есть (без CEL).
	for key, val := range fileVars {
		resolved[key] = val
	}
	// Поверх: task-vars перезаписывают одноимённые file-vars (Вариант A).
	for key, val := range resolvedTask {
		resolved[key] = val
	}
	base.Vars = resolved
	return base, nil
}

// fileVarsForHost возвращает резолвленные destiny-локалы `vars.yml` для хоста host
// (база слоя `vars.*`, Вариант A). Источник — in.DestinyVarsResolved, заполненный
// renderApplyDestiny per-host. nil host (синтетический пустой контекст) → ключ "".
// nil-карта (scenario-проход / destiny без vars.yml) → nil (база пуста).
func fileVarsForHost(in RenderInput, host *topology.HostFacts) map[string]any {
	if in.DestinyVarsResolved == nil {
		return nil
	}
	sid := ""
	if host != nil {
		sid = host.SID
	}
	return in.DestinyVarsResolved[sid]
}

// incarnationVars строит incarnation-map для CEL-контекста: name/service/
// service_version из IncarnationMeta + host_count (число targeted-хостов,
// scenario-предикаты используют его — add_user/main.yml).
//
// state — read-only снимок incarnation.state на момент захвата row-lock прогона
// ([ADR-009]/[ADR-010]): scenario-render-контекст видит pre-run state как
// `incarnation.state.<path>` в params/where/apply-input И в state_changes
// (stateChangesVars зовёт ту же функцию). Снимок ИНВАРИАНТЕН на все passages
// staged-render-а (RenderInput.State фиксируется один раз stateBefore под FOR
// UPDATE, не накапливается между passages — в отличие от register). nil-State →
// ключ не кладётся: `incarnation.state.<x>` даёт штатный no-such-key (push/trial
// без State, backward-compat), не compile-ошибку (`incarnation` — DynType).
func incarnationVars(in RenderInput, hostCount int) map[string]any {
	m := map[string]any{
		"name":            in.Incarnation.Name,
		"service":         in.Incarnation.Service,
		"service_version": in.Incarnation.ServiceVersion,
		"host_count":      hostCount,
	}
	if in.State != nil {
		m["state"] = in.State
	}
	return m
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
		Compute:        in.Compute,
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
// essence }` + УСЛОВНО `input`. Это per-host структура (self host-вариативен),
// которую Keeper кладёт в params.render_context и доставляет Soul-у; Soul
// передаёт её КОРНЕМ в text/template (rendered.go), поэтому шаблон видит
// `.vars.*`/`.self.*`/`.role`/`.essence.*` (и `.input.*` при injectInput).
//
// self — ТА ЖЕ soulprintSelfMap, что в CEL-фазе (hostVars): единая точка правды
// (ADR-018, soulprint.self.<path> в CEL ≡ .self.<path> в шаблоне). vars — слияние
// РЕФЕРЕНСНЫХ destiny-локалов `vars.yml` (fileVars, БАЗА — только ключи, что шаблон
// читает как `.vars.<key>`, см. referencedFileVars) и CEL-резолвленного
// `params.vars` шага (override); под ключ vars, НЕ плоским корнем. essence —
// effective-слой incarnation (host-инвариантный snapshot). role — declared-роль
// хоста из spec (bootstrap-create; может быть "").
//
// vars-слой ЗЕРКАЛИТ CEL-фазу (resolveTaskVars, Вариант A vars.md): file-vars
// доступны шаблону как `.vars.<file_var>` НАПРЯМУЮ, без redundant passthrough
// через `params.vars` каждого file-var (node-exporter: `.vars.bin_path`). Поверх
// file-vars кладётся `params.vars` шага — одноимённый task-var перетирает file-var
// (детерминированный override). ТОЧЕЧНОСТЬ (referencedFileVars): подкладываются
// только file-vars, чей ключ шаблон реально читает — шаблон без `.vars.<file_var>`
// (redis: читает task-var-ключи) лишних file-var-ключей НЕ получает, его `.vars`
// БИТ-В-БИТ как до фичи. В scenario-проходе fileVars пуст (vars.yml — destiny-
// сущность) → `.vars` = только params.vars, поведение БИТ-В-БИТ как было.
//
// input — резолвнутый operator-input прохода (Вариант B, ADR-010 §3.2
// amendment): шаблон читает `.input.<name>` напрямую, без passthrough
// `params.vars` каждого input-поля. Источник — in.Input (host-инвариантен: общий
// контекст прогона). ★УСЛОВНО: ключ `input` кладётся ТОЛЬКО когда injectInput ==
// true — т.е. шаблон реально читает `.input.*` (детект по AST до per-host цикла,
// renderTaskIter → tmpl.UsesRootField). Шаблоны на одних `.vars` (redis: секреты
// едут через `.vars`) `input` НЕ получают → их render_context БИТ-В-БИТ как до
// Варианта B (deep-equal-фикстуры стабильны, секреты в render_context.input не
// попадают). При injectInput && nil-Input → пустой map: `.input.*` упадёт
// strict-mode на незаявленном поле, что корректно.
//
// ★Security (seal S-1, ADR-010 §7.4): убрав passthrough vars, теряем
// seal-провенанс секретов из сырых params (раньше `${ input.secret }` физически
// в params.vars → collectSealed ловил). Провенанс восстанавливается
// ДЕКЛАРАТИВНО — caller (renderTaskIter) помечает sealed пути
// `render_context.input.<secret>` для каждого secret-input активной схемы
// (sealRenderContextInput), И ТОЛЬКО когда input реально инъектится (тот же
// injectInput-гейт), а не по присутствию выражения в params.
//
// fileVars — резолвленные destiny-локалы `vars.yml` хоста (база `.vars`-слоя,
// fileVarsForHost). paramsVars — CEL-rendered значение `params.vars` шага (override).
// Оба nil/пусты → `.vars` пустой map: шаблон с `.vars.*` упадёт strict-mode, что
// корректно — обращение к незаявленной vars-переменной = ошибка автора.
func buildRenderContext(in RenderInput, host *topology.HostFacts, fileVars, paramsVars map[string]any, injectInput bool) map[string]any {
	// `.vars` = file-vars (база) + params.vars (override) — Вариант A, как
	// resolveTaskVars в CEL-фазе. Зеркало гарантирует: file-var виден и в params
	// (CEL `vars.<x>`), и в шаблоне (`.vars.<x>`). orEmptyMap нормализует nil
	// (mergeVars отдаёт nil при обоих пустых) — `.vars` всегда присутствует ключом.
	rc := map[string]any{
		"vars":    orEmptyMap(mergeVars(fileVars, paramsVars)),
		"self":    soulprintSelfMap(host),
		"role":    host.Role,
		"essence": in.Essence,
	}
	if injectInput {
		rc["input"] = orEmptyMap(in.Input)
	}
	return rc
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
// имена Choir-ов хоста (ADR-044, S-T4); registry-проекция, как covens. traits —
// operator-set key-value метки (ADR-060); registry-проекция, как covens/choirs.
//
// Симметрия с hostFactsToMap (soulprint.hosts): self и элемент hosts дают
// согласованные sid/covens/role/choirs/traits. host.Soulprint не мутируется — строится
// новый верхнеуровневый map (значения подсекций reported шарятся read-only,
// render их не меняет).
func soulprintSelfMap(host *topology.HostFacts) map[string]any {
	self := make(map[string]any, len(host.Soulprint)+5)
	for k, v := range host.Soulprint {
		self[k] = v
	}
	self["sid"] = host.SID
	self["covens"] = covensList(host.Coven)
	self["role"] = host.Role
	self["choirs"] = covensList(host.Choirs)
	// traits — operator-set key-value метки (ADR-060), registry-проекция как
	// covens/choirs (перекрывает одноимённый reported-ключ). Всегда кладётся
	// (пустой map при nil): `soulprint.self.traits.<key>` даёт штатный
	// no-such-key, не отсутствие самого traits.
	self["traits"] = orEmptyMap(host.Traits)
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
// covens/choirs — копии срезов (cel читает как list); traits — operator-set
// key-value map (ADR-060, registry-проекция); network/os — подмапы Soulprint
// (отсутствуют → пустой map, обращение к полю даёт штатный no-such-key).
func hostFactsToMap(h *topology.HostFacts) map[string]any {
	return map[string]any{
		"sid":     h.SID,
		"role":    h.Role,
		"covens":  covensList(h.Coven),
		"choirs":  covensList(h.Choirs),
		"traits":  orEmptyMap(h.Traits), // operator-set key-value (ADR-060), как covens/choirs
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
		Compute:       in.Compute,
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
