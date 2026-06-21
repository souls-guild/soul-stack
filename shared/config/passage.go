package config

// Passage-стратификация ([ADR-056](../../docs/adr/0056-staged-render-passage.md)).
// ЧИСТАЯ функция над планом задач прогона ([]Task после ExpandIncludes): вычисляет
// для каждой задачи её Passage-индекс (0-based) по графу cross-task register-
// зависимости и валидирует граф (цикл / висячая ссылка). НИЧЕГО не исполняет и не
// рендерит — stage-loop (render→dispatch→barrier→повтор) живёт keeper-side.
//
// Живёт в shared/config (а не keeper-internal): один и тот же граф register-
// зависимости обязан строиться (а) keeper-рантаймом перед dispatch, (б) keeper-
// тестами, (в) ОФЛАЙН soul-lint-ом ДО apply. Дубль логики между ними = риск
// silent-wrong-target (расхождение графа). keeper/internal/render держит тонкие
// alias-ы (Stratify/Passage/коды) на эти символы.
//
// Зачем (ADR-056, §«Риски — silent-wrong-target»). Задача, читающая `register.X`
// в любом passage-определяющем register-источнике, ОБЯЗАНА исполниться строго
// ПОСЛЕ probe-шага, эмитящего `register: X` (probe и его потребитель не могут
// оказаться в одном Passage) — иначе `where:` отберёт хосты по пустому/устаревшему
// register и разрушительная операция уйдёт не на те хосты МОЛЧА.
//
// Канонический реестр passage-определяющих register-источников Task (ADR-056):
//
//	where · vars · params · apply.input · output · loop.items · loop.when · block (рекурсия).
//
// Все они резолвятся Keeper-side ДО dispatch, поэтому обязаны видеть register
// предыдущего Passage → определяют Passage.
//
// requisites (`onchanges`/`onfail`/`require`) — НЕ passage-определяющие (адресные
// ссылки, не интерполяция). Flow-control CEL `when`/`changed_when`/`failed_when`/
// `retry.until` тоже НЕ определяет passage (ADR-056:85) — это Soul-side per-task
// gating ([ADR-012(d)], исполняется в срезе одного ApplyRequest из своего register).
// Они НЕ входят в collectTaskReads: иначе register-зависимый `when` расщеплял бы
// probe и same-passage потребителя по разным Passage, где Soul cross-passage
// register не видит → `no such key` (FC-5). Genuinely cross-passage `when`
// (probe в раннем Passage по ДРУГОЙ причине) — UNSUPPORTED, ловится отдельным
// детектором CrossPassageWhenGating (fail-closed, симметрия within-block).
//
// Инвариант сопровождения: новое keeper-рендеримое passage-определяющее register-
// читающее поле Task обязано синхронно появиться в collectTaskReads (тут) И в
// collectRefs (cross-ref-валидатор task_refs.go) И в реестре ADR-056. Flow-control
// (`when`/`changed_when`/`failed_when`) — НАМЕРЕННАЯ асимметрия: ∈ collectRefs
// (refs — проверка существования register), но ∉ collectTaskReads (не определяет
// Passage). Guard-тест reads ⊆ refs (а для flow-control reads ⊊ refs).

import (
	"fmt"
	"sort"
)

// Passage — стратификационный план прогона: для каждой top-level задачи её
// passage-индекс (0-based) + общее число Passage. Index ссылается на позицию в
// плане (Tasks после ExpandIncludes), совпадает с RenderedTask.Index.
//
// TaskPassage[i] — passage i-й задачи плана. Count = max(TaskPassage)+1 (>=1).
// Count==1 — fast-path: ни одной cross-task register-зависимости, все задачи в
// passage 0, поведение идентично up-front render (backward-compat).
type Passage struct {
	TaskPassage []int
	Count       int
}

// StratifyError — ошибка стратификации register-графа. Несёт код (для caller-а:
// keeper run.go → render_failed; soul-lint → офлайн-диагностика) и человекочитаемое
// сообщение. Это невалидный граф зависимостей автора сценария (цикл / ссылка на
// несуществующий register), который обязан остановить прогон ЯВНО, а не молча
// (симметрия unknown_register_reference в config-валидаторе и silent-wrong-target).
type StratifyError struct {
	Code string
	Msg  string
}

func (e *StratifyError) Error() string { return e.Msg }

