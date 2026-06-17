package util

import (
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// StringParam извлекает обязательный строковый параметр из ApplyRequest.Params.
// Возвращает ошибку с указанием имени параметра — она проксируется наружу как
// failed-event с понятным message.
func StringParam(params *structpb.Struct, key string) (string, error) {
	v, err := lookup(params, key)
	if err != nil {
		return "", err
	}
	s, ok := v.Kind.(*structpb.Value_StringValue)
	if !ok {
		return "", fmt.Errorf("param %q: expected string, got %T", key, v.Kind)
	}
	return s.StringValue, nil
}

// OptStringParam — то же, но опциональный. Возвращает "", nil если ключа нет.
func OptStringParam(params *structpb.Struct, key string) (string, error) {
	if params == nil || params.Fields == nil {
		return "", nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return "", nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return "", nil
	}
	s, ok := v.Kind.(*structpb.Value_StringValue)
	if !ok {
		return "", fmt.Errorf("param %q: expected string, got %T", key, v.Kind)
	}
	return s.StringValue, nil
}

// OptIntParam — опциональный integer (для uid/gid). proto json маршалит числа
// как float64, поэтому принимаем NumberValue и проверяем целостность.
func OptIntParam(params *structpb.Struct, key string) (int64, bool, error) {
	if params == nil || params.Fields == nil {
		return 0, false, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return 0, false, nil
	}
	n, ok := v.Kind.(*structpb.Value_NumberValue)
	if !ok {
		return 0, false, fmt.Errorf("param %q: expected integer, got %T", key, v.Kind)
	}
	f := n.NumberValue
	if f != float64(int64(f)) {
		return 0, false, fmt.Errorf("param %q: expected integer, got %v", key, f)
	}
	return int64(f), true, nil
}

// OptBoolParam — опциональный bool (для флагов вида core.line.create).
// Отсутствие ключа или null → false, nil. Non-bool значение → ошибка.
func OptBoolParam(params *structpb.Struct, key string) (bool, error) {
	if params == nil || params.Fields == nil {
		return false, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return false, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return false, nil
	}
	b, ok := v.Kind.(*structpb.Value_BoolValue)
	if !ok {
		return false, fmt.Errorf("param %q: expected bool, got %T", key, v.Kind)
	}
	return b.BoolValue, nil
}

// TriBoolParam — опциональный bool с различением «ключ отсутствует» от «false».
// Симметрично OptIntParam: возвращает (value, present, error). Нужен там, где
// три исхода семантически различны — например param `enabled` у
// core.service.running: опущено = не управлять enabled, true = enable,
// false = disable. Голый OptBoolParam для этого не годится (даёт false и на
// отсутствие, и на явный false).
func TriBoolParam(params *structpb.Struct, key string) (value bool, present bool, err error) {
	if params == nil || params.Fields == nil {
		return false, false, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return false, false, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return false, false, nil
	}
	b, ok := v.Kind.(*structpb.Value_BoolValue)
	if !ok {
		return false, false, fmt.Errorf("param %q: expected bool, got %T", key, v.Kind)
	}
	return b.BoolValue, true, nil
}

// OptStringMapParam — опциональный map string→string (для core.exec.env).
// Все значения map обязаны быть строками; non-string значение → ошибка.
func OptStringMapParam(params *structpb.Struct, key string) (map[string]string, error) {
	if params == nil || params.Fields == nil {
		return nil, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return nil, nil
	}
	sv, ok := v.Kind.(*structpb.Value_StructValue)
	if !ok {
		return nil, fmt.Errorf("param %q: expected map of strings, got %T", key, v.Kind)
	}
	out := make(map[string]string, len(sv.StructValue.Fields))
	for k, item := range sv.StructValue.Fields {
		s, ok := item.Kind.(*structpb.Value_StringValue)
		if !ok {
			return nil, fmt.Errorf("param %q[%q]: expected string, got %T", key, k, item.Kind)
		}
		out[k] = s.StringValue
	}
	return out, nil
}

// OptStructMapParam — опциональный произвольный map (для core.file.rendered
// vars). В отличие от OptStringMapParam значения не обязаны быть строками:
// vars приходят уже CEL-rendered и могут содержать числа/булевы/вложенные
// объекты/списки, которые text/template обрабатывает нативно. Возврат —
// structpb.AsMap() (рекурсивная конвертация в map[string]any). Отсутствие
// ключа или null → nil, nil без ошибки.
func OptStructMapParam(params *structpb.Struct, key string) (map[string]any, error) {
	if params == nil || params.Fields == nil {
		return nil, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return nil, nil
	}
	sv, ok := v.Kind.(*structpb.Value_StructValue)
	if !ok {
		return nil, fmt.Errorf("param %q: expected object, got %T", key, v.Kind)
	}
	return sv.StructValue.AsMap(), nil
}

// OptStringSliceParam — опциональный список строк (для core.user.groups).
func OptStringSliceParam(params *structpb.Struct, key string) ([]string, error) {
	if params == nil || params.Fields == nil {
		return nil, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return nil, nil
	}
	lv, ok := v.Kind.(*structpb.Value_ListValue)
	if !ok {
		return nil, fmt.Errorf("param %q: expected list of strings, got %T", key, v.Kind)
	}
	out := make([]string, 0, len(lv.ListValue.Values))
	for i, item := range lv.ListValue.Values {
		s, ok := item.Kind.(*structpb.Value_StringValue)
		if !ok {
			return nil, fmt.Errorf("param %q[%d]: expected string, got %T", key, i, item.Kind)
		}
		out = append(out, s.StringValue)
	}
	return out, nil
}

// OptIntSliceParam — опциональный список integer-ов (для core.http.probe
// status_codes). proto json маршалит числа как float64, поэтому каждый элемент
// проверяется на целостность (как в OptIntParam). Отсутствие ключа или null →
// nil, nil.
func OptIntSliceParam(params *structpb.Struct, key string) ([]int64, error) {
	if params == nil || params.Fields == nil {
		return nil, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return nil, nil
	}
	lv, ok := v.Kind.(*structpb.Value_ListValue)
	if !ok {
		return nil, fmt.Errorf("param %q: expected list of integers, got %T", key, v.Kind)
	}
	out := make([]int64, 0, len(lv.ListValue.Values))
	for i, item := range lv.ListValue.Values {
		n, ok := item.Kind.(*structpb.Value_NumberValue)
		if !ok {
			return nil, fmt.Errorf("param %q[%d]: expected integer, got %T", key, i, item.Kind)
		}
		f := n.NumberValue
		if f != float64(int64(f)) {
			return nil, fmt.Errorf("param %q[%d]: expected integer, got %v", key, i, f)
		}
		out = append(out, int64(f))
	}
	return out, nil
}

func lookup(params *structpb.Struct, key string) (*structpb.Value, error) {
	if params == nil || params.Fields == nil {
		return nil, fmt.Errorf("param %q: missing (params is empty)", key)
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, fmt.Errorf("param %q: missing", key)
	}
	return v, nil
}
