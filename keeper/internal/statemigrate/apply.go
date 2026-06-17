package statemigrate

import (
	"fmt"
	"strings"
)

// applyOps применяет список операций последовательно к мутируемому state.
// Каждая операция видит state, изменённый предыдущими (та же миграция/тот же
// foreach-do). scope несёт корневой state и активные foreach-переменные.
//
// state — корень incarnation.state (мутируется на месте; deep-copy сделан в
// [Apply] до первой операции, caller-ский map не затрагивается).
func applyOps(ops []Op, state map[string]any, ev Evaluator, loop map[string]any) error {
	scope := Scope{State: state, Loop: loop}
	for i := range ops {
		if err := applyOp(ops[i], state, ev, scope); err != nil {
			return err
		}
	}
	return nil
}

// applyOp диспетчеризует одну операцию по дискриминатору.
func applyOp(op Op, state map[string]any, ev Evaluator, scope Scope) error {
	switch {
	case op.Rename != nil:
		return applyRename(op.Rename, state, ev, scope)
	case op.Set != nil:
		return applySet(op.Set, state, ev, scope)
	case op.Delete != nil:
		return applyDelete(op.Delete, state, ev, scope)
	case op.Foreach != nil:
		return applyForeach(op.Foreach, state, ev, scope)
	default:
		// Дискриминатор гарантирован парсером; защита от программной ошибки.
		return &EvalError{Class: ClassPathSegment, Msg: "пустая операция (нет дискриминатора)"}
	}
}

// applyRename переносит значение from → to (move = тот же code-path). Источник
// отсутствует → ничего не переносим (нечего двигать); существующий to → ошибка
// (явный delete перед rename, [docs/migrations.md]).
func applyRename(op *RenameOp, state map[string]any, ev Evaluator, scope Scope) error {
	fromKeys, err := resolvePath(op.From, ev, scope)
	if err != nil {
		return err
	}
	toKeys, err := resolvePath(op.To, ev, scope)
	if err != nil {
		return err
	}

	val, ok, err := getPath(state, fromKeys)
	if err != nil {
		return err
	}
	if !ok {
		// Источника нет — нечего переносить (no-op, симметрично delete).
		return nil
	}

	if _, exists, err := getPath(state, toKeys); err != nil {
		return err
	} else if exists {
		return &EvalError{Class: ClassRenameToExists, Path: op.To, Msg: "целевой путь rename/move уже существует (нужен явный delete)"}
	}

	if err := setPath(state, toKeys, val); err != nil {
		return err
	}
	return deletePath(state, fromKeys)
}

// applySet записывает value в path (с перезаписью). value рекурсивно
// интерполируется: каждый строковый лист со встроенными `${ … }` резолвится
// через Evaluator (узкая копия логики render-pipeline, ядро остаётся чистым).
func applySet(op *SetOp, state map[string]any, ev Evaluator, scope Scope) error {
	keys, err := resolvePath(op.Path, ev, scope)
	if err != nil {
		return err
	}
	val, err := interpolateValue(op.Value, ev, scope)
	if err != nil {
		return err
	}
	return setPath(state, keys, val)
}

// applyDelete удаляет значение по path. Несуществующий путь → no-op.
func applyDelete(op *DeleteOp, state map[string]any, ev Evaluator, scope Scope) error {
	keys, err := resolvePath(op.Path, ev, scope)
	if err != nil {
		return err
	}
	return deletePath(state, keys)
}

// applyForeach итерирует по результату In (список → элемент, map → значение),
// биндит As и рекурсивно применяет Do с обновлённым scope. Вложенные foreach
// добавляют свои As поверх внешних (новый loop-map на итерацию).
func applyForeach(op *ForeachOp, state map[string]any, ev Evaluator, scope Scope) error {
	if op.As == "" {
		return &EvalError{Class: ClassForeachType, Msg: "foreach без as:"}
	}
	coll, err := evalCollection(op.In, ev, scope)
	if err != nil {
		return &EvalError{Class: ClassCELInterp, Msg: fmt.Sprintf("foreach in: %q", op.In), Err: err}
	}

	items, err := iterItems(coll, op.In)
	if err != nil {
		return err
	}
	for _, item := range items {
		childLoop := make(map[string]any, len(scope.Loop)+1)
		for k, v := range scope.Loop {
			childLoop[k] = v
		}
		childLoop[op.As] = item
		if err := applyOps(op.Do, state, ev, childLoop); err != nil {
			return err
		}
	}
	return nil
}