// StratifyError-коды.
const (
	// StratifyCycle — register-зависимость по кругу (probe A читает register.B,
	// probe B читает register.A): топологического порядка нет.
	StratifyCycle = "register_dependency_cycle"
	// StratifyUnknownRegister — задача читает register.X, но НИ ОДНА задача плана
	// не эмитит `register: X`. Дублирует cross-ref-валидатор config-а (страховка:
	// стратификация обязана падать явно, а не молча стратифицировать по неполному
	// графу — silent-wrong-target).
	StratifyUnknownRegister = "unknown_register_reference"
	// CodeWithinBlockRegisterDependency — потомок block: читает register, эмитнутый
	// СОСЕДНИМ потомком ТОГО ЖЕ блока. block атомарен по Passage (весь fan-out в
	// одном Passage, ADR-056), peer-register доступен только Soul-side ПОСЛЕ probe,
	// а where/when/params резолвятся Keeper-side ДО dispatch → where отберёт хосты
	// по устаревшему/внешнему register молча (silent-wrong-target). Fail-closed
	// reject офлайн (soul-lint) и runtime-страховкой (run.go), а не молчаливый
	// мисфайр. Лечится выносом probe на top-level (probe и потребитель — разные
	// Passage; Stratify тогда упорядочит их штатно). Имя кода держим отдельным от
	// функции-детектора WithinBlockRegisterDependency (одноимённый const+func в
	// одном пакете недопустим в Go).
	CodeWithinBlockRegisterDependency = "within_block_register_dependency"
	// CodeCrossPassageWhenGating — задача гейтит `when:`/`changed_when:`/
	// `failed_when:` по register, эмитнутому в БОЛЕЕ РАННЕМ Passage (ADR-056:85,
	// FC-5). flow-control = Soul-side per-task gating ([ADR-012(d)]), видит только
	// register СВОЕГО Passage; cross-passage register ему недоступен (другой
	// ApplyRequest) → `no such key` молча. После narrow-fix flow-control сам Passage
	// НЕ расщепляет, но probe мог уехать в ранний Passage по ДРУГОЙ причине (иная
	// задача с `where: register.X`). Fail-closed reject (офлайн soul-lint + runtime
	// keeper-страховка), симметрия CodeWithinBlockRegisterDependency. Лечится
	// where: (cross-task таргетинг) или register.self (same-task gating).
	CodeCrossPassageWhenGating = "cross_passage_when_unsupported"
)

// Stratify вычисляет passage-индексы для плана задач прогона по графу cross-task
// register-зависимости. Возвращает [Passage] либо *[StratifyError] (цикл / висячая
// ссылка). tasks — плоский top-level список задач (после ExpandIncludes);
// nil/пустой → Passage{Count: 1}.
//
// Алгоритм (топологическая стратификация + program-order для эмиттеров):
//
//  1. Эмиттеры: register-имя → индекс задачи, его эмитящей (`register: X`),
//     last-wins при дубле.
//  2. Читатели: для каждой задачи — набор cross-task register-имён из passage-
//     определяющих источников (where/vars/params/apply.input/output/loop.items,
//     рекурсивно через block; `register.self` исключён).
//  3. passage(T) — мемоизированная рекурсия по двум видам рёбер:
//     register-ребро (passage(T) >= 1 + passage(эмиттер X)) и program-order ребро
//     для probe-эмиттера (passage(probe) >= passage любой предшествующей задачи —
//     даёт re-probe в Passage ПОСЛЕ действия, restart-кейс).
//  4. Цикл по register-рёбрам → StratifyCycle. Висячая ссылка → StratifyUnknownRegister.
func Stratify(tasks []Task) (Passage, error) {
	n := len(tasks)
	if n == 0 {
		return Passage{TaskPassage: nil, Count: 1}, nil
	}

	emitter := emitterIndex(tasks)
	reads := make([][]string, n)
	emits := make([]bool, n)
	for i := range tasks {
		reads[i] = taskRegisterReads(&tasks[i])
		emits[i] = taskEmitsRegister(&tasks[i])
	}

	// Висячая ссылка: читатель register.X, которого никто не эмитит. Падаем ДО
	// топосорта — иначе стратификация пошла бы по неполному графу.
	for i := range reads {
		for _, name := range reads[i] {
			if _, ok := emitter[name]; !ok {
				return Passage{}, &StratifyError{
					Code: StratifyUnknownRegister,
					Msg: fmt.Sprintf(
						"task #%d reads register %q, which no task in the run declares — staged-render cannot stratify (would target on an unresolved register)",
						i, name),
				}
			}
		}
	}

	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := make([]int, n)
	memo := make([]int, n)

	var visit func(i int) (int, error)
	visit = func(i int) (int, error) {
		switch state[i] {
		case done:
			return memo[i], nil
		case onStack:
			return 0, &StratifyError{
				Code: StratifyCycle,
				Msg:  fmt.Sprintf("register dependency cycle detected at task #%d — tasks read each other's register in a loop, no Passage order exists", i),
			}
		}
		state[i] = onStack

		level := 0
		for _, name := range reads[i] {
			src := emitter[name]
			if src == i {
				continue
			}
			p, err := visit(src)
			if err != nil {
				return 0, err
			}
			if p+1 > level {
				level = p + 1
			}
		}
		if emits[i] {
			for j := 0; j < i; j++ {
				p, err := visit(j)
				if err != nil {
					return 0, err
				}
				if p > level {
					level = p
				}
			}
		}

		state[i] = done
		memo[i] = level
		return level, nil
	}

	maxP := 0
	for i := 0; i < n; i++ {
		p, err := visit(i)
		if err != nil {
			return Passage{}, err
		}
		if p > maxP {
			maxP = p
		}
	}

	return Passage{TaskPassage: memo, Count: maxP + 1}, nil
}

