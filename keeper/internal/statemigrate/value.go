package statemigrate

import "strings"

// interpolateValue рекурсивно обходит value-дерево set.value и резолвит
// `${ … }`-CEL-интерполяцию в каждом строковом листе ([docs/migrations.md
// §«Операции transform:» set]). Узкая копия логики render-pipeline
// (keeper/internal/render.renderValue) — ядро statemigrate не тянет зависимость
// от render-пакета, чтобы остаться чистым.
//
// Семантика интерполяции наследует shared/cel: строка-лист с ровно одним
// `${ }`-блоком даёт нативный тип результата (число/map/list), иначе — склейку
// через стрингификацию. Строки без `${ }` проходят как литералы (Eval вернёт
// их без изменений). Non-string листья (числа/bool/nil) — насквозь.
func interpolateValue(v any, ev Evaluator, scope Scope) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			rv, err := interpolateValue(val, ev, scope)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			rv, err := interpolateValue(val, ev, scope)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	case string:
		// Нет маркера — литерал (не гоняем CEL впустую, set чаще пишет
		// статические ACL-строки).
		if !strings.Contains(t, "${") {
			return t, nil
		}
		res, err := ev.Interpolate(t, scope)
		if err != nil {
			return nil, &EvalError{Class: ClassCELInterp, Msg: "set.value: " + t, Err: err}
		}
		return res, nil
	default:
		return v, nil
	}
}
