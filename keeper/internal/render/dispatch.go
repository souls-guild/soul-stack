package render

import (
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// resolveTargets резолвит хосты задачи: `on:` (выбор Coven-меток) → `where:`
// (per-host предикат). Возвращает отсортированный по SID slice TargetSIDs и
// сами host-объекты для последующего per-host CEL-рендера params.
//
// Резолв `on:` (orchestration.md §3, [ADR-040] amendment 2026-05-27):
//   - опущен (task.On == nil) → весь incarnation (все hosts);
//   - `on: keeper` → keeper-side задача, вне pilot-объёма → [ErrUnsupportedDSL];
//   - `on: [coven, …]` → AND-пересечение по Coven-меткам (хост попадает, только
//     если у него присутствуют ВСЕ перечисленные метки). Литералы-ковены с
//     CEL-обёрткой `${ … }` (например, `${ incarnation.name }`) вычисляются;
//     `${ incarnation.name }` проходит через общий фильтр как обычная метка и не
//     сужает scope — каждый хост roster-а несёт корневую метку по построению
//     (rosterSQL `WHERE $1 = ANY(coven)`, ADR-008), так что фильтрация по ней
//     эквивалентна «весь incarnation».
//
// Резолв `where:` — per-host bool-предикат (evalWhere). Пустой where → все
// targeted-хосты.
func resolveTargets(engine *cel.Engine, in RenderInput, task config.Task) ([]*topology.HostFacts, error) {
	covens, err := resolveOn(engine, in, task.On)
	if err != nil {
		return nil, err
	}

	targeted := filterByCovens(in.Hosts, covens)

	if task.Where != "" {
		out := make([]*topology.HostFacts, 0, len(targeted))
		for _, h := range targeted {
			vars := hostVars(in, h, len(targeted))
			vars, err = resolveTaskVars(engine, fileVarsForHost(in, h), task.Vars, vars)
			if err != nil {
				return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
			}
			ok, err := evalWhere(engine, task.Where, vars)
			if err != nil {
				return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
			}
			if ok {
				out = append(out, h)
			}
		}
		targeted = out
	}

	// Сортировка по SID — контракт «первый по SID» (golden-path), на который
	// опираются: renderTaskIter (hi==0 кладёт render_context/flow_context первого
	// по SID в rt.Params), compute apply.input (резолв на targeted[0]), run_once:
	// (applyRunOnce берёт первого по SID). filterByCovens/where сохраняют порядок
	// roster-а (in.Hosts) — без этой сортировки targeted[0] был бы первым в roster,
	// а не первым по SID, и per-host материализация render_context (RenderContextBySID
	// ключуется SID) расходилась бы с тем, что уезжает в rt.Params. sidsOf/
	// DispatchPlan.TargetSIDs уже сортируют независимо — порядок плана не меняется.
	sortBySID(targeted)
	return targeted, nil
}

// sortBySID упорядочивает хосты лексикографически по SID на месте (детерминизм
// «первого по SID» — golden-path resolveTargets/renderTaskIter). Идемпотентна на
// уже отсортированном и на пустом/одиночном срезе.
func sortBySID(hosts []*topology.HostFacts) {
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].SID < hosts[j].SID })
}

// keeperOnLiteral — скалярная форма `on:`, помечающая keeper-side задачу
// (docs/keeper/modules.md). Совпадает с [KeeperTargetSID] не случайно: литерал
// `on: keeper` и synthetic target-SID keeper-инстанса — одно и то же понятие.
const keeperOnLiteral = "keeper"

// IsKeeperTask сообщает, объявлена ли задача keeper-side (`on: keeper`,
// docs/keeper/modules.md). keeper-side задача рендерится в keeper-контексте (без
// per-host roster, см. renderKeeperTask) и исполняется локально на keeper-
// инстансе scenario-runner-ом. Прочие формы `on:` (опущен / список ковенов) —
// Soul-side. config-валидатор гарантирует, что скалярная форма `on:` — только
// "keeper".
func IsKeeperTask(task config.Task) bool {
	s, ok := task.On.(string)
	return ok && s == keeperOnLiteral
}

// IsAssertTask сообщает, является ли задача assert-проверкой (ADR-009 amendment
// 2026-06-23): дискриминатор `assert:`. assert вычисляется Keeper-side на render-
// фазе как run-level precondition и НЕ emit RenderedTask — поэтому отводится из
// общего цикла Render до guard/static-when-обработки (см. evalAssertTask).
func IsAssertTask(task config.Task) bool {
	return task.Assert != nil
}