// emitterIndex строит карту register-имя → индекс top-level задачи, эмитящей его.
// register, объявленный внутри block:, адресуется плоско → приписывается индексу
// КОНТЕЙНЕРА (block — атомарная единица Passage). last-wins при дубле имени.
func emitterIndex(tasks []Task) map[string]int {
	idx := map[string]int{}
	for i := range tasks {
		for _, name := range taskEmittedRegisters(&tasks[i]) {
			idx[name] = i
		}
	}
	return idx
}

// taskEmittedRegisters — все register-имена, эмитимые задачей и её block-потомками.
func taskEmittedRegisters(t *Task) []string {
	var out []string
	if t.Register != "" {
		out = append(out, t.Register)
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			out = append(out, taskEmittedRegisters(&t.Block.Block[i])...)
		}
	}
	return out
}

// taskEmitsRegister — задача (или её block-потомок) эмитит хотя бы один register.
func taskEmitsRegister(t *Task) bool {
	return len(taskEmittedRegisters(t)) > 0
}

// taskRegisterReads — отсортированный уникальный набор cross-task register-имён,
// которые задача ЧИТАЕТ. Имена собственного register: задачи (и block-потомков)
// ИСКЛЮЧАЮТСЯ — это self-ссылка внутри одного среза, не cross-task ребро.
func taskRegisterReads(t *Task) []string {
	own := map[string]bool{}
	for _, name := range taskEmittedRegisters(t) {
		own[name] = true
	}
	seen := map[string]bool{}
	collectTaskReads(t, seen)
	out := make([]string, 0, len(seen))
	for name := range seen {
		if own[name] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// collectTaskReads наполняет seen register-именами, читаемыми задачей в passage-
// ОПРЕДЕЛЯЮЩИХ источниках (ADR-056-реестр): where / vars / params / apply.input /
// output / loop.items / loop.when, рекурсивно через block. Self-фильтрация — в
// taskRegisterReads.
//
// Flow-control CEL `when` / `changed_when` / `failed_when` / `retry.until` СЮДА НЕ
// ВХОДИТ (ADR-056:85 — flow-control НЕ определяет Passage). Это Soul-side per-task
// gating ([ADR-012(d)](../../docs/adr/0012-keeper-soul-grpc.md)): исполняется в
// срезе ОДНОГО ApplyRequest из register, накопленного его собственным Passage.
// Включение этих полей в passage-граф РАСЩЕПЛЯЛО probe и его same-passage when-
// потребителя по разным Passage (probe→Passage 0, when→Passage 1), где Soul cross-
// passage register не видит → `no such key` (FC-5). Это нарушало gating-семантику
// `when` вместо «conservative over-approx». `where` (таргетинг Keeper-side) cross-
// passage умеет (Keeper пере-рендерит с накопленным register), `when` — нет:
// асимметрия легитимна, поэтому where остаётся passage-определяющим, а when — нет.
//
// requisites (`onchanges`/`onfail`/`require`) сюда НЕ входят по той же причине
// (адресные ссылки, не интерполяция). loop.when ОСТАЁТСЯ passage-определяющим:
// fan-out цикла строится Keeper-side ДО dispatch (как loop.items), не Soul-side.
//
// Cross-ref-валидатор (config.collectRefs) при этом ОБХОДИТ when/changed_when/
// failed_when — они register-ЧИТАЮЩИЕ (проверка «register существует»), но НЕ
// passage-ОПРЕДЕЛЯЮЩИЕ. Эта асимметрия (refs ⊋ passage-reads) — намеренная;
// guard-инвариант её фиксирует (TestStratify_FlowControlInRefsNotPassageReads).
func collectTaskReads(t *Task, seen map[string]bool) {
	addCELRefs(t.Where, seen)
	if t.Loop != nil {
		addCELRefs(t.Loop.When, seen)
	}

	addMapRefs(t.Vars, seen)
	addMapRefs(t.Output, seen)
	if t.Loop != nil {
		addValueRefs(t.Loop.Items, seen)
	}
	if t.Module != nil {
		addMapRefs(t.Module.Params, seen)
	}
	if t.Apply != nil {
		addMapRefs(t.Apply.Input, seen)
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			collectTaskReads(&t.Block.Block[i], seen)
		}
	}
}

// addCELRefs извлекает cross-task register-имена из голой CEL-строки через
// канонический парсер ExtractRegisterRefs.
func addCELRefs(expr string, seen map[string]bool) {
	for _, name := range ExtractRegisterRefs(expr) {
		seen[name] = true
	}
}

// addMapRefs обходит map значений (vars/params/apply.input) и извлекает register-
// имена из строковых литералов (внутри `${ … }`-маркера). Рекурсивно в map/seq.
func addMapRefs(m map[string]any, seen map[string]bool) {
	for _, v := range m {
		addValueRefs(v, seen)
	}
}

// addValueRefs рекурсивно обходит any-значение (string / map / seq) и собирает
// register-имена из всех строк.
func addValueRefs(v any, seen map[string]bool) {
	switch t := v.(type) {
	case string:
		addCELRefs(t, seen)
	case map[string]any:
		for _, sub := range t {
			addValueRefs(sub, seen)
		}
	case []any:
		for _, sub := range t {
			addValueRefs(sub, seen)
		}
	}
}

// CrossPassageRequisite детектирует задачу, чей onchanges/onfail-источник лежит в
// ДРУГОМ Passage (ADR-056 amend, R2 — explicit-reject до полной keeper-side gating-
// поддержки R3). requisites (`onchanges:`/`onfail:`) — НЕ passage-определяющие
// (в граф Stratify не входят), поэтому их источник может оказаться в любом Passage.
// Если consumer и источник в РАЗНЫХ Passage, они едут РАЗНЫМИ ApplyRequest-ами:
// Soul gating одного Passage не видит register источника другого Passage →
// registerByIdx[remap=sentinel]=nil → onchanges-задача молча SKIPPED, onfail-rescue
// молча НЕ запускается. Фикс до R3 — fail-closed reject (симметрия
// serial_staged_unsupported), а не молчаливый мисфайр.
//
// passage — результат [Stratify] того же плана tasks. Возвращает координаты первой
// найденной cross-passage-связи (task-имя consumer-а, requisite-имя, его kind,
// passage обоих) и ok=true; ok=false — все requisites same-passage (R1-remap их
// чинит). N=1 (passage.Count==1) → ok=false всегда (один Passage).
func CrossPassageRequisite(tasks []Task, passage Passage) (info CrossPassageInfo, ok bool) {
	if passage.Count <= 1 || len(passage.TaskPassage) != len(tasks) {
		return CrossPassageInfo{}, false
	}
	emitter := emitterIndex(tasks)
	for i := range tasks {
		consumerPassage := passage.TaskPassage[i]
		for _, req := range taskRequisites(&tasks[i]) {
			srcIdx, known := emitter[req.name]
			if !known {
				// Висячий requisite (нет задачи с таким register:) — НЕ наша забота:
				// его ловит cross-ref-валидатор config-а (unknown_register_reference)
				// офлайн. Здесь интересует только cross-passage СУЩЕСТВУЮЩЕГО источника.
				continue
			}
			if srcPassage := passage.TaskPassage[srcIdx]; srcPassage != consumerPassage {
				return CrossPassageInfo{
					ConsumerName:    taskDisplayName(&tasks[i]),
					RequisiteName:   req.name,
					Kind:            req.kind,
					ConsumerPassage: consumerPassage,
					SourcePassage:   srcPassage,
				}, true
			}
		}
	}
	return CrossPassageInfo{}, false
}

// CrossPassageInfo — координаты cross-passage requisite-связи (для текста abort-а).
type CrossPassageInfo struct {
	ConsumerName    string // имя задачи-потребителя requisite
	RequisiteName   string // register-имя источника
	Kind            string // "onchanges" | "onfail"
	ConsumerPassage int
	SourcePassage   int
}

// CrossPassageWhenGating детектирует задачу, чей flow-control CEL (`when:`/
// `changed_when:`/`failed_when:`) ссылается на cross-task register, эмитнутый в
// БОЛЕЕ РАННЕМ Passage (ADR-056:85 amend, FC-5 — fail-closed reject genuinely
// cross-passage when-gating). После narrow-fix flow-control САМ Passage НЕ
// расщепляет (он ∉ collectTaskReads), поэтому register-зависимый `when` обычно
// едет SAME-passage с probe → Soul видит register → gating работает (как задумано
// [ADR-012(d)]). НО probe может оказаться в раннем Passage по ДРУГОЙ причине: иная
// задача с `where: register.X` (passage-определяющий) загнала эмиттер X в Passage 0,
// а when-потребитель уехал в Passage 1 по СВОЕЙ register-зависимости. Тогда
// `when: register.X` — genuinely cross-passage: consumer едет отдельным ApplyRequest,
// Soul его Passage не видит register X (он в registerByIdx другого Passage) →
// when молча eval-падает `no such key` / задача FAILED.
//
// where это умеет (Keeper пере-рендерит where с накопленным register предыдущего
// Passage ДО dispatch), when — нет (Soul-side, видит только свой Passage). Поэтому
// genuinely cross-passage flow-control gating = UNSUPPORTED → fail-closed reject
// (симметрия within_block_register_dependency и cross_passage_requisite), а не
// молчаливый мисфайр. Лечится: where: для cross-task register-таргетинга ИЛИ
// register.self для same-task gating.
//
// register.self НЕ считается (ExtractRegisterRefs его режет — same-task). Собственный
// register задачи тоже исключён (self-ссылка, не cross-task ребро). passage —
// результат [Stratify] того же плана. Возвращает координаты первой найденной связи
// и ok=true; ok=false — все flow-control register-ссылки same-passage. N=1
// (passage.Count==1) → ok=false всегда (один Passage, cross-passage невозможен).
func CrossPassageWhenGating(tasks []Task, passage Passage) (info CrossPassageWhenInfo, ok bool) {
	if passage.Count <= 1 || len(passage.TaskPassage) != len(tasks) {
		return CrossPassageWhenInfo{}, false
	}
	emitter := emitterIndex(tasks)
	for i := range tasks {
		consumerPassage := passage.TaskPassage[i]
		own := map[string]bool{}
		for _, name := range taskEmittedRegisters(&tasks[i]) {
			own[name] = true
		}
		for _, ref := range taskFlowControlReads(&tasks[i]) {
			if own[ref.name] {
				continue // собственный register задачи — self-ссылка, не cross-task.
			}
			srcIdx, known := emitter[ref.name]
			if !known {
				// Висячий register — НЕ наша забота: cross-ref-валидатор / Stratify
				// ловят unknown_register_reference офлайн. Здесь — только cross-passage
				// СУЩЕСТВУЮЩЕГО источника.
				continue
			}
			if srcPassage := passage.TaskPassage[srcIdx]; srcPassage != consumerPassage {
				return CrossPassageWhenInfo{
					ConsumerName:    taskDisplayName(&tasks[i]),
					RegisterName:    ref.name,
					Kind:            ref.kind,
					ConsumerPassage: consumerPassage,
					SourcePassage:   srcPassage,
				}, true
			}
		}
	}
	return CrossPassageWhenInfo{}, false
}

// CrossPassageWhenInfo — координаты cross-passage flow-control gating-связи (для
// текста abort-а / диагностики линтера).
type CrossPassageWhenInfo struct {
	ConsumerName    string // имя задачи с flow-control-предикатом
	RegisterName    string // register-имя, прочитанное предикатом
	Kind            string // "when" | "changed_when" | "failed_when"
	ConsumerPassage int
	SourcePassage   int
}

// flowControlRead — одна flow-control register-ссылка задачи (cross-task register-
// имя + поле-источник). retry.until НЕ включается: он гейтит retry внутри одной
// задачи по её СОБСТВЕННОМУ register.self (cross-task retry.until.register.X
// бессмыслен — задача не видит чужой результат в своём retry-цикле), поэтому
// register.self его покрывает, а cross-task ссылка в нём — отдельный класс ошибки
// (unknown/мисюз), не gating.
type flowControlRead struct {
	name string
	kind string
}

// taskFlowControlReads собирает cross-task register-имена из `when`/`changed_when`/
// `failed_when` задачи И её block-потомков (block — атомарная единица Passage,
// flow-control его потомков адресуется по тем же register-именам). register.self
// уже отфильтрован ExtractRegisterRefs. Сортировка имён внутри одного поля делает
// диагностику детерминированной.
func taskFlowControlReads(t *Task) []flowControlRead {
	var out []flowControlRead
	for _, kv := range []struct {
		expr string
		kind string
	}{
		{t.When, "when"},
		{t.ChangedWhen, "changed_when"},
		{t.FailedWhen, "failed_when"},
	} {
		for _, name := range ExtractRegisterRefs(kv.expr) {
			out = append(out, flowControlRead{name: name, kind: kv.kind})
		}
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			out = append(out, taskFlowControlReads(&t.Block.Block[i])...)
		}
	}
	return out
}

