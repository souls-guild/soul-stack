package main

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

// redactError вырезает секреты из текста ошибки. mongo-driver формирует ошибки
// коннекта/команд, чей текст на некоторых путях (auth-failure, URI-parse) может
// содержать пароль — fail-safe заменяем подстроку каждого секрета на "***".
// Вариадик: пустые секреты — no-op.
func redactError(err error, secrets ...string) string {
	msg := err.Error()
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return msg
}

// --- structpb helpers ---

func stringValue(v *structpb.Value) (string, bool) {
	if v == nil {
		return "", false
	}
	if sv, ok := v.GetKind().(*structpb.Value_StringValue); ok {
		return sv.StringValue, true
	}
	return "", false
}

// stringOrEmpty — строковое значение или "" (для опциональных полей).
func stringOrEmpty(v *structpb.Value) string {
	s, _ := stringValue(v)
	return s
}

func boolOrDefault(v *structpb.Value, def bool) bool {
	if v == nil {
		return def
	}
	if bv, ok := v.GetKind().(*structpb.Value_BoolValue); ok {
		return bv.BoolValue
	}
	return def
}

func intOrDefault(v *structpb.Value, def int) int {
	if v == nil {
		return def
	}
	if nv, ok := v.GetKind().(*structpb.Value_NumberValue); ok {
		return int(nv.NumberValue)
	}
	return def
}

// structField — вложенный struct как map полей ({role, db} у элемента roles).
// nil / не-struct → nil (caller трактует как «не задано»).
func structField(v *structpb.Value) map[string]*structpb.Value {
	if v == nil {
		return nil
	}
	sv, ok := v.GetKind().(*structpb.Value_StructValue)
	if !ok {
		return nil
	}
	return sv.StructValue.GetFields()
}

// listField — элементы list-значения ({role, db}-массив roles). nil / не-list → nil.
func listField(v *structpb.Value) []*structpb.Value {
	if v == nil {
		return nil
	}
	lv, ok := v.GetKind().(*structpb.Value_ListValue)
	if !ok {
		return nil
	}
	return lv.ListValue.GetValues()
}

// valueToNative приводит structpb.Value к нативному Go-значению для передачи в
// bson-документ raw-команды (command-state). Числа без дробной части — int64
// (mongo различает int/double: значение 1.0 из YAML не должно стать double там,
// где ждётся int). Вложенные struct/list рекурсивно.
func valueToNative(v *structpb.Value) any {
	if v == nil {
		return nil
	}
	switch k := v.GetKind().(type) {
	case *structpb.Value_NullValue:
		return nil
	case *structpb.Value_StringValue:
		return k.StringValue
	case *structpb.Value_NumberValue:
		if k.NumberValue == float64(int64(k.NumberValue)) {
			return int64(k.NumberValue)
		}
		return k.NumberValue
	case *structpb.Value_BoolValue:
		return k.BoolValue
	case *structpb.Value_StructValue:
		out := make(map[string]any, len(k.StructValue.GetFields()))
		for kk, vv := range k.StructValue.GetFields() {
			out[kk] = valueToNative(vv)
		}
		return out
	case *structpb.Value_ListValue:
		vals := k.ListValue.GetValues()
		out := make([]any, 0, len(vals))
		for _, e := range vals {
			out = append(out, valueToNative(e))
		}
		return out
	default:
		return nil
	}
}

// validateAddr — общая проверка непустого params.addr.
func validateAddr(f map[string]*structpb.Value) []string {
	if s, _ := stringValue(f["addr"]); strings.TrimSpace(s) == "" {
		return []string{"params.addr: must be a non-empty string"}
	}
	return nil
}

// requireString — вспомогалка для Validate: поле обязано быть непустой строкой.
func requireString(f map[string]*structpb.Value, key string) []string {
	if s, _ := stringValue(f[key]); strings.TrimSpace(s) == "" {
		return []string{fmt.Sprintf("params.%s: must be a non-empty string", key)}
	}
	return nil
}
