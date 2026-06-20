package config

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// validateTaskRefs — cross-task проверки списка задач плана (scenario/main.yml
// `tasks[]` или destiny `tasks/main.yml`), которые нельзя поднять при валидации
// одной задачи в отдельности (validateTaskNode):
//
//  1. duplicate_task_address — два+ задачи делят одно имя в адресном
//     пространстве подписки `register ∪ id` (два register, два id, либо
//     register одной задачи == id другой).
//  2. unknown_register_reference — ссылка на register-имя `register.<name>.*`,
//     которого нет ни у одной задачи плана, в любом register-читающем поле:
//     `onchanges:`/`onfail:`/`require:` (списочная форма) ИЛИ CEL-предикат
//     (`when:`/`changed_when:`/`failed_when:`/`until:`/`where:`/`loop.when:`) ИЛИ
//     интерполяция `${ register.<name>.* }` в source-поле (`vars:`/`output:`/
//     `params:`/`apply.input:`/`loop.items:`). Последний класс закрыт ADR-056 S2 —
//     до него unknown-register в интерполяции доживал до рантайм-стратификатора;
//     набор полей здесь обязан совпадать с passage-определяющими источниками
//     стратификатора (см. collectRefs).
//
// Почему ОШИБКА, а не warning, для дубля адреса:
// register-имена резолвятся в task-индексы через плоскую карту имя→index по
// всему плану (keeper/internal/render registerIndex) — last-wins. При двух
// задачах с `register: X` зависимость (`onchanges:[X]`/`onfail:[X]`/`when:
// register.X.*`) тихо привязывается ТОЛЬКО к последней X; изменение/провал
// первой X молча не активирует зависимость. Это немой источник «rescue не
// сработал»/«onchanges пропущен» — линтер обязан ловить его статически, а не
// отдавать на отладку в рантайме. Симметрично `id:` — это адрес задачи для
// подписки на алерты «таска X изменила» (ADR-052 §h); register и id живут в
// ОДНОМ адресном пространстве (destiny/tasks.md §8), поэтому дубль id или
// пересечение register/id сматчили бы подписку «алерт на таску X» не с той
// задачей (или с несколькими). Дубль почти всегда баг (копипаст без
// переименования), легитимного кейса под одно имя у двух задач нет — поэтому
// ошибка, не warning.
//
// Имена обходятся по плоскому пространству всего плана, включая вложенные
// `block:` (рекурсивно), потому что keeper-side registerIndex тоже плоский:
// register с block-задачи адресуется из любой точки плана. Порядок не важен:
// для дубля — потому что дубль есть дубль; для cross-ref — потому что резолв
// идёт по индексам, и unknown-имя = баг независимо от позиции (forward-ref на
// существующее имя — отдельный, легитимный кейс, тут не караем).
//
// Cross-file uniqueness через раскрытый include (дубль адреса между основным
// файлом и подключённым) ловится отдельно — на плоском `[]Task` после
// ExpandIncludes (validateFlatTaskAddresses). Здесь — per-file AST-уровень с
// точными line/col.
//
// Cross-ref для CEL-предикатов (`when:`/`changed_when:`/`failed_when:`/`until:`/
// `where:`/`loop.when:`, где ссылка пишется как `register.<name>.*`) покрыт
// текстовым извлечением: содержимое строковых литералов вырезается (как в
// shared/cel guard-ах), затем regex `register.<name>` собирает имена (см.
// ExtractRegisterRefs). Динамический доступ (`register["..."]`) и `register.self`
// (текущая задача) сознательно не флагаются. Полный CEL-AST-парс не нужен —
// форма ссылки фиксирована грамматикой (`register.<name>`).
//
// tasksSeq — AST-узел `tasks:` (scenario) либо корневой sequence (destiny).
// nil-узел → nil (пустой/невалидный список уже отдиагностирован выше).
func validateTaskRefs(tasksSeq *ast.SequenceNode, pathPrefix string) []diag.Diagnostic {
	if tasksSeq == nil {
		return nil
	}

	// addrs — всё адресное пространство подписки (register ∪ id) для проверки
	// уникальности. registers — только register-имена для cross-ref
	// (unknown_register_reference): id адресует подписку на алерт, но НЕ создаёт
	// `register.<name>` — ссылаться на id-задачу в onchanges/onfail/when нельзя.
	addrs := map[string]bool{}
	registers := map[string]bool{}
	var dupDiags []diag.Diagnostic
	collectAddresses(tasksSeq, pathPrefix, addrs, registers, &dupDiags)

	var out []diag.Diagnostic
	out = append(out, dupDiags...)
	out = append(out, collectRefs(tasksSeq, pathPrefix, registers)...)
	return out
}