// resolveOn преобразует значение `on:` в список Coven-меток. Возвращаемый
// nil/empty означает «нет фильтра по ковенам» (весь incarnation, только при
// опущенном on:). Корневая метка `${ incarnation.name }` НЕ отбрасывается
// специально: она попадает в список как обычная метка, а фильтрация по ней
// безопасна и эквивалентна «весь incarnation» — каждый хост roster-а несёт
// корневую метку (rosterSQL `WHERE $1 = ANY(coven)`, ADR-008).
//
// `on: keeper` сюда НЕ доходит — keeper-side задачи отводятся пайплайном в
// renderKeeperTask до резолва roster (см. [IsKeeperTask]); ветка оставлена
// defense-in-depth (программная ошибка маршрутизации → явная ошибка, не silent).
func resolveOn(engine *cel.Engine, in RenderInput, on any) ([]string, error) {
	switch v := on.(type) {
	case nil:
		return nil, nil
	case string:
		if v == keeperOnLiteral {
			return nil, fmt.Errorf("render: on: keeper достиг Soul-side резолва roster — keeper-side задача должна маршрутизироваться в renderKeeperTask (программная ошибка)")
		}
		return nil, fmt.Errorf("render: on: %q — недопустимая скалярная форма (ожидалось 'keeper' или список ковенов)", v)
	case []any:
		return resolveCovenList(engine, in, v)
	default:
		return nil, fmt.Errorf("render: on: имеет тип %T, ожидалась строка 'keeper' или список ковенов", on)
	}
}

// keeperVars строит CEL-контекст для рендера params keeper-side задачи: ровно тот
// «контекст без soulprint», что [resolveCovenList] использует для `on:`-меток
// (per-run, не per-host). У keeper-задачи хостов нет → soulprint.self/.hosts
// недоступны (обращение к ним в params keeper-задачи — штатная CEL-ошибка
// no-such-key, как и должно быть: keeper-шаг оперирует input/incarnation/essence,
// не фактами хоста).
//
// incarnation.state — read-only pre-run снимок (RenderInput.State, тот же stateBefore
// под FOR UPDATE, см. [incarnationVars]): keeper-задача (core.cloud.destroyed и пр.)
// читает `incarnation.state.<path>` в params симметрично Soul-side. Снимок инвариантен
// (фиксируется один раз, не накапливается между passages). nil-State → ключ `state`
// не кладётся: `incarnation.state.<x>` даёт штатный no-such-key (push/trial без State,
// backward-compat). Граница keeper↔soul соблюдена: state — operator-факты (не секреты),
// soulprint.self/.hosts по-прежнему недоступны (хостов нет).
//
// register: keeper→keeper chaining (staged-render, ADR-056) — keeper-задача активного
// Passage видит `register.<prev>.*` keeper-задач прошлых Passage через ИЗОЛИРОВАННЫЙ
// канал [RenderInput.KeeperRegister] (stage-loop переливает туда keeperRegisterBucket).
// Канал отделён от плоской Register СОЗНАТЕЛЬНО: host-fallback ([hostRegister]) остаётся
// на Register, поэтому host-задача смешанного Passage НЕ читает keeper-register при
// пустом per-host bucket. Пусто (P0, N=1, не-staged, host-only Passage) → fallback на
// плоскую Register (backward-compat: trial/push/прочие caller-ы, выставляющие только
// Register, видят register тем же путём БИТ-В-БИТ).
func keeperVars(in RenderInput) cel.Vars {
	inc := map[string]any{
		"name":            in.Incarnation.Name,
		"service":         in.Incarnation.Service,
		"service_version": in.Incarnation.ServiceVersion,
		"host_count":      0,
	}
	if in.State != nil {
		inc["state"] = in.State
	}
	reg := in.Register
	if len(in.KeeperRegister) > 0 {
		reg = in.KeeperRegister
	}
	return cel.Vars{
		Input:       in.Input,
		Register:    reg,
		Incarnation: inc,
		Essence:     in.Essence,
		Ctx:         in.Ctx,
	}
}

