package config

import (
	"fmt"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// IncludeResolver резолвит include-цель по имени файла в её содержимое и
// канонический display-путь. Display-путь — стабильный идентификатор источника
// (используется в детекции циклов и в диагностике), не обязательно файловый: для
// двухуровневого scenario-резолва (orchestration.md §6) это resolved-путь
// «локально или service-level», для within-destiny — путь внутри каталога
// destiny.
//
// Резолвер инкапсулирует ВЕСЬ I/O и весь выбор источника (двухуровневый fallback,
// securejoin-кламп). [ExpandIncludes] остаётся чистой над содержимым: парсит,
// раскрывает, детектит циклы.
type IncludeResolver func(name string) (data []byte, displayPath string, err error)

// maxIncludeDepth — жёсткий потолок глубины include-цепочки. Защита-страховка
// поверх cycle-detection (visited-stack): даже без прямого цикла бесконтрольно
// глубокая цепочка — почти всегда ошибка автора, а не легитимная композиция.
const maxIncludeDepth = 32

// ExpandIncludes раскрывает include-задачи в ПЛОСКИЙ список задач до фазы
// рендера (orchestration.md §6, destiny/tasks.md §4). Каждая include-задача
// заменяется задачами подключённого файла inline, на их месте; вложенные
// include раскрываются рекурсивно.
//
// resolve инкапсулирует выбор источника (двухуровневый scenario-резолв или
// within-destiny) и I/O. Подключённый файл парсится тем же task-парсером
// ([LoadDestinyTasksFromBytes]) — top-level YAML-список задач без обёртки.
//
// Циклы (a→b→a, прямой self-include) детектируются по display-пути через
// visited-stack: повторное вхождение пути в активную цепочку → ошибка
// `include_cycle` (не бесконечная рекурсия). Глубина ограничена
// [maxIncludeDepth].
//
// Контракт возврата:
//   - parse/cycle/depth/resolve-ошибки подключённых файлов → diagnostics уровня
//     error (caller отбраковывает через [diag.HasErrors]); tasks возвращается
//     максимально раскрытым (для частичной диагностики).
//   - error != nil — никогда (зарезервировано симметрично прочим Load*).
//
// Splice-семантика (слайс B): чистый `include: <file>` (опц. `name:`) splice'ится
// плоско. На include-задаче допустимы поля `include:`/`name:` И `when:` (условный
// include, ADR-009 amendment) — whitelist; любой другой непустой модификатор
// scope/контроля раскрытие отвергает диагностикой `include_modifier_unsupported`,
// чтобы не потерять scope молча.
//
// Условный include (`when:` на include-задаче): include-when ОБЯЗАН быть
// статическим (input./essence./incarnation./vars. — [IsStaticIncludeWhen]),
// поскольку раскрытие идёт ДО фазы Stratify, когда register ещё не собран, а
// per-host soulprint неизвестен. Динамический when → `include_when_dynamic_unsupported`.
// Статический when и id группы протаскиваются в КАЖДУЮ вклеенную задачу
// (Task.IncludeWhen/IncludeGroupID) — keeper-side render дропает всю группу одним
// вычислением include-when (group-drop, реальное исключение из плана).
//
// Каскад вложенных условных include: эффективный include-when ВЛОЖЕННОЙ условной
// группы = конъюнкция предков `(<ancestor include-when>) && (<inner include-when>)`.
// Накопленный ancestor-when протаскивается вниз по рекурсии раскрытия; вложенная
// группа получает СВОЙ group-id, чей include-when кодирует полную конъюнкцию
// предков. Тогда дроп родителя (например, `outer=='no'`) каскадит на потомка
// естественно: его конъюнктивный include-when тоже вычисляется в false.
func ExpandIncludes(tasks []Task, resolve IncludeResolver) ([]Task, []diag.Diagnostic) {
	e := &includeExpander{resolve: resolve}
	out := e.expand(tasks, nil, "")
	// Уникальность адресного пространства подписки (register ∪ id) по ПЛОСКОМУ
	// списку прогона: per-file validateTaskRefs ловит дубль в пределах одного
	// файла, но не между основным файлом и раскрытым include (каждый файл там
	// валидируется в своём scope). Эта проверка работает на финальном плоском
	// `[]Task` — единый прогон после flatten — и ловит cross-include дубль.
	// Координат line/col на этом уровне нет (раскрытие стёрло AST-позиции);
	// диагностика адресует по имени.
	if !diag.HasErrors(e.diags) {
		e.diags = append(e.diags, validateFlatTaskAddresses(out)...)
	}
	return out, e.diags
}

// validateFlatTaskAddresses проверяет уникальность адресного пространства
// подписки `register ∪ id` (destiny/tasks.md §8) по плоскому списку задач
// прогона — после раскрытия include. Дубль (два register, два id, либо
// пересечение register/id) → duplicate_task_address. Рекурсивно обходит
// вложенные block: (его адреса живут в том же плоском пространстве плана).
//
// Вызывается на выходе ExpandIncludes только при отсутствии error-диагностик
// раскрытия: на полуразвёрнутом списке (parse/cycle/resolve-фейл) проверять
// адреса бессмысленно. Per-file дубли (в одном файле) уже отловлены
// validateTaskRefs при загрузке файла — здесь ловятся именно cross-file дубли,
// поэтому одиночный файл без include эта проверка дублирующих диагностик не даёт
// (валидный per-file список уникален и тут).
func validateFlatTaskAddresses(tasks []Task) []diag.Diagnostic {
	seen := map[string]bool{}
	var out []diag.Diagnostic
	collectFlatAddresses(tasks, seen, &out)
	return out
}

// collectFlatAddresses наполняет seen именами register/id из плоского списка
// (рекурсивно через block:); повторное имя → duplicate_task_address. Порядок
// детерминирован обходом списка.
func collectFlatAddresses(tasks []Task, seen map[string]bool, out *[]diag.Diagnostic) {
	for i := range tasks {
		t := &tasks[i]
		for _, addr := range []string{t.Register, t.ID} {
			if addr == "" {
				continue
			}
			if seen[addr] {
				*out = append(*out, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
					Code:    "duplicate_task_address",
					Message: fmt.Sprintf("task address %q (register/id) is declared more than once in this plan after include expansion", addr),
					Hint:    "register and id share one subscription address space across the flattened run (main + included files) — a duplicate makes \"alert on task X\" ambiguous; rename one",
				})
				continue
			}
			seen[addr] = true
		}
		if t.Block != nil {
			collectFlatAddresses(t.Block.Block, seen, out)
		}
	}
}

