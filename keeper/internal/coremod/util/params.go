// Package util — общие helper-ы для keeper-side core-модулей
// (`core.soul.registered`, `core.cloud.provisioned`, `core.vault.kv-read`).
//
// Содержимое — типизированные accessor-ы над `*structpb.Struct` (params
// ApplyRequest-а) и helper-ы отправки финальных ApplyEvent-ов на gRPC-stream.
//
// Аналог Soul-side `soul/internal/coremod/util/`: имена и сигнатуры симметричны,
// чтобы автор keeper-side модуля писал код тем же способом, что и Soul-side.
// Cross-import между сторонами запрещён (Soul-изоляция — ADR-011), поэтому
// helper-ы дублируются.
package util

import (
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// StringParam — обязательный строковый параметр. Возвращает ошибку с именем
// поля (она проксируется наружу как `failed`-event с понятным message).
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

// OptStringParam — опциональный string. "", nil если ключа нет или null.
func OptStringParam(params *structpb.Struct, key string) (string, error) {
	v, ok := optLookup(params, key)
	if !ok {
		return "", nil
	}
	s, ok := v.Kind.(*structpb.Value_StringValue)
	if !ok {
		return "", fmt.Errorf("param %q: expected string, got %T", key, v.Kind)
	}
	return s.StringValue, nil
}

// StringSliceParam — обязательный список строк. Используется
// `core.soul.registered` для `params.coven` (min_items=1).
func StringSliceParam(params *structpb.Struct, key string) ([]string, error) {
	v, err := lookup(params, key)
	if err != nil {
		return nil, err
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

// StringOrSliceParam — обязательный параметр, принимающий строку ИЛИ список
// строк, нормализованный в []string. Используется `core.soul.registered` для
// `params.sid` (ADR-061): одиночный SID-строкой остаётся валиден (обратная
// совместимость), список — регистрация+ожидание N хостов одним шагом-барьером.
// Пустой список / пустые элементы — ошибка (каждый SID должен быть непустым).
func StringOrSliceParam(params *structpb.Struct, key string) ([]string, error) {
	v, err := lookup(params, key)
	if err != nil {
		return nil, err
	}
	switch k := v.Kind.(type) {
	case *structpb.Value_StringValue:
		if k.StringValue == "" {
			return nil, fmt.Errorf("param %q: empty string", key)
		}
		return []string{k.StringValue}, nil
	case *structpb.Value_ListValue:
		vals := k.ListValue.Values
		if len(vals) == 0 {
			return nil, fmt.Errorf("param %q: empty list", key)
		}
		out := make([]string, 0, len(vals))
		for i, item := range vals {
			s, ok := item.Kind.(*structpb.Value_StringValue)
			if !ok {
				return nil, fmt.Errorf("param %q[%d]: expected string, got %T", key, i, item.Kind)
			}
			if s.StringValue == "" {
				return nil, fmt.Errorf("param %q[%d]: empty string", key, i)
			}
			out = append(out, s.StringValue)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("param %q: expected string or list of strings, got %T", key, v.Kind)
	}
}

// OptStringSliceParam — опциональный список строк (для cloud `fields`,
// `vm_ids` и т.п.). nil если ключа нет.
func OptStringSliceParam(params *structpb.Struct, key string) ([]string, error) {
	v, ok := optLookup(params, key)
	if !ok {
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

// OptBoolParam — опциональный bool. (false,false,nil) если ключа нет/null.
// Второй bool — флаг наличия (нужен, чтобы отличить default от явного false).
func OptBoolParam(params *structpb.Struct, key string) (bool, bool, error) {
	v, ok := optLookup(params, key)
	if !ok {
		return false, false, nil
	}
	b, ok := v.Kind.(*structpb.Value_BoolValue)
	if !ok {
		return false, false, fmt.Errorf("param %q: expected bool, got %T", key, v.Kind)
	}
	return b.BoolValue, true, nil
}

// OptIntParam — опциональный integer (proto json маршалит как float64).
func OptIntParam(params *structpb.Struct, key string) (int64, bool, error) {
	v, ok := optLookup(params, key)
	if !ok {
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

// OptStructParam — опциональный объект (для `profile` cloud-провайдера).
// Возвращает nil если ключа нет; ошибка только при wrong-type.
func OptStructParam(params *structpb.Struct, key string) (*structpb.Struct, error) {
	v, ok := optLookup(params, key)
	if !ok {
		return nil, nil
	}
	sv, ok := v.Kind.(*structpb.Value_StructValue)
	if !ok {
		return nil, fmt.Errorf("param %q: expected object, got %T", key, v.Kind)
	}
	return sv.StructValue, nil
}

func lookup(params *structpb.Struct, key string) (*structpb.Value, error) {
	if params == nil || params.Fields == nil {
		return nil, fmt.Errorf("param %q: missing (params is empty)", key)
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, fmt.Errorf("param %q: missing", key)
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return nil, fmt.Errorf("param %q: missing (null)", key)
	}
	return v, nil
}

func optLookup(params *structpb.Struct, key string) (*structpb.Value, bool) {
	if params == nil || params.Fields == nil {
		return nil, false
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, false
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return nil, false
	}
	return v, true
}