// resolveCovenList вычисляет элементы `on: [...]`: статические kebab-метки — как
// есть; CEL-обёртки `${ … }` — через интерполяцию (контекст без soulprint:
// `on:` резолвится один раз на прогон, не per-host). Корневая метка
// `incarnation.name` НЕ имеет спец-обработки — попадает в список наравне с
// прочими: фильтр по ней безопасен и эквивалентен «весь incarnation», т.к.
// каждый хост roster-а несёт корневую метку (rosterSQL `WHERE $1 = ANY(coven)`,
// ADR-008).
func resolveCovenList(engine *cel.Engine, in RenderInput, items []any) ([]string, error) {
	// on: резолвится не per-host — soulprint в контексте недоступен.
	vars := cel.Vars{
		Input:    in.Input,
		Register: in.Register,
		Incarnation: map[string]any{
			"name":            in.Incarnation.Name,
			"service":         in.Incarnation.Service,
			"service_version": in.Incarnation.ServiceVersion,
		},
		Ctx: in.Ctx,
	}

	out := make([]string, 0, len(items))
	for i, raw := range items {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("render: on[%d] имеет тип %T, ожидалась строка-coven", i, raw)
		}
		val, err := engine.EvalInterpolation(s, vars)
		if err != nil {
			return nil, fmt.Errorf("render: on[%d] %q: %w", i, s, err)
		}
		coven, ok := val.(string)
		if !ok {
			return nil, fmt.Errorf("render: on[%d] %q вычислился в %T, ожидалась строка-coven", i, s, val)
		}
		out = append(out, coven)
	}
	return out, nil
}

// filterByCovens оставляет хосты, у которых присутствуют ВСЕ метки covens —
// AND-пересечение (orchestration.md §3; [ADR-040] amendment 2026-05-27
// «Multi-label семантика внутри одного списка»). Пустой covens → roster без
// изменений. Зеркалит [topology.Resolver.FilterByCovens] как чистая функция без
// зависимости от *Resolver (pipeline резолвер не держит).
//
// Security-инвариант: AND-семантика fail-closed — перечисление меток не
// расширяет scope.
func filterByCovens(hosts []*topology.HostFacts, covens []string) []*topology.HostFacts {
	if len(covens) == 0 {
		return hosts
	}
	out := make([]*topology.HostFacts, 0, len(hosts))
	for _, h := range hosts {
		if hostHasAllCovens(h.Coven, covens) {
			out = append(out, h)
		}
	}
	return out
}

// hostHasAllCovens — AND-предикат: все метки required присутствуют в hostCoven.
// Линейное сканирование (см. парную функцию в topology) на типичных размерах
// быстрее map-индекса.
func hostHasAllCovens(hostCoven, required []string) bool {
	for _, want := range required {
		found := false
		for _, c := range hostCoven {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// sidsOf извлекает отсортированные SID-ы хостов (детерминизм DispatchPlan).
func sidsOf(hosts []*topology.HostFacts) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.SID
	}
	sort.Strings(out)
	return out
}

// applyRunOnce реализует `run_once: true` (orchestration.md §2.2.2): шаг идёт
// ровно на ОДНОМ хосте — первом по SID из резолва on:+where:. >1 хоста в
// таргете — норма (берём детерминированно первый). 0 хостов — оставляем пустой
// таргет (run_once не вводит собственной политики пустого таргета, §5).
//
// run_once==false → таргет без изменений.
func applyRunOnce(targeted []*topology.HostFacts, runOnce bool) []*topology.HostFacts {
	if !runOnce || len(targeted) <= 1 {
		return targeted
	}
	first := targeted[0]
	for _, h := range targeted[1:] {
		if h.SID < first.SID {
			first = h
		}
	}
	return []*topology.HostFacts{first}
}

// serialWidth вычисляет ширину волны `serial:` (orchestration.md §2.2.1) из
// значения `serial:` (int >= 1 или percent-string "<N>%") против числа
// таргетированных хостов n. Возврат:
//   - 0 — serial: не задан (nil); вся ширина таргета в одной волне.
//   - >=1 — число хостов в волне (≤ n): для percent — округление вверх,
//     минимум 1; для целого — само N (диспатчер сам ограничит ≤ n).
//
// config-валидатор (validateSerialField) уже гарантировал форму значения
// (int >= 1 либо "<N>%", N=1..99), поэтому здесь — чистое вычисление без
// повторной валидации; нераспознанная форма → 0 (трактуется как «не задан»,
// fail-safe — не дробить).
func serialWidth(serial any, n int) int {
	switch v := serial.(type) {
	case nil:
		return 0
	case int:
		return v
	case int64:
		return int(v)
	case uint64:
		return int(v)
	case string:
		return percentWidth(v, n)
	default:
		return 0
	}
}

// percentWidth переводит percent-форму "<N>%" в число хостов волны: ceil(n*N/100),
// минимум 1. Разбор формы — единый config.ParseSerialPercent (тот же источник
// правды, что и config-валидатор). Невалидная форма (не должна доходить после
// config-валидатора) → 0.
func percentWidth(s string, n int) int {
	pct, ok := config.ParseSerialPercent(s)
	if !ok {
		return 0
	}
	w := (n*pct + 99) / 100 // ceil(n*pct/100)
	if w < 1 {
		w = 1
	}
	return w
}