// collectAddresses обходит задачи (рекурсивно через block:) и наполняет два
// набора: addrs — всё адресное пространство подписки `register ∪ id`
// (destiny/tasks.md §8); registers — только register-имена (для cross-ref).
// Повторное имя в addrs (дубль register, дубль id, либо пересечение register/id)
// → duplicate_task_address на КАЖДОМ повторном объявлении (первое считаем
// «основным», диагностику на нём не поднимаем — симметрично
// task_discriminator_multiple).
//
// addrs несёт обе грани одного пространства, поэтому register «X» и id «X» на
// разных задачах сталкиваются: подписка «алерт на таску X» не должна молча
// сматчить две разные задачи. registers заполняется только из `register:` —
// id-задача ссылок register.<name> на себя не создаёт.
func collectAddresses(seq *ast.SequenceNode, pathPrefix string, addrs, registers map[string]bool, dups *[]diag.Diagnostic) {
	for i, item := range seq.Values {
		taskPath := fmt.Sprintf("%s[%d]", pathPrefix, i)
		mm, ok := item.(*ast.MappingNode)
		if !ok {
			continue
		}
		for _, kv := range mm.Values {
			tok := kv.Key.GetToken()
			if tok == nil {
				continue
			}
			switch tok.Value {
			case "register", "id":
				sn, isStr := kv.Value.(*ast.StringNode)
				if !isStr || sn.Value == "" {
					continue // type/format уже отдиагностирован validateRegisterField/validateIDField.
				}
				if tok.Value == "register" {
					registers[sn.Value] = true
				}
				if addrs[sn.Value] {
					rt := sn.GetToken()
					*dups = append(*dups, diagAt(rt.Position.Line, rt.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
						Code:     "duplicate_task_address",
						Message:  fmt.Sprintf("task address %q (register/id) is declared more than once in this plan", sn.Value),
						Hint:     "register and id share one subscription address space — a duplicate makes \"alert on task X\" match the wrong task and silently breaks onchanges/onfail/when wiring; rename one",
						YAMLPath: taskPath + "." + tok.Value,
					}))
					continue
				}
				addrs[sn.Value] = true
			case "block":
				if bseq, isSeq := kv.Value.(*ast.SequenceNode); isSeq {
					collectAddresses(bseq, taskPath+".block", addrs, registers, dups)
				}
			}
		}
	}
}

// collectRefs обходит задачи (рекурсивно через block:) и проверяет КАЖДОЕ
// register-читающее поле задачи против known; неизвестное имя →
// unknown_register_reference. Поля бьются на три класса:
//
//   - requisite-списки (onchanges/onfail/require) — имя task пишется как голый
//     элемент списка (checkRefList);
//   - CEL-предикаты (when/changed_when/failed_when/where + вложенные retry.until/
//     loop.when) — ссылка как `register.<name>.*` в выражении (checkPredicateRefs);
//   - интерполяционные поля (vars/output/params/apply.input/loop.items) — ссылка
//     как `${ register.<name>.* }` в строковых литералах, рекурсивно по map/seq
//     (checkInterpRefs).
//
// Этот набор обязан 1:1 совпадать с passage-определяющими источниками
// стратификатора (keeper/internal/render.collectTaskReads, ADR-056-реестр), чтобы
// граф register-ссылок линтера и граф стратификатора не расходились: дыра в
// валидаторе означала бы, что unknown-register в output/vars/params/apply.input
// доживает до рантайма и падает StratifyUnknownRegister онлайн, а не ловится
// офлайн линтером. Guard-инвариант — reads==refs consistency (passage_test.go).
func collectRefs(seq *ast.SequenceNode, pathPrefix string, known map[string]bool) []diag.Diagnostic {
	var out []diag.Diagnostic
	for i, item := range seq.Values {
		taskPath := fmt.Sprintf("%s[%d]", pathPrefix, i)
		mm, ok := item.(*ast.MappingNode)
		if !ok {
			continue
		}
		for _, kv := range mm.Values {
			tok := kv.Key.GetToken()
			if tok == nil {
				continue
			}
			switch tok.Value {
			case "onchanges", "onfail", "require":
				out = append(out, checkRefList(tok.Value, kv.Value, known, taskPath)...)
			case "when", "changed_when", "failed_when", "where":
				out = append(out, checkPredicateRefs(tok.Value, kv.Value, known, taskPath)...)
				out = append(out, checkSoulprintRefs(tok.Value, kv.Value, taskPath)...)
			case "vars", "output", "params":
				// Интерполяционные source-поля: ${ register.X } в строковых
				// литералах, рекурсивно по вложенным map/seq.
				out = append(out, checkInterpRefs(kv.Value, known, taskPath, tok.Value)...)
			case "apply":
				// applier-задача: register читается в apply.input (вложенный map).
				if amm, isMap := kv.Value.(*ast.MappingNode); isMap {
					for _, sub := range amm.Values {
						st := sub.Key.GetToken()
						if st != nil && st.Value == "input" {
							out = append(out, checkInterpRefs(sub.Value, known, taskPath, "apply.input")...)
						}
					}
				}
			case "retry":
				// `until:` — CEL-предикат внутри retry-mapping.
				if rmm, isMap := kv.Value.(*ast.MappingNode); isMap {
					for _, sub := range rmm.Values {
						st := sub.Key.GetToken()
						if st != nil && st.Value == "until" {
							out = append(out, checkPredicateRefs("retry.until", sub.Value, known, taskPath)...)
							out = append(out, checkSoulprintRefs("retry.until", sub.Value, taskPath)...)
						}
					}
				}
			case "loop":
				// `when:` — CEL-предикат; `items:` — интерполяционный source
				// (${ register.X } в скаляре/списке/map).
				if lmm, isMap := kv.Value.(*ast.MappingNode); isMap {
					for _, sub := range lmm.Values {
						st := sub.Key.GetToken()
						if st == nil {
							continue
						}
						switch st.Value {
						case "when":
							out = append(out, checkPredicateRefs("loop.when", sub.Value, known, taskPath)...)
							out = append(out, checkSoulprintRefs("loop.when", sub.Value, taskPath)...)
						case "items":
							out = append(out, checkInterpRefs(sub.Value, known, taskPath, "loop.items")...)
						}
					}
				}
			case "block":
				if bseq, isSeq := kv.Value.(*ast.SequenceNode); isSeq {
					out = append(out, collectRefs(bseq, taskPath+".block", known)...)
				}
			}
		}
	}
	return out
}

