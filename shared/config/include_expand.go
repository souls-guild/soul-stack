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
// плоско. На include-задаче допустимы ТОЛЬКО поля `include:` и `name:` —
// whitelist; любой другой непустой модификатор scope/контроля раскрытие отвергает
// диагностикой `include_modifier_unsupported`, чтобы не потерять scope молча.
func ExpandIncludes(tasks []Task, resolve IncludeResolver) ([]Task, []diag.Diagnostic) {
	e := &includeExpander{resolve: resolve}
	out := e.expand(tasks, nil)
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
}

// expand рекурсивно раскрывает список задач. stack — активная цепочка
// display-путей (для cycle-detection и глубины); nil на верхнем уровне.
func (e *includeExpander) expand(tasks []Task, stack []string) []Task {
	out := make([]Task, 0, len(tasks))
	for i := range tasks {
		task := tasks[i]

		// Не-include или block: splice не нужен. block раскрывается в слайсе C;
		// здесь passthrough, его отвергнет render-guard (ErrUnsupportedDSL) —
		// рекурсия в block до слайса C бесполезна и могла бы выдать include-
		// ошибку раньше понятной «block вне pilot».
		if task.Include == nil {
			out = append(out, task)
			continue
		}

		expanded, ok := e.expandOne(task, stack)
		if !ok {
			// Диагностика уже зафиксирована; задачу не вклеиваем (нечего).
			continue
		}
		out = append(out, expanded...)
	}
	return out
}

// expandOne раскрывает одну include-задачу: проверяет модификаторы, резолвит и
// парсит цель, детектит цикл/глубину, рекурсивно раскрывает её задачи.
func (e *includeExpander) expandOne(task Task, stack []string) ([]Task, bool) {
	name := task.Include.Include

	if reason := includeModifierReason(task); reason != "" {
		e.addError("include_modifier_unsupported",
			fmt.Sprintf("include %q несёт %s — проброс scope/контроля через include вне слайса B; вынеси модификатор на module-задачу подключённого файла", name, reason),
			"")
		return nil, false
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

	return e.expand(parsed, append(append([]string(nil), stack...), display)), true
}

// includeModifierReason возвращает человекочитаемую причину, если include-задача
// несёт любое поле помимо `include:`/`name:`. На чистой include-задаче допустимы
// только эти два поля; любой другой непустой модификатор scope/контроля splice'ом
// потерялся бы молча — раскрытие его отвергает. Whitelist (а не blacklist)
// устойчив к будущим полям Task: новое поле по умолчанию запрещено на include.
// Пустая строка — задача чистая.
func includeModifierReason(task Task) string {
	switch {
	case task.Loop != nil:
		return "loop: (слайс E)"
	case len(task.Vars) > 0:
		return "vars:"
	case task.When != "":
		return "when:"
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