// taskRequisite — одна requisite-ссылка задачи (register-имя источника + kind).
type taskRequisite struct {
	name string
	kind string
}

// taskRequisites собирает onchanges/onfail-имена задачи И её block-потомков
// (requisites адресуются плоско по register-имени, block — атомарная единица
// Passage). require: НЕ включается: его семантика — порядок исполнения, не
// changed/failed-gating по registerByIdx (R2 строго про onchanges/onfail).
func taskRequisites(t *Task) []taskRequisite {
	var out []taskRequisite
	for _, name := range t.OnChanges {
		out = append(out, taskRequisite{name: name, kind: "onchanges"})
	}
	for _, name := range t.OnFail {
		out = append(out, taskRequisite{name: name, kind: "onfail"})
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			out = append(out, taskRequisites(&t.Block.Block[i])...)
		}
	}
	return out
}

// taskDisplayName — имя задачи для диагностики (name: либо register: либо "<unnamed>").
func taskDisplayName(t *Task) string {
	switch {
	case t.Name != "":
		return t.Name
	case t.Register != "":
		return t.Register
	default:
		return "<unnamed>"
	}
}

// WithinBlockInfo — координаты within-block register-зависимости (для текста
// abort-а / диагностики линтера).
type WithinBlockInfo struct {
	ReaderName   string // имя потомка-читателя register
	RegisterName string // peer-register, прочитанный читателем
	EmitterName  string // имя потомка-эмиттера того же блока (или его контейнера)
}