// checkInterpRefs рекурсивно обходит AST-узел интерполяционного поля (vars /
// output / params / apply.input / loop.items) и проверяет register-имена,
// встреченные внутри `${ register.<name>.* }` строковых литералов, против known.
// Это закрывает дыру cross-ref-валидатора: до ADR-056 S2 unknown-register в
// интерполяции (а не в предикате/requisite) не ловился офлайн и падал только
// рантайм-стратификатором (StratifyUnknownRegister).
//
// Извлечение делает ExtractRegisterRefs — тот же канон-парсер `register.<name>`,
// что и стратификатор и checkPredicateRefs (без дубля regex). Не-string узлы
// (int/bool/null) ссылок не несут, рекурсия спускается только по map/seq.
// Диагностика — на позиции строкового узла, где найдена ссылка.
func checkInterpRefs(node ast.Node, known map[string]bool, taskPath, kind string) []diag.Diagnostic {
	switch n := node.(type) {
	case *ast.StringNode:
		var out []diag.Diagnostic
		rt := n.GetToken()
		line, col := 0, 0
		if rt != nil {
			line, col = rt.Position.Line, rt.Position.Column
		}
		for _, name := range ExtractRegisterRefs(n.Value) {
			if known[name] {
				continue
			}
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:     "unknown_register_reference",
				Message:  fmt.Sprintf("%s interpolates register %q, which no task declares", kind, name),
				Hint:     "interpolation reads ${ register.<name>.* }; check for a typo or a missing register: on the producing task (register.self is the current task and is not checked)",
				YAMLPath: fmt.Sprintf("%s.%s", taskPath, kind),
			}))
		}
		return out
	case *ast.MappingNode:
		var out []diag.Diagnostic
		for _, kv := range n.Values {
			out = append(out, checkInterpRefs(kv.Value, known, taskPath, kind)...)
		}
		return out
	case *ast.MappingValueNode:
		return checkInterpRefs(n.Value, known, taskPath, kind)
	case *ast.SequenceNode:
		var out []diag.Diagnostic
		for _, v := range n.Values {
			out = append(out, checkInterpRefs(v, known, taskPath, kind)...)
		}
		return out
	default:
		return nil
	}
}