type includeExpander struct {
	resolve IncludeResolver
	diags   []diag.Diagnostic
	// lastGroupID — счётчик id условных include-групп (carry-through group-drop).
	// Монотонно растёт на КАЖДЫЙ include с непустым `when:`; 0 зарезервирован за
	// «вне условного include» (Task.IncludeGroupID==0). Вложенные условные include
	// получают разные id, дроп каждого — независимое вычисление своего include-when.
	lastGroupID int
}

// expand рекурсивно раскрывает список задач. stack — активная цепочка
// display-путей (для cycle-detection и глубины); nil на верхнем уровне.
// ancestorWhen — накопленный include-when условных include-предков (конъюнкция
// для каскадного дропа); "" на верхнем уровне и под безусловными include.
func (e *includeExpander) expand(tasks []Task, stack []string, ancestorWhen string) []Task {
	out := make([]Task, 0, len(tasks))
	for i := range tasks {
		task := tasks[i]

		// Не-include или block: splice не нужен. block раскрывается в render-фазе
		// (renderBlockTask, как loop) — здесь passthrough. within-block include
		// в pilot C1 не поддержан (guardPilotBlockChild отвергает include-потомок
		// как ErrUnexpandedInclude) — рекурсия в block на этом слое не нужна.
		if task.Include == nil {
			out = append(out, task)
			continue
		}

		expanded, ok := e.expandOne(task, stack, ancestorWhen)
		if !ok {
			// Диагностика уже зафиксирована; задачу не вклеиваем (нечего).
			continue
		}
		out = append(out, expanded...)
	}
	return out
}