// WithinBlockRegisterDependency детектирует потомка block:, читающего register,
// эмитнутый СОСЕДНИМ потомком ТОГО ЖЕ блока (ADR-056, §«Риски — silent-wrong-target»).
// block — атомарная единица Passage: весь его fan-out (probe-потомок + потребитель)
// едет ОДНИМ ApplyRequest в одном Passage. peer-register становится доступен только
// Soul-side ПОСЛЕ probe, тогда как KEEPER-SIDE-резолвимые поля потребителя (where/
// params/vars/apply.input — те, что в collectTaskReads) резолвятся ДО dispatch блока —
// по пустому/внешнему/устаревшему register МОЛЧА. Stratify это не ловит: внутри-блочное
// ребро не пересекает границу top-level задач (emitterIndex клеймит весь register
// block-а индексом КОНТЕЙНЕРА). Поэтому отдельный fail-closed детектор.
//
// Flow-control `when`/`changed_when`/`failed_when` СЮДА НЕ входит (collectTaskReads его
// больше не возвращает, FC-5 narrow-fix): within-block `when: register.peer` ВАЛИДЕН —
// peer-probe исполняется тем же ApplyRequest ДО потребителя, Soul видит peer-register в
// накопленном срезе блока на момент eval (в отличие от Keeper-side where, который пуст).
//
// blockEmits каждого блока строится из ЕГО СТРУКТУРЫ (taskEmittedRegisters минус
// собственный register контейнера), рекурсивно вглубь — НЕ из глобального emitterIndex.
// Это критично: внешний top-level probe, эмитящий тот же register-имя, читать из block
// потомком ВАЛИДНО (probe — отдельный Passage до блока, restart-кейс) и обязан остаться
// ok==false. Сверка идёт только против register-ов, рождённых ВНУТРИ этого блока.
//
// Возвращает координаты первой найденной within-block-зависимости (имя читателя,
// peer-register, имя эмиттера) и ok=true; ok=false — ни один block-потомок не читает
// peer-register своего блока. План без block-задач → fast-path ok=false.
func WithinBlockRegisterDependency(tasks []Task) (info WithinBlockInfo, ok bool) {
	for i := range tasks {
		if tasks[i].Block == nil {
			continue
		}
		if bi, bad := blockPeerRegisterRead(&tasks[i]); bad {
			return bi, true
		}
	}
	return WithinBlockInfo{}, false
}