// checkRefList проверяет элементы списочного requisite-поля (onchanges/onfail/
// require) против known. `require:` допускает скалярную форму `"all"` (не
// register-список) — она пропускается. Не-string/CEL-обёрнутые элементы
// пропускаются (имя статически неизвестно). Пустой/невалидный список —
// type-проверки тут нет (зона отдельного validateTaskNode), молча skip.
func checkRefList(kind string, value ast.Node, known map[string]bool, taskPath string) []diag.Diagnostic {
	seq, ok := value.(*ast.SequenceNode)
	if !ok {
		return nil // require: "all" (скаляр) или невалидная форма — не наша зона.
	}
	var out []diag.Diagnostic
	for j, item := range seq.Values {
		sn, isStr := item.(*ast.StringNode)
		if !isStr || isCELWrapped(sn.Value) {
			continue
		}
		if known[sn.Value] {
			continue
		}
		rt := sn.GetToken()
		out = append(out, diagAt(rt.Position.Line, rt.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "unknown_register_reference",
			Message:  fmt.Sprintf("%s[%d] references register %q, which no task declares", kind, j, sn.Value),
			Hint:     "requisite lists name a task by its register: value; check for a typo or a missing register: on the producing task",
			YAMLPath: fmt.Sprintf("%s.%s[%d]", taskPath, kind, j),
		}))
	}
	return out
}

// reRegisterCELRef извлекает `register.<name>` из CEL-текста. Граница слева —
// начало строки ИЛИ символ, не входящий в идентификатор и не `.` (чтобы
// `myregister.x` и `foo.register.y` НЕ матчились: `register` обязан быть
// корневым идентификатором, как в грамматике CEL-контекста). Имя — snake_case
// register-id (совпадает с reRegisterID). Динамический доступ `register["x"]`
// формой не покрывается — точки нет, regex не матчит (безопасный пропуск).
var reRegisterCELRef = regexp.MustCompile(`(^|[^A-Za-z0-9_.])register\.([a-z][a-z0-9_]*)`)

// celStringLiteral — строковый литерал CEL (одинарные/двойные кавычки).
// Зеркалит shared/cel.stringLiteralRe; вырезаем содержимое перед текстовым
// поиском идентификаторов, чтобы `register.x` внутри литерала-данных не давал
// ложное unknown_register_reference.
var celStringLiteral = regexp.MustCompile(`'[^']*'|"[^"]*"`)

// ExtractRegisterRefs возвращает отсортированный набор уникальных имён из
// `register.<name>` в CEL-строке (после вырезания строковых литералов). `self`
// исключается — это текущая задача (register.self.*), её известность не
// проверяется cross-ref-ом. Сортировка делает диагностики детерминированными
// при нескольких unknown-ссылках в одном предикате.
//
// Экспортирована как канонический парсер cross-task register-ссылок: stage-render
// стратификация ([ADR-056], keeper/internal/render) переиспользует её, чтобы
// строить граф register-зависимостей по той же грамматике `register.<name>`, что
// и cross-ref-валидатор, — без дубля regex. `register.self.*` (Soul-side
// собственный результат той же задачи) ссылкой НЕ считается ни здесь, ни в
// стратификации (это не cross-task ребро).
func ExtractRegisterRefs(expr string) []string {
	stripped := celStringLiteral.ReplaceAllString(expr, `""`)
	seen := map[string]struct{}{}
	for _, m := range reRegisterCELRef.FindAllStringSubmatch(stripped, -1) {
		name := m[2]
		if name == "self" {
			continue
		}
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// checkPredicateRefs проверяет register-имена в CEL-предикате (when/changed_when/
// failed_when/until/where/loop.when) против known. bool-литерал (force-shortcut
// changed_when/failed_when) и null — не CEL-строка, пропускаются. Forward-ref на
// существующее имя легитимен (known плоский по всему плану); караем только
// отсутствующее. Диагностика — на позиции value-ноды предиката (точное смещение
// внутри строки не извлекается — это текстовый, не AST-анализ).
func checkPredicateRefs(kind string, value ast.Node, known map[string]bool, taskPath string) []diag.Diagnostic {
	sn, ok := value.(*ast.StringNode)
	if !ok {
		return nil
	}
	var out []diag.Diagnostic
	rt := sn.GetToken()
	line, col := 0, 0
	if rt != nil {
		line, col = rt.Position.Line, rt.Position.Column
	}
	for _, name := range ExtractRegisterRefs(sn.Value) {
		if known[name] {
			continue
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "unknown_register_reference",
			Message:  fmt.Sprintf("%s references register %q, which no task declares", kind, name),
			Hint:     "CEL predicate reads register.<name>.*; check for a typo or a missing register: on the producing task (register.self is the current task and is not checked)",
			YAMLPath: fmt.Sprintf("%s.%s", taskPath, kind),
		}))
	}
	return out
}