// evalCollection вычисляет foreach.in. Выражение коллекции в фикстурах несёт
// маркер `${ … }` (docs/migrations.md), поэтому при его наличии резолвим через
// Interpolate (литерал+блок, нативный тип). Без маркера трактуем всю строку как
// голый CEL (Eval) — толерантность к обеим формам записи.
func evalCollection(in string, ev Evaluator, scope Scope) (any, error) {
	if strings.Contains(in, "${") {
		return ev.Interpolate(in, scope)
	}
	return ev.Eval(in, scope)
}

// iterItems раскладывает результат foreach.in в упорядоченный список
// итерируемых значений: список → его элементы (порядок сохранён); map → её
// ЗНАЧЕНИЯ. Прочие типы (скаляр/null) → ошибка ClassForeachType.
//
// Над map порядок значений детерминирован сортировкой ключей (миграция —
// чистая воспроизводимая функция; map-итерация в Go недетерминирована).
func iterItems(coll any, expr string) ([]any, error) {
	switch t := coll.(type) {
	case []any:
		return t, nil
	case map[string]any:
		keys := sortedKeys(t)
		out := make([]any, 0, len(t))
		for _, k := range keys {
			out = append(out, t[k])
		}
		return out, nil
	default:
		return nil, &EvalError{Class: ClassForeachType, Msg: fmt.Sprintf("foreach in: %q дал %T, ожидался список или map", expr, coll)}
	}
}

// resolvePath парсит и резолвит ${ … }-сегменты адреса в плоский список ключей.
func resolvePath(raw string, ev Evaluator, scope Scope) ([]string, error) {
	segs, err := parsePath(raw)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, &EvalError{Class: ClassPathSegment, Path: raw, Msg: "пустой адрес (операции над корнем state не поддерживаются)"}
	}
	return resolveSegments(segs, ev, scope)
}

// getPath навигирует state по keys. Возвращает (значение, найдено, ошибка).
// Промежуточный сегмент не-map (скаляр/список) при ещё не пройденном пути →
// ошибка ClassPathTraverse. Отсутствие ключа → (nil, false, nil).
func getPath(state map[string]any, keys []string) (any, bool, error) {
	cur := state
	for i, k := range keys {
		v, ok := cur[k]
		if !ok {
			return nil, false, nil
		}
		if i == len(keys)-1 {
			return v, true, nil
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil, false, &EvalError{Class: ClassPathTraverse, Path: joinKeys(keys[:i+1]), Msg: fmt.Sprintf("промежуточный сегмент — %T, не map", v)}
		}
		cur = next
	}
	return nil, false, nil
}

// setPath записывает val по keys, создавая промежуточные map при отсутствии.
// Промежуточный существующий не-map → ошибка ClassPathTraverse (не молчаливая
// перезапись чужой структуры).
func setPath(state map[string]any, keys []string, val any) error {
	cur := state
	for i, k := range keys {
		if i == len(keys)-1 {
			cur[k] = val
			return nil
		}
		v, ok := cur[k]
		if !ok {
			next := map[string]any{}
			cur[k] = next
			cur = next
			continue
		}
		next, ok := v.(map[string]any)
		if !ok {
			return &EvalError{Class: ClassPathTraverse, Path: joinKeys(keys[:i+1]), Msg: fmt.Sprintf("промежуточный сегмент — %T, не map", v)}
		}
		cur = next
	}
	return nil
}

// deletePath удаляет значение по keys. Отсутствие любого сегмента → no-op
// (не ошибка, [docs/migrations.md]). Промежуточный не-map → no-op (раз пути
// нет — нечего удалять).
func deletePath(state map[string]any, keys []string) error {
	cur := state
	for i, k := range keys {
		if i == len(keys)-1 {
			delete(cur, k)
			return nil
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil // путь не существует целиком — no-op
		}
		cur = next
	}
	return nil
}