// conjoinIncludeWhen строит эффективный include-when вложенной условной группы:
// конъюнкция накопленного ancestor-when и собственного include-when. Каждый
// операнд оборачивается в скобки, чтобы CEL-приоритет операторов внутри предиката
// (например `a || b`) не «протёк» через &&. Пустой ancestor (верхний уровень или
// безусловный родитель) → собственный when без обёртки (single-level не меняется).
func conjoinIncludeWhen(ancestorWhen, ownWhen string) string {
	if ancestorWhen == "" {
		return ownWhen
	}
	return fmt.Sprintf("(%s) && (%s)", ancestorWhen, ownWhen)
}

// stampIncludeGroup протаскивает include-when и id группы в КАЖДУЮ вклеенную
// задачу (рекурсивно через block:, чтобы потомки block-задачи внутри условного
// include тоже дропались целиком). Вложенный условный include уже проставил СВОЙ
// (более конкретный) IncludeGroupID на свои задачи раньше (recursion expandOne →
// expand → stampIncludeGroup), поэтому здесь НЕ перезаписываем уже проставленную
// группу. Дроп каждого уровня каскадит через КОНЪЮНКЦИЮ: вложенная группа уже
// застамплена эффективным include-when `(ancestor) && (own)` (conjoinIncludeWhen),
// поэтому ложный предок гасит и потомка, даже если собственный when потомка истинен.
// Зеркалит идею block.when-инжекта (mergeBlockInheritance), но через отдельную
// carry-through-ось, не через AND во when самой задачи (render дропает группу до
// emitStaticWhenSkip по её IncludeWhen, а не гасит её placeholder-ом).
func stampIncludeGroup(tasks []Task, when string, groupID int) {
	for i := range tasks {
		t := &tasks[i]
		if t.IncludeGroupID != 0 {
			continue // вложенный условный include уже застампил свою группу.
		}
		t.IncludeWhen = when
		t.IncludeGroupID = groupID
		if t.Block != nil {
			stampIncludeGroup(t.Block.Block, when, groupID)
		}
	}
}

// expandOne раскрывает одну include-задачу: проверяет модификаторы, резолвит и
// парсит цель, детектит цикл/глубину, рекурсивно раскрывает её задачи.
//
// ancestorWhen — накопленный include-when условных предков. Если у этого include
// есть собственный `when:`, эффективный include-when группы = конъюнкция
// `(ancestorWhen) && (own)` (conjoinIncludeWhen) — именно она стампится в задачи и
// протаскивается дальше вниз. Если `when:` пуст (безусловный include), группа не
// заводится, но ancestorWhen протаскивается дальше БЕЗ изменения — потомок-условный
// получит конъюнкцию с предком через своё раскрытие.
func (e *includeExpander) expandOne(task Task, stack []string, ancestorWhen string) ([]Task, bool) {
	name := task.Include.Include

	if reason := includeModifierReason(task); reason != "" {
		e.addError("include_modifier_unsupported",
			fmt.Sprintf("include %q несёт %s — проброс scope/контроля через include вне слайса B; вынеси модификатор на module-задачу подключённого файла", name, reason),
			"")
		return nil, false
	}

	// Условный include (`when:` на include): предикат ОБЯЗАН быть статическим —
	// include раскрывается ДО Stratify, register предыдущих задач ещё не собран,
	// per-host soulprint неизвестен. Динамический when (register./soulprint.) →
	// include_when_dynamic_unsupported. Допустимый when резолвится в group-id,
	// который stampIncludeGroup протащит в каждую вклеенную задачу.
	//
	// effectiveWhen — конъюнкция с накопленным ancestor-when (каскадный дроп
	// вложенных условных include): вложенная группа дропается, если ложен ЛЮБОЙ
	// предок ИЛИ собственный предикат. Этот же effectiveWhen протаскивается вниз
	// как ancestorWhen для следующего уровня раскрытия.
	groupID := 0
	effectiveWhen := ancestorWhen
	if task.When != "" {
		if !IsStaticIncludeWhen(task.When) {
			e.addError("include_when_dynamic_unsupported",
				fmt.Sprintf("include %q несёт динамический when %q (ссылка на register./soulprint.) — include раскрывается ДО стратификации, доступен только статический предикат input./essence./incarnation./vars.", name, task.When),
				"замените на статический предикат (input./essence./incarnation.) либо перенесите условие на module-задачу подключённого файла через when:")
			return nil, false
		}
		e.lastGroupID++
		groupID = e.lastGroupID
		effectiveWhen = conjoinIncludeWhen(ancestorWhen, task.When)
	}

	data, display, err := e.resolve(name)
	if err != nil {
		e.addError("include_resolve_failed", fmt.Sprintf("include %q: %v", name, err), "")
		return nil, false
	}

	if depth := len(stack); depth >= maxIncludeDepth {
		e.addError("include_depth_exceeded",
			fmt.Sprintf("include %q: превышена максимальная глубина %d (цепочка: %v)", name, maxIncludeDepth, stack),
			"")
		return nil, false
	}
	for _, prev := range stack {
		if prev == display {
			e.addError("include_cycle",
				fmt.Sprintf("include %q образует цикл: %s уже в активной цепочке %v", name, display, stack),
				"разорви циклическую зависимость include")
			return nil, false
		}
	}

	parsed, diags, _ := LoadDestinyTasksFromBytes(display, data, ValidateOptions{})
	if diag.HasErrors(diags) {
		e.diags = append(e.diags, diags...)
		return nil, false
	}

	expanded := e.expand(parsed, append(append([]string(nil), stack...), display), effectiveWhen)
	if groupID != 0 {
		stampIncludeGroup(expanded, effectiveWhen, groupID)
	}
	return expanded, true
}

