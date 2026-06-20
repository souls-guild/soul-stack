package trial

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// deepEqualJSON сравнивает две Go-структуры, прошедшие через structpb
// (map[string]any / []any / float64 / string / bool / nil). Прямой
// reflect.DeepEqual достаточен: обе стороны нормализованы одинаковым
// structpb.AsMap (числа → float64, обход map детерминирован сравнением).
func deepEqualJSON(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

// mergeStateChanges — ★ логика идентична scenario.mergeStateChanges (state.go):
// применяет упорядоченный список отрендеренных операций `state_changes` поверх
// базового state (orchestration.md §7). Trial обязан давать тот же state_after,
// что прод-коммит; любое расхождение тел разведёт Trial с продом (анти-дрейф
// дубля, держит Mirror-тест). Семантика — см. scenario.mergeStateChanges.
func mergeStateChanges(stateBefore map[string]any, ops []render.RenderedOp, schema map[string]any, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) (map[string]any, error) {
	out := deepCopyState(stateBefore)
	for i := range ops {
		op := ops[i]
		switch op.Verb {
		case config.VerbSet:
			out[op.Field] = op.Value
		case config.VerbAdd:
			if err := applyAddOp(out, op, schema, matchEval, opEval); err != nil {
				return nil, fmt.Errorf("state_changes[%d] add %q: %w", i, op.Field, err)
			}
		case config.VerbModify:
			if err := applyModifyOp(out, op, opEval); err != nil {
				return nil, fmt.Errorf("state_changes[%d] modify %q: %w", i, op.Field, err)
			}
		case config.VerbRemove:
			if err := applyRemoveOp(out, op, opEval); err != nil {
				return nil, fmt.Errorf("state_changes[%d] remove %q: %w", i, op.Field, err)
			}
		default:
			return nil, fmt.Errorf("state_changes[%d]: verb %q не поддержан движком", i, op.Verb)
		}
	}
	return out, nil
}

// applyModifyOp — ★ логика идентична scenario.applyModifyOp.
func applyModifyOp(out map[string]any, op render.RenderedOp, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	if !present {
		return checkExpect(op, 0)
	}
	switch coll := existing.(type) {
	case map[string]any:
		matched := 0
		for k, v := range coll {
			binds := map[string]any{"key": k, "value": v}
			ok, err := evalOpBool(opEval, op.Match, op.Context, binds)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			matched++
			patched, err := applyPatch(v, op.Patch, op.Context, binds, opEval)
			if err != nil {
				return err
			}
			coll[k] = patched
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		out[op.Field] = coll
		return nil
	case []any:
		matched := 0
		for i := range coll {
			binds := map[string]any{"elem": coll[i]}
			ok, err := evalOpBool(opEval, op.Match, op.Context, binds)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			matched++
			patched, err := applyPatch(coll[i], op.Patch, op.Context, binds, opEval)
			if err != nil {
				return err
			}
			coll[i] = patched
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		out[op.Field] = coll
		return nil
	}
	return fmt.Errorf("поле %q не является коллекцией (map/list)", op.Field)
}

// applyRemoveOp — ★ логика идентична scenario.applyRemoveOp.
func applyRemoveOp(out map[string]any, op render.RenderedOp, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	if !present {
		return checkExpect(op, 0)
	}
	switch coll := existing.(type) {
	case map[string]any:
		matched := 0
		drop := make([]string, 0, len(coll))
		for k, v := range coll {
			ok, err := evalOpBool(opEval, op.Match, op.Context, map[string]any{"key": k, "value": v})
			if err != nil {
				return err
			}
			if ok {
				matched++
				drop = append(drop, k)
			}
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		for _, k := range drop {
			delete(coll, k)
		}
		out[op.Field] = coll
		return nil
	case []any:
		kept := make([]any, 0, len(coll))
		matched := 0
		for i := range coll {
			ok, err := evalOpBool(opEval, op.Match, op.Context, map[string]any{"elem": coll[i]})
			if err != nil {
				return err
			}
			if ok {
				matched++
				continue
			}
			kept = append(kept, coll[i])
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		out[op.Field] = kept
		return nil
	}
	return fmt.Errorf("поле %q не является коллекцией (map/list)", op.Field)
}

// evalOpBool — ★ логика идентична scenario.evalOpBool.
func evalOpBool(opEval render.StateOpEvalFunc, match string, ctx, binds map[string]any) (bool, error) {
	if match == "" {
		return false, nil
	}
	res, err := opEval(match, ctx, binds, true)
	if err != nil {
		return false, fmt.Errorf("match-предикат %q: %w", match, err)
	}
	b, ok := res.(bool)
	if !ok {
		return false, fmt.Errorf("match-предикат %q вернул %T, ожидался bool", match, res)
	}
	return b, nil
}

// applyPatch — ★ логика идентична scenario.applyPatch.
func applyPatch(elem any, patch, ctx, binds map[string]any, opEval render.StateOpEvalFunc) (any, error) {
	target, ok := deepCopyValue(elem).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("patch применим только к объекту-записи (элемент %T не объект)", elem)
	}
	for path, rawVal := range patch {
		val, err := renderPatchValue(rawVal, ctx, binds, opEval)
		if err != nil {
			return nil, fmt.Errorf("patch %q: %w", path, err)
		}
		if err := setNestedPath(target, path, val); err != nil {
			return nil, fmt.Errorf("patch %q: %w", path, err)
		}
	}
	return target, nil
}

// renderPatchValue — ★ логика идентична scenario.renderPatchValue.
func renderPatchValue(raw any, ctx, binds map[string]any, opEval render.StateOpEvalFunc) (any, error) {
	s, ok := raw.(string)
	if !ok {
		return raw, nil
	}
	return opEval(s, ctx, binds, false)
}

// setNestedPath — ★ логика идентична scenario.setNestedPath. Отсутствующий
// промежуточный узел материализуем (ADR-057 §f); существующий не-map узел →
// ошибка (не молчаливый клоббер). Расхождение с прод-веткой разведёт Trial с
// продом — держит Mirror-тест.
func setNestedPath(m map[string]any, path string, val any) error {
	parts := splitPath(path)
	cur := m
	for i := 0; i < len(parts)-1; i++ {
		seg := parts[i]
		existing, present := cur[seg]
		if !present {
			next := map[string]any{}
			cur[seg] = next
			cur = next
			continue
		}
		next, ok := existing.(map[string]any)
		if !ok {
			return fmt.Errorf("промежуточный узел %q уже существует и не является объектом (%T) — patch вложенного пути %q затёр бы его", seg, existing, path)
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = val
	return nil
}

// splitPath — ★ логика идентична scenario.splitPath.
func splitPath(path string) []string {
	return strings.Split(path, ".")
}

// checkExpect — ★ логика идентична scenario.checkExpect.
func checkExpect(op render.RenderedOp, matched int) error {
	switch op.Expect {
	case "", config.ExpectAny:
		return nil
	case config.ExpectOne:
		if matched != 1 {
			return fmt.Errorf("expect: one — match зацепил %d элементов (ожидался ровно один)", matched)
		}
	case config.ExpectAtMostOne:
		if matched > 1 {
			return fmt.Errorf("expect: at_most_one — match зацепил %d элементов (ожидалось ≤1)", matched)
		}
	}
	return nil
}

// deepCopyValue — ★ логика идентична scenario.deepCopyValue.
func deepCopyValue(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// applyAddOp — ★ логика идентична scenario.applyAddOp.
func applyAddOp(out map[string]any, op render.RenderedOp, schema map[string]any, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	kind := collectionKind(existing, present, schema, op.Field)

	switch kind {
	case collKindMap:
		if op.Key == "" {
			return fmt.Errorf("add в map-коллекцию требует key:")
		}
		coll, _ := existing.(map[string]any)
		if coll == nil {
			coll = map[string]any{}
		}
		if _, exists := coll[op.Key]; exists {
			switch op.OnConflict {
			case config.OnConflictError:
				// Без зарезолвленного op.Key в reason (BUG-3, security) — см.
				// scenario.applyAddOp. Печатаем только имя коллекции-поля.
				return fmt.Errorf("add %q: ключ уже существует (on_conflict: error)", op.Field)
			case config.OnConflictReplace:
				coll[op.Key] = op.Value
			default: // skip (default) — идемпотентный no-op
			}
		} else {
			coll[op.Key] = op.Value
		}
		out[op.Field] = coll
		return nil

	case collKindList:
		coll, _ := existing.([]any)
		idx, err := findListMatch(coll, op, matchEval, opEval)
		if err != nil {
			return err
		}
		if idx >= 0 {
			switch op.OnConflict {
			case config.OnConflictError:
				// Без зарезолвленного op.Value в reason (BUG-3, security).
				return fmt.Errorf("add %q: элемент с такой идентичностью уже существует (on_conflict: error)", op.Field)
			case config.OnConflictReplace:
				coll[idx] = op.Value
			default: // skip (default) — идемпотентный no-op
			}
		} else {
			coll = append(coll, op.Value)
		}
		out[op.Field] = coll
		return nil
	}
	return fmt.Errorf("поле %q не является коллекцией (map/list) и тип не выводится из schema", op.Field)
}

// findListMatch — ★ логика идентична scenario.findListMatch.
func findListMatch(coll []any, op render.RenderedOp, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) (int, error) {
	for i := range coll {
		if op.Match != "" {
			var ok bool
			var err error
			if op.Context != nil {
				ok, err = evalOpBool(opEval, op.Match, op.Context, map[string]any{"elem": coll[i], "value": op.Value})
			} else {
				ok, err = matchEval(op.Match, coll[i], op.Value)
			}
			if err != nil {
				return -1, fmt.Errorf("match-предикат %q: %w", op.Match, err)
			}
			if ok {
				return i, nil
			}
			continue
		}
		if reflect.DeepEqual(coll[i], op.Value) {
			return i, nil
		}
	}
	return -1, nil
}

// setOpsProjection проецирует set-операции отрендеренного списка в map
// поле→значение для back-compat ассерта `assert.state_changes` (add-операции в
// проекцию не входят — они проверяются через assert.state_after). Логика
// идентична render.Pipeline.RenderStateChanges-проекции.
func setOpsProjection(ops []render.RenderedOp) map[string]any {
	out := make(map[string]any, len(ops))
	for i := range ops {
		if ops[i].Verb == config.VerbSet {
			out[ops[i].Field] = ops[i].Value
		}
	}
	return out
}

// collKind/collectionKind/schemaFieldType — ★ логика идентична scenario.* (одноимённые).
type collKind int

const (
	collKindUnknown collKind = iota
	collKindList
	collKindMap
)

func collectionKind(existing any, present bool, schema map[string]any, field string) collKind {
	if present {
		switch existing.(type) {
		case []any:
			return collKindList
		case map[string]any:
			return collKindMap
		}
		return collKindUnknown
	}
	switch schemaFieldType(schema, field) {
	case "array":
		return collKindList
	case "object":
		return collKindMap
	}
	return collKindUnknown
}

func schemaFieldType(schema map[string]any, field string) string {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return ""
	}
	fieldSchema, ok := props[field].(map[string]any)
	if !ok {
		return ""
	}
	t, _ := fieldSchema["type"].(string)
	return t
}

// deepCopyState — deep-copy базового state через JSON round-trip (ожидаемый итог
// не должен держать ссылку на fixtures.state). nil/пустой → пустой map. Данные
// JSON-safe (YAML-фикстуры); при сбое marshal — пустая база, расхождение поймает
// сверка. ★ Логика идентична scenario.deepCopyMap (имя локальное во избежание коллизии).
func deepCopyState(m map[string]any) map[string]any {
	out := map[string]any{}
	if len(m) > 0 {
		if b, err := json.Marshal(m); err == nil {
			_ = json.Unmarshal(b, &out)
		}
	}
	return out
}

// hasErrors — есть ли среди диагностик ошибки уровня error.
func hasErrors(ds []diag.Diagnostic) bool {
	return diag.HasErrors(ds)
}

// formatDiags сводит диагностики в одну строку для сообщения об ошибке.
func formatDiags(ds []diag.Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		if d.Level != diag.LevelError {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(d.Code)
		if d.YAMLPath != "" {
			b.WriteString(" @ " + d.YAMLPath)
		}
		b.WriteString(": " + d.Message)
	}
	return b.String()
}