// blockPeerRegisterRead проверяет один block-контейнер: читает ли какой-либо его
// потомок (рекурсивно, любая глубина) register, рождённый ВНУТРИ этого блока соседним
// потомком. blockEmits — register-имена всего поддерева блока МИНУС собственный
// register контейнера (это структура ЭТОГО блока, не глобальный emitterIndex). Затем
// для каждого потомка: childReads (collectTaskReads — все 7 passage-источников;
// register.self уже отфильтрован ExtractRegisterRefs) пересекается с blockEmits, но БЕЗ
// собственного register потомка (peer, а не self) → reject. Вложенные блоки: их
// потомки тоже сверяются против blockEmits внешнего блока (peer внутри = peer снаружи,
// один Passage), плюс рекурсивный заход ловит peer-зависимость внутри вложенного блока.
func blockPeerRegisterRead(container *Task) (WithinBlockInfo, bool) {
	blockEmits := map[string]string{} // register-имя → имя эмитящего потомка
	for i := range container.Block.Block {
		collectBlockEmits(&container.Block.Block[i], blockEmits)
	}
	if len(blockEmits) == 0 {
		return WithinBlockInfo{}, false // блок ничего не эмитит — peer-зависимости быть не может.
	}

	for i := range container.Block.Block {
		if bi, bad := childPeerRead(&container.Block.Block[i], blockEmits); bad {
			return bi, true
		}
		// Вложенный блок: его внутренние peer-зависимости (полностью внутри него) —
		// отдельная проверка с собственным blockEmits.
		if container.Block.Block[i].Block != nil {
			if bi, bad := blockPeerRegisterRead(&container.Block.Block[i]); bad {
				return bi, true
			}
		}
	}
	return WithinBlockInfo{}, false
}