// includeModifierReason возвращает человекочитаемую причину, если include-задача
// несёт любое поле помимо `include:`/`name:`/`when:`. На include-задаче допустимы
// эти поля (`when:` — условный include, ADR-009 amendment); любой другой непустой
// модификатор scope/контроля splice'ом потерялся бы молча — раскрытие его отвергает.
// Whitelist (а не blacklist) устойчив к будущим полям Task: новое поле по умолчанию
// запрещено на include. Пустая строка — задача чистая.
//
// `when:` НЕ в этом списке (условный include) — его статичность проверяет
// отдельно expandOne (IsStaticIncludeWhen → include_when_dynamic_unsupported для
// динамического). `loop:` остаётся запрещён: loop на include не реализован (drift
// docs↔code, docs/destiny/tasks.md §7) → include_modifier_unsupported.
func includeModifierReason(task Task) string {
	switch {
	case task.Loop != nil:
		return "loop: (слайс E)"
	case len(task.Vars) > 0:
		return "vars:"
	case task.Parallel:
		return "parallel:"
	case task.Register != "":
		return "register:"
	case len(task.Output) > 0:
		return "output:"
	case task.NoLog:
		return "no_log:"
	case len(task.OnChanges) > 0:
		return "onchanges:"
	case len(task.OnFail) > 0:
		return "onfail:"
	case task.Require != nil:
		return "require:"
	case task.ChangedWhen != "":
		return "changed_when:"
	case task.FailedWhen != "":
		return "failed_when:"
	case task.Retry != nil:
		return "retry:"
	case task.Timeout != "":
		return "timeout:"
	case task.On != nil:
		return "on:"
	case task.Where != "":
		return "where:"
	case task.Serial != nil:
		return "serial:"
	case task.RunOnce:
		return "run_once:"
	}
	return ""
}

// addError фиксирует семантическую диагностику раскрытия (cycle/depth/modifier/
// resolve): это cross-field/cross-file инварианты, а не проверка структуры узла.
func (e *includeExpander) addError(code, msg, hint string) {
	e.diags = append(e.diags, diag.Diagnostic{
		Level:   diag.LevelError,
		Phase:   diag.PhaseSemanticValidate,
		Code:    code,
		Message: msg,
		Hint:    hint,
	})
}