// collectBlockEmits наполняет emits register-именами, рождёнными внутри потомка блока
// (его собственный register: + register block-потомков рекурсивно), сопоставляя их с
// именем потомка-эмиттера для диагностики. last-wins при дубле (симметрия emitterIndex).
func collectBlockEmits(child *Task, emits map[string]string) {
	name := taskDisplayName(child)
	for _, reg := range taskEmittedRegisters(child) {
		emits[reg] = name
	}
}

// childPeerRead проверяет одного потомка блока (рекурсивно вглубь его собственных
// block-потомков): читает ли он register из blockEmits, который НЕ его собственный
// (peer, не self). Собственные register потомка исключаются — внутри-задачная self-
// ссылка не cross-task ребро (register.self уже отфильтрован, но потомок с собственным
// register: X, читающий register.X в другом поле, тоже не peer-зависимость).
func childPeerRead(child *Task, blockEmits map[string]string) (WithinBlockInfo, bool) {
	own := map[string]bool{}
	for _, reg := range taskEmittedRegisters(child) {
		own[reg] = true
	}
	seen := map[string]bool{}
	collectTaskReads(child, seen)
	reads := make([]string, 0, len(seen))
	for reg := range seen {
		reads = append(reads, reg)
	}
	sort.Strings(reads) // детерминизм диагностики при нескольких peer-ссылках.
	for _, reg := range reads {
		if own[reg] {
			continue
		}
		if emitter, peer := blockEmits[reg]; peer {
			return WithinBlockInfo{
				ReaderName:   taskDisplayName(child),
				RegisterName: reg,
				EmitterName:  emitter,
			}, true
		}
	}
	return WithinBlockInfo{}, false
}
